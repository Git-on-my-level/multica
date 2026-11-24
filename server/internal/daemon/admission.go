package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	admissionCommandTimeout = 5 * time.Second
	admissionScanTimeout    = 2 * admissionCommandTimeout
	admissionBatchPath      = "/api/daemon/tasks/admission"
	admissionMatchTTL       = 3 * time.Minute
)

type admissionClient interface {
	match(context.Context, string) (string, error)
	acquire(context.Context, string, string, int) (bool, error)
	release(context.Context, string, string) error
	observe(context.Context, string, int64, int64) error
}

type agentctlAdmission struct{ path string }

func (d *Daemon) startAdmission() {
	if !d.cfg.Admission.Enabled {
		return
	}
	path := strings.TrimSpace(d.cfg.Admission.AgentctlPath)
	if path == "" {
		path = "agentctl"
	}
	resolved, err := exec.LookPath(path)
	if err == nil {
		path = resolved
	}
	d.admission = newAdmissionController(agentctlAdmission{path: path}, d.logger)
	d.logger.Info("OpenCode admission enabled", "agentctl_path", path, "agentctl_resolved", err == nil)
}

func (a agentctlAdmission) call(ctx context.Context, result any, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, admissionCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.path, append([]string{"admission"}, args...)...)
	cmd.WaitDelay = 250 * time.Millisecond
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(out, &envelope) != nil || !envelope.OK {
		return errors.New("agentctl returned an unsuccessful admission response")
	}
	if result != nil && (len(envelope.Result) == 0 || json.Unmarshal(envelope.Result, result) != nil) {
		return errors.New("agentctl returned an invalid admission result")
	}
	return nil
}

func (a agentctlAdmission) match(ctx context.Context, model string) (string, error) {
	var result struct {
		Bucket  string `json:"bucket"`
		Enabled *bool  `json:"enabled"`
	}
	err := a.call(ctx, &result, "match", "--harness", "opencode", "--model", model)
	if result.Enabled != nil && !*result.Enabled {
		return "", err
	}
	return result.Bucket, err
}

func (a agentctlAdmission) acquire(ctx context.Context, bucket, holder string, pid int) (bool, error) {
	var result struct {
		Granted *bool `json:"granted"`
	}
	err := a.call(ctx, &result, "acquire", "--bucket", bucket, "--holder", holder,
		"--priority", "background", "--no-wait", "--pid", strconv.Itoa(pid))
	if err != nil {
		return false, err
	}
	if result.Granted == nil {
		return false, errors.New("agentctl admission result has no grant decision")
	}
	return *result.Granted, nil
}

func (a agentctlAdmission) release(ctx context.Context, bucket, holder string) error {
	return a.call(ctx, nil, "release", "--bucket", bucket, "--holder", holder)
}

func (a agentctlAdmission) observe(ctx context.Context, bucket string, attempts, throttled int64) error {
	return a.call(ctx, nil, "observe", "--bucket", bucket, "--attempts", strconv.FormatInt(attempts, 10),
		"--throttled", strconv.FormatInt(throttled, 10))
}

type admissionController struct {
	client admissionClient
	logger *slog.Logger
	warn   sync.Once

	// The batch poller is the sole reader and writer of the match cache.
	matches         map[string]admissionMatch
	scanTimeout     time.Duration
	pendingReleases sync.Map // holder -> bucket, for commands that could not release immediately
}

type admissionMatch struct {
	bucket    string
	expiresAt time.Time
}

func newAdmissionController(client admissionClient, logger *slog.Logger) *admissionController {
	return &admissionController{client: client, logger: logger, scanTimeout: admissionScanTimeout, matches: make(map[string]admissionMatch)}
}

func (a *admissionController) warnOnce(err error) {
	a.warn.Do(func() {
		a.logger.Warn("OpenCode admission unavailable", "error", err)
	})
}

