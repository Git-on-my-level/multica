package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
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
	admissionMatchTTL       = 3 * time.Minute
)

type admissionClient interface {
	match(context.Context, string) (string, error)
	acquire(context.Context, string, string, int) (bool, error)
	release(context.Context, string, string) error
	observe(context.Context, string, int64, int64) error
}

type agentctlAdmission struct{ path string }

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
	retryAt         time.Time
	pendingReleases sync.Map // holder -> bucket, for commands that could not release immediately
}

type admissionMatch struct {
	bucket    string
	expiresAt time.Time
}

func newAdmissionController(client admissionClient, logger *slog.Logger) *admissionController {
	return &admissionController{client: client, logger: logger, matches: make(map[string]admissionMatch)}
}

func (a *admissionController) warnOnce(err error) {
	a.warn.Do(func() {
		a.logger.Warn("OpenCode admission unavailable, using daemon concurrency limits", "error", err)
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
			a.retryAt = now.Add(DefaultPollInterval)
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
		a.retryAt = now.Add(DefaultPollInterval)
		// A lost reply can hide a committed grant. Release this exact holder
		// even though the claim may proceed under the normal concurrency caps.
		lease.release()
		return nil, true, err
	}
	if !granted {
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
			l.settle()
		}
	}
}

func (l *admissionLease) release() {
	if l == nil {
		return
	}
	l.processMu.Lock()
	l.taskEnded = true
	ready := l.processes == 0
	l.processMu.Unlock()
	if ready {
		l.settle()
	}
}

func (l *admissionLease) settle() {
	l.releaseOnce.Do(func() {
		// A cancelled run still owns a live release operation.
		ctx := context.Background()
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

func (c *Client) openCodeAdmissionCandidates(ctx context.Context, runtimeID string) ([]protocol.AdmissionCandidate, error) {
	ctx, cancel := context.WithTimeout(ctx, admissionCommandTimeout)
	defer cancel()
	var candidates []protocol.AdmissionCandidate
	err := c.getJSON(ctx, openCodeAdmissionPath(runtimeID), &candidates)
	return candidates, err
}

func (c *Client) claimOpenCodeAdmission(ctx context.Context, runtimeID string, candidate protocol.AdmissionCandidate) (*Task, error) {
	ctx, cancel := context.WithTimeout(ctx, batchClaimRequestTimeout)
	defer cancel()
	var resp struct {
		Task *Task `json:"task"`
	}
	if err := c.postJSON(ctx, openCodeAdmissionPath(runtimeID), candidate, &resp); err != nil {
		return nil, err
	}
	return resp.Task, nil
}

func openCodeAdmissionPath(runtimeID string) string {
	return fmt.Sprintf("/api/daemon/runtimes/%s/tasks/admission", url.PathEscape(runtimeID))
}

// claimOpenCodeRuntime sends a mutating request only for an agent whose model
// has already passed admission. Failure to preview is safe to fall back from;
// failure of the claim itself may have committed and must never be replayed
// through the legacy path.
func (d *Daemon) claimOpenCodeRuntime(ctx context.Context, runtimeID string) (task *Task, previewOK bool, err error) {
	candidates, err := d.client.openCodeAdmissionCandidates(ctx, runtimeID)
	if err != nil {
		return nil, false, err
	}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, true, err
		}
		model := candidate.Model
		if model == "" {
			model = d.cfg.Agents["opencode"].Model
		}
		lease, allowed, err := d.admission.reserve(ctx, runtimeID, model)
		if err != nil {
			return nil, false, err
		}
		if !allowed {
			continue
		}
		task, err := d.client.claimOpenCodeAdmission(ctx, runtimeID, candidate)
		if task == nil || err != nil {
			lease.release()
			if err != nil {
				return nil, true, err
			}
			continue
		}
		task.admission = lease
		return task, true, nil
	}
	return nil, true, nil
}

// claimTasksWithAdmission keeps opt-out and non-OpenCode runtimes on the
// normal machine-wide path. An opted-in OpenCode runtime can fill one local
// slot per pass; each successful claim nudges the poller to fill more slots.
func (d *Daemon) claimTasksWithAdmission(ctx context.Context, runtimeIDs []string, slots int, forceRecheck []string) (claimTasksResult, error) {
	if d.admission == nil {
		return d.claimTasksWSFirst(ctx, d.cfg.DaemonID, runtimeIDs, slots, forceRecheck...)
	}
	regular := make([]string, 0, len(runtimeIDs))
	var admitted []*Task
	var polled bool
	var openCodeErr error
	previewed := make(map[string]struct{})
	// Bound the extra work in one cycle, even when the host serves many
	// workspaces or several candidates have raced another claimant.
	admissionCtx, cancel := context.WithTimeout(ctx, 2*admissionCommandTimeout)
	defer cancel()
	d.admission.retryRelease(admissionCtx)
	for _, runtimeID := range runtimeIDs {
		runtime := d.findRuntime(runtimeID)
		if runtime == nil || runtime.Provider != "opencode" || time.Now().Before(d.admission.retryAt) {
			regular = append(regular, runtimeID)
			continue
		}
		polled = true
		if len(admitted) >= slots || admissionCtx.Err() != nil {
			continue
		}
		task, previewOK, err := d.claimOpenCodeRuntime(admissionCtx, runtimeID)
		if !previewOK {
			if ctx.Err() == nil {
				d.admission.warnOnce(err)
				regular = append(regular, runtimeID)
			}
			continue
		}
		if err != nil {
			openCodeErr = err
			continue
		}
		previewed[runtimeID] = struct{}{}
		if task != nil {
			admitted = append(admitted, task)
		}
	}

	result := claimTasksResult{ForceRecheckedRuntimeIDs: ackNone()}
	if len(regular) > 0 && len(admitted) < slots && ctx.Err() == nil {
		var regularForce []string
		for _, id := range forceRecheck {
			for _, runtimeID := range regular {
				if id == runtimeID {
					regularForce = append(regularForce, id)
					break
				}
			}
		}
		var err error
		result, err = d.claimTasksWSFirst(ctx, d.cfg.DaemonID, regular, slots-len(admitted), regularForce...)
		if err != nil {
			if len(admitted) == 0 {
				return claimTasksResult{}, err
			}
			d.logger.Warn("ordinary batch claim failed after an OpenCode claim", "error", err)
			result = claimTasksResult{ForceRecheckedRuntimeIDs: ackNone()}
		}
	}
	// Return every successful claim even when another transport failed.
	result.Tasks = append(admitted, result.Tasks...)
	result.AdmissionPolled = polled
	result.AdmissionClaimed = len(admitted) > 0
	if polled && result.ForceRecheckedRuntimeIDs != nil {
		// The OpenCode preview reads the database directly. Drop its wake hints
		// instead of repeatedly asking the ordinary batch cache to clear them.
		for _, id := range forceRecheck {
			if _, ok := previewed[id]; ok {
				*result.ForceRecheckedRuntimeIDs = append(*result.ForceRecheckedRuntimeIDs, id)
			}
		}
	}
	if openCodeErr != nil && ctx.Err() == nil {
		d.logger.Warn("OpenCode scoped claim failed", "error", openCodeErr)
	}
	return result, nil
}