// reserve returns false only for a definite denial. The claim holder is scoped
// to one daemon-owned claim attempt and lives until that run has stopped.
func (a *admissionController) reserve(ctx context.Context, runtimeID, model string) (*admissionLease, bool, error) {
	model, _, _ = strings.Cut(model, "#")
	now := time.Now()
	if model == "" {
		return nil, true, nil
	}
	match, cached := a.matches[model]
	if !cached || !now.Before(match.expiresAt) {
		bucket, err := a.client.match(ctx, model)
		if err != nil {
			a.warnOnce(err)
			return nil, true, err
		}
		for key, entry := range a.matches {
			if !now.Before(entry.expiresAt) {
				delete(a.matches, key)
			}
		}
		match = admissionMatch{bucket: bucket, expiresAt: now.Add(admissionMatchTTL)}
		a.matches[model] = match
	}
	if match.bucket == "" {
		return nil, true, nil
	}

	lease := &admissionLease{controller: a, bucket: match.bucket, holder: "multica:" + runtimeID + ":" + uuid.NewString()}
	granted, err := a.client.acquire(ctx, lease.bucket, lease.holder, os.Getpid())
	if err != nil {
		a.warnOnce(err)
		// A lost reply can hide a committed grant. Release this exact holder
		// even though the claim may proceed under the normal concurrency caps.
		lease.releaseWithContext(ctx)
		return nil, true, err
	}
	if !granted {
		a.logger.Info("OpenCode admission denied", "bucket", lease.bucket, "holder", lease.holder)
		return nil, false, nil
	}
	return lease, true, nil
}

// Retry one failed release per cycle. A transport failure must not strand a
// slot for the remaining lifetime of this daemon.
func (a *admissionController) retryRelease(ctx context.Context) {
	a.pendingReleases.Range(func(key, value any) bool {
		holder, bucket := key.(string), value.(string)
		if err := a.client.release(ctx, bucket, holder); err == nil {
			a.pendingReleases.Delete(holder)
		}
		return false
	})
}

type admissionLease struct {
	controller  *admissionController
	bucket      string
	holder      string
	releaseOnce sync.Once
	processMu   sync.Mutex
	processes   int
	taskEnded   bool
	steps       atomic.Int64
	throttled   atomic.Int64
}

func (l *admissionLease) countAttempt(throttled bool) {
	if throttled {
		l.throttled.Add(1)
	} else {
		l.steps.Add(1)
	}
}

// processStarted registers an OpenCode subprocess before Execute returns. The
// drain loop can time out before that subprocess has actually been reaped.
func (l *admissionLease) processStarted() func() {
	l.processMu.Lock()
	l.processes++
	l.processMu.Unlock()
	return func() {
		l.processMu.Lock()
		l.processes--
		ready := l.taskEnded && l.processes == 0
		l.processMu.Unlock()
		if ready {
			l.settle(context.Background())
		}
	}
}

func (l *admissionLease) release() {
	l.releaseWithContext(context.Background())
}

func (l *admissionLease) releaseWithContext(ctx context.Context) {
	if l == nil {
		return
	}
	l.processMu.Lock()
	l.taskEnded = true
	ready := l.processes == 0
	l.processMu.Unlock()
	if ready {
		l.settle(ctx)
	}
}

func (l *admissionLease) settle(ctx context.Context) {
	l.releaseOnce.Do(func() {
		a := l.controller
		if err := a.client.release(ctx, l.bucket, l.holder); err != nil {
			a.warnOnce(err)
			a.pendingReleases.Store(l.holder, l.bucket)
		}
		throttled := l.throttled.Load()
		if attempts := l.steps.Load() + throttled; attempts > 0 {
			if err := a.client.observe(ctx, l.bucket, attempts, throttled); err != nil {
				a.warnOnce(err)
			}
		}
	})
}

func (c *Client) previewOpenCodeAdmission(ctx context.Context, daemonID string, runtimeIDs []string) ([]protocol.BatchAdmissionCandidate, error) {
	ctx, cancel := context.WithTimeout(ctx, admissionCommandTimeout)
	defer cancel()
	var resp struct {
		Candidates *[]protocol.BatchAdmissionCandidate `json:"admission_candidates"`
	}
	if err := c.postJSON(ctx, admissionBatchPath, claimTasksBody(daemonID, runtimeIDs, 0), &resp); err != nil {
		return nil, err
	}
	if resp.Candidates == nil {
		return nil, errors.New("server did not return an admission preview")
	}
	return *resp.Candidates, nil
}

func (c *Client) claimAdmittedBatch(ctx context.Context, daemonID string, runtimeIDs []string, slots int, forceRecheck []string, permits []protocol.BatchAdmissionCandidate) (claimTasksResult, error) {
	ctx, cancel := context.WithTimeout(ctx, batchClaimRequestTimeout)
	defer cancel()
	body := claimTasksBody(daemonID, runtimeIDs, slots, forceRecheck...)
	body["admission_candidates"] = permits
	var result claimTasksResult
	if err := c.postJSON(ctx, admissionBatchPath, body, &result); err != nil {
		return claimTasksResult{}, err
	}
	return result, nil
}

// claimTasksWithAdmission stages permits before one machine-wide batch claim.
// The server schedules ordinary work and permitted OpenCode work together,
// including stale-dispatch recovery, in its normal priority/FIFO order.
func (d *Daemon) claimTasksWithAdmission(ctx context.Context, runtimeIDs []string, slots int, forceRecheck []string) (claimTasksResult, error) {
	ordinaryClaim := func() (claimTasksResult, error) {
		return d.claimTasksWSFirst(ctx, d.cfg.DaemonID, runtimeIDs, slots, forceRecheck...)
	}
	if d.admission == nil {
		return ordinaryClaim()
	}
	hasOpenCode := false
	for _, id := range runtimeIDs {
		if runtime := d.findRuntime(id); runtime != nil && runtime.Provider == "opencode" {
			hasOpenCode = true
			break
		}
	}
	if !hasOpenCode || slots <= 0 {
		return ordinaryClaim()
	}
	if retryAfter := d.wsClaimHTTPFallbackAfter.Load(); retryAfter > time.Now().UnixNano() {
		// A previous fail-open batch over WS may already have committed.
		return claimTasksResult{ForceRecheckedRuntimeIDs: ackNone()}, nil
	}

	scanCtx, cancelScan := context.WithTimeout(ctx, d.admission.scanTimeout)
	defer cancelScan()
	d.admission.retryRelease(scanCtx)
	candidates, err := d.client.previewOpenCodeAdmission(scanCtx, d.cfg.DaemonID, runtimeIDs)
	if err != nil && scanCtx.Err() == nil {
		d.admission.warnOnce(err)
		return ordinaryClaim()
	}

	type candidateKey struct{ runtimeID, agentID string }
	leases := make(map[candidateKey]*admissionLease)
	defer func() {
		// Release losing permits without giving each command a fresh budget.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), admissionCommandTimeout)
		defer cancel()
		for _, lease := range leases {
			lease.releaseWithContext(releaseCtx)
		}
	}()

	var permits []protocol.BatchAdmissionCandidate
	checked, denied := false, false
	for _, candidate := range candidates {
		if scanCtx.Err() != nil {
			break
		}
		// The batch can return at most `slots` tasks; a permit beyond that would
		// hold shared provider capacity that this claim can never use.
		if len(permits) >= slots {
			break
		}
		model := candidate.Model
		if model == "" {
			model = d.cfg.Agents["opencode"].Model
		}
		lease, allowed, err := d.admission.reserve(scanCtx, candidate.RuntimeID, model)
		if err != nil {
			if !checked && scanCtx.Err() == nil {
				return ordinaryClaim()
			}
			break
		}
		checked = true
		if !allowed {
			denied = true
			continue
		}
		permits = append(permits, candidate)
		key := candidateKey{candidate.RuntimeID, candidate.AgentID}
		leases[key] = lease
	}
	cancelScan()

	// The server gates every OpenCode runtime in this batch, including those
	// with no permits. An interrupted scan can still dispatch ordinary work
	// and earlier grants, while absent candidates stay queued.
	result, err := d.client.claimAdmittedBatch(ctx, d.cfg.DaemonID, runtimeIDs, slots, forceRecheck, permits)
	if err != nil {
		return claimTasksResult{}, err
	}
	for _, task := range result.Tasks {
		if task == nil {
			continue
		}
		key := candidateKey{task.RuntimeID, task.AgentID}
		task.admission = leases[key]
		delete(leases, key)
	}
	result.AdmissionDenied = denied
	result.ClaimedWithAdmission = true
	return result, nil
}
