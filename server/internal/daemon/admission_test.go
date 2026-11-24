package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

type fakeAdmission struct {
	granted      bool
	err          error
	acquireHook  func(context.Context, int) (bool, error)
	holder       string
	holders      []string
	matches      int
	acquires     int
	releases     int
	observations [][2]int64
}

func (f *fakeAdmission) match(_ context.Context, model string) (string, error) {
	f.matches++
	if model == "morph/deepseek" {
		return "morph", nil
	}
	return "", nil
}

func (f *fakeAdmission) acquire(ctx context.Context, _, holder string, pid int) (bool, error) {
	f.acquires++
	f.holder = holder
	f.holders = append(f.holders, holder)
	if pid != os.Getpid() {
		return false, fmt.Errorf("wrong PID: %d", pid)
	}
	if f.acquireHook != nil {
		return f.acquireHook(ctx, f.acquires)
	}
	return f.granted, f.err
}

func (f *fakeAdmission) release(ctx context.Context, _, holder string) error {
	if !slices.Contains(f.holders, holder) || ctx.Err() != nil {
		return fmt.Errorf("release lost its holder or cleanup context")
	}
	f.releases++
	return nil
}

func (f *fakeAdmission) observe(_ context.Context, _ string, attempts, throttled int64) error {
	f.observations = append(f.observations, [2]int64{attempts, throttled})
	return nil
}

type admissionBatchRequest struct {
	MaxTasks   int                                `json:"max_tasks"`
	RuntimeIDs []string                           `json:"runtime_ids"`
	Permits    []protocol.BatchAdmissionCandidate `json:"admission_candidates"`
}

func readAdmissionBatchRequest(t *testing.T, r *http.Request) admissionBatchRequest {
	t.Helper()
	var req admissionBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("decode admission request: %v", err)
	}
	return req
}

func TestAdmissionClaimBoundary(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		granted, hasTask, previewUnavailable bool
		acquireErr                           error
		wantScoped, wantRegular, wantRelease int
	}{
		{name: "denied", wantScoped: 1},
		{name: "granted without task", granted: true, wantScoped: 1, wantRelease: 1},
		{name: "granted with task", granted: true, hasTask: true, wantScoped: 1, wantRelease: 1},
		{name: "agentctl unavailable before first check", acquireErr: errors.New("agentctl unavailable"), wantRegular: 1, wantRelease: 1},
		{name: "preview unavailable before any check", previewUnavailable: true, wantRegular: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var scopedClaims, regularClaims atomic.Int32
			const candidate = `{"runtime_id":"rt","agent_id":"agent","model":"morph/deepseek#low"}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case admissionBatchPath:
					req := readAdmissionBatchRequest(t, r)
					if req.MaxTasks == 0 {
						if tc.previewUnavailable {
							http.NotFound(w, r)
							return
						}
						_, _ = fmt.Fprint(w, `{"admission_candidates":[`+candidate+`]}`)
						return
					}
					scopedClaims.Add(1)
					if tc.granted {
						if len(req.Permits) != 1 || req.Permits[0].AgentID != "agent" || req.Permits[0].Model != "morph/deepseek#low" {
							t.Errorf("unexpected permits: %+v", req.Permits)
						}
					} else if len(req.Permits) != 0 {
						t.Errorf("denied agent received a permit: %+v", req.Permits)
					}
					if tc.hasTask {
						_, _ = fmt.Fprint(w, `{"tasks":[{"id":"task","runtime_id":"rt","agent_id":"agent"}]}`)
					} else {
						_, _ = fmt.Fprint(w, `{"tasks":[]}`)
					}
				case "/api/daemon/tasks/claim":
					regularClaims.Add(1)
					_, _ = fmt.Fprint(w, `{"tasks":[{"id":"task","runtime_id":"rt","agent_id":"agent"}]}`)
				default:
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			}))
			defer srv.Close()
			var logs bytes.Buffer
			d := New(Config{ServerBaseURL: srv.URL, MaxConcurrentTasks: 1}, slog.New(slog.NewTextHandler(&logs, nil)))
			d.runtimeIndex["rt"] = Runtime{ID: "rt", Provider: "opencode"}
			fake := &fakeAdmission{granted: tc.granted, err: tc.acquireErr}
			d.admission = newAdmissionController(fake, d.logger)

			result, err := d.claimTasksWithAdmission(t.Context(), []string{"rt"}, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			if int(scopedClaims.Load()) != tc.wantScoped || int(regularClaims.Load()) != tc.wantRegular {
				t.Fatalf("scoped = %d, regular = %d", scopedClaims.Load(), regularClaims.Load())
			}
			wantChecks := 1
			if tc.previewUnavailable {
				wantChecks = 0
			}
			if fake.matches != wantChecks || fake.acquires != wantChecks {
				t.Fatalf("matches = %d, acquires = %d, want %d", fake.matches, fake.acquires, wantChecks)
			}
			wantDenied := !tc.granted && tc.acquireErr == nil && !tc.previewUnavailable
			if result.AdmissionDenied != wantDenied {
				t.Fatalf("AdmissionDenied = %t, want %t", result.AdmissionDenied, wantDenied)
			}
			if wantDenied {
				if log := logs.String(); !strings.Contains(log, "level=INFO") ||
					!strings.Contains(log, "bucket=morph") || !strings.Contains(log, "holder="+fake.holder) {
					t.Fatalf("denial log missing bucket or holder: %s", log)
				}
			}
			wantTasks := tc.hasTask || tc.wantRegular > 0
			if (len(result.Tasks) > 0) != wantTasks {
				t.Fatalf("tasks = %v", result.Tasks)
			}
			if tc.hasTask {
				lease := result.Tasks[0].admission
				if lease == nil || fake.releases != 0 {
					t.Fatalf("task must own its live lease until it exits")
				}
				lease.countAttempt(false)
				lease.countAttempt(true)
				lease.countAttempt(false)
				lease.release()
				lease.release()
				if !reflect.DeepEqual(fake.observations, [][2]int64{{3, 1}}) {
					t.Fatalf("observations = %v", fake.observations)
				}
			}
			if fake.releases != tc.wantRelease {
				t.Fatalf("releases = %d, want %d", fake.releases, tc.wantRelease)
			}
		})
	}
}

func TestAdmissionDenialDoesNotSuppressOtherAgents(t *testing.T) {
	var scopedClaims atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != admissionBatchPath {
			t.Errorf("unexpected claim endpoint %s", r.URL.Path)
		}
		req := readAdmissionBatchRequest(t, r)
		if req.MaxTasks == 0 {
			_, _ = fmt.Fprint(w, `{"admission_candidates":[{"runtime_id":"rt","agent_id":"denied","model":"morph/deepseek"},{"runtime_id":"rt","agent_id":"other","model":"provider/other"}]}`)
			return
		}
		scopedClaims.Add(1)
		if len(req.Permits) != 1 || req.Permits[0].AgentID != "other" {
			t.Errorf("attempted to claim the wrong agent: %+v", req.Permits)
		}
		_, _ = fmt.Fprint(w, `{"tasks":[{"id":"task","runtime_id":"rt","agent_id":"other"}]}`)
	}))
	defer srv.Close()
	d := New(Config{ServerBaseURL: srv.URL, MaxConcurrentTasks: 1}, slog.New(slog.NewTextHandler(noopWriter{}, nil)))
	d.runtimeIndex["rt"] = Runtime{ID: "rt", Provider: "opencode"}
	fake := &fakeAdmission{}
	d.admission = newAdmissionController(fake, d.logger)
	for range 2 {
		result, err := d.claimTasksWithAdmission(t.Context(), []string{"rt"}, 1, nil)
		if err != nil || len(result.Tasks) != 1 || result.Tasks[0].AgentID != "other" || result.Tasks[0].admission != nil {
			t.Fatalf("unexpected claims: %+v, %v", result, err)
		}
	}
	if scopedClaims.Load() != 2 || fake.matches != 2 || fake.acquires != 2 || fake.releases != 0 {
		t.Fatalf("scoped=%d matches=%d acquires=%d releases=%d", scopedClaims.Load(), fake.matches, fake.acquires, fake.releases)
	}
}

func TestAdmissionPartialScanNeverFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name          string
		exhaustBudget bool
		firstGranted  bool
	}{
		{name: "error after denial"},
		{name: "budget expires after denial", exhaustBudget: true},
		{name: "error after grant", firstGranted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var batchClaims atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != admissionBatchPath {
					t.Errorf("partial scan used unscoped claim %s", r.URL.Path)
					http.Error(w, "unexpected unscoped claim", http.StatusBadRequest)
					return
				}
				req := readAdmissionBatchRequest(t, r)
				if req.MaxTasks == 0 {
					_, _ = fmt.Fprint(w, `{"admission_candidates":[{"runtime_id":"oc","agent_id":"first","model":"morph/deepseek"},{"runtime_id":"oc","agent_id":"unchecked","model":"morph/deepseek"},{"runtime_id":"oc-other","agent_id":"later","model":"morph/deepseek"}]}`)
					return
				}
				batchClaims.Add(1)
				if !reflect.DeepEqual(req.RuntimeIDs, []string{"oc", "oc-other", "regular"}) {
					t.Errorf("unexpected partial batch: %+v", req)
				}
				if tc.firstGranted {
					if len(req.Permits) != 1 || req.Permits[0].AgentID != "first" {
						t.Errorf("partial scan lost the earlier grant or included unchecked work: %+v", req.Permits)
					}
				} else if len(req.Permits) != 0 {
					t.Errorf("partial scan included a denied or unchecked agent: %+v", req.Permits)
				}
				_, _ = fmt.Fprint(w, `{"tasks":[{"id":"ordinary","runtime_id":"regular","agent_id":"agent"}]}`)
			}))
			defer srv.Close()
			d := New(Config{ServerBaseURL: srv.URL}, slog.New(slog.NewTextHandler(noopWriter{}, nil)))
			d.runtimeIndex["oc"] = Runtime{ID: "oc", Provider: "opencode"}
			fake := &fakeAdmission{acquireHook: func(ctx context.Context, attempt int) (bool, error) {
				if attempt == 1 {
					return tc.firstGranted, nil
				}
				if tc.exhaustBudget {
					<-ctx.Done()
					return false, ctx.Err()
				}
				return false, errors.New("agentctl unavailable after first check")
			}}
			d.admission = newAdmissionController(fake, d.logger)
			d.admission.scanTimeout = 500 * time.Millisecond
			result, err := d.claimTasksWithAdmission(t.Context(), []string{"oc", "oc-other", "regular"}, 1, nil)
			if err != nil || result.AdmissionDenied == tc.firstGranted || len(result.Tasks) != 1 || result.Tasks[0].ID != "ordinary" {
				t.Fatalf("unexpected partial result: %+v, %v", result, err)
			}
			if batchClaims.Load() != 1 || fake.acquires != 2 {
				t.Fatalf("batch claims = %d, admission attempts = %d", batchClaims.Load(), fake.acquires)
			}
		})
	}
}

func TestAdmissionExpiredPreviewDoesNotFallBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != admissionBatchPath {
			t.Errorf("expired preview used unscoped claim %s", r.URL.Path)
			http.Error(w, "unexpected unscoped claim", http.StatusBadRequest)
			return
		}
		req := readAdmissionBatchRequest(t, r)
		if req.MaxTasks == 0 {
			<-r.Context().Done()
			return
		}
		if len(req.Permits) != 0 {
			t.Errorf("expired preview sent permits: %+v", req.Permits)
		}
		_, _ = fmt.Fprint(w, `{"tasks":[{"id":"ordinary","runtime_id":"regular","agent_id":"agent"}]}`)
	}))
	defer srv.Close()
	d := New(Config{ServerBaseURL: srv.URL}, slog.New(slog.NewTextHandler(noopWriter{}, nil)))
	d.runtimeIndex["oc"] = Runtime{ID: "oc", Provider: "opencode"}
	fake := &fakeAdmission{granted: true}
	d.admission = newAdmissionController(fake, d.logger)
	d.admission.scanTimeout = 200 * time.Millisecond
	result, err := d.claimTasksWithAdmission(t.Context(), []string{"oc", "regular"}, 1, nil)
	if err != nil || !result.ClaimedWithAdmission || len(result.Tasks) != 1 || result.Tasks[0].ID != "ordinary" || fake.acquires != 0 {
		t.Fatalf("unexpected result after expired preview: %+v, %v, acquires %d", result, err, fake.acquires)
	}
}

func TestAdmissionBatchReleasesPermitWhenOrdinaryWorkWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != admissionBatchPath {
			t.Errorf("split claim sent to %s", r.URL.Path)
		}
		req := readAdmissionBatchRequest(t, r)
		if req.MaxTasks == 0 {
			_, _ = fmt.Fprint(w, `{"admission_candidates":[{"runtime_id":"oc","agent_id":"admitted","model":"morph/deepseek"}]}`)
			return
		}
		if req.MaxTasks != 1 || !reflect.DeepEqual(req.RuntimeIDs, []string{"oc", "regular"}) ||
			len(req.Permits) != 1 || req.Permits[0].AgentID != "admitted" {
			t.Errorf("unexpected mixed claim: %+v", req)
		}
		_, _ = fmt.Fprint(w, `{"tasks":[{"id":"winner","runtime_id":"regular","agent_id":"ordinary"}]}`)
	}))
	defer srv.Close()
	d := New(Config{ServerBaseURL: srv.URL}, slog.New(slog.NewTextHandler(noopWriter{}, nil)))
	d.runtimeIndex["oc"] = Runtime{ID: "oc", Provider: "opencode"}
	fake := &fakeAdmission{granted: true}
	d.admission = newAdmissionController(fake, d.logger)
	result, err := d.claimTasksWithAdmission(t.Context(), []string{"oc", "regular"}, 1, nil)
	if err != nil || len(result.Tasks) != 1 || result.Tasks[0].ID != "winner" {
		t.Fatalf("unexpected batch result: %+v, %v", result, err)
	}
	if fake.releases != 1 || result.Tasks[0].admission != nil {
		t.Fatalf("ordinary work inherited an unused permit, releases = %d", fake.releases)
	}
}

func TestAdmissionCancellationRetainsSlotUntilOpenCodeExits(t *testing.T) {
	fake := &fakeAdmission{granted: true}
	controller := newAdmissionController(fake, slog.New(slog.NewTextHandler(noopWriter{}, nil)))
	lease, allowed, err := controller.reserve(t.Context(), "rt", "morph/deepseek")
	if !allowed || err != nil || lease == nil {
		t.Fatalf("reserve = %v, %t, %v", lease, allowed, err)
	}
	exited := lease.processStarted()
	lease.release()
	if fake.releases != 0 {
		t.Fatal("slot released while the cancelled process was still running")
	}
	// The stream can still deliver events during bounded cancellation cleanup.
	lease.countAttempt(true)
	exited()
	if fake.releases != 1 || !reflect.DeepEqual(fake.observations, [][2]int64{{1, 1}}) {
		t.Fatalf("releases=%d, observations=%v", fake.releases, fake.observations)
	}
}

func TestAdmissionLeaseReleasedWhenTaskExits(t *testing.T) {
	for _, status := range []string{"completed", "blocked", "cancelled", "shutdown"} {
		t.Run(status, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, `{"status":"running"}`)
			}))
			defer server.Close()
			d := &Daemon{
				client: NewClient(server.URL), logger: slog.New(slog.NewTextHandler(noopWriter{}, nil)),
				runtimeIndex: map[string]Runtime{"rt": {ID: "rt", Provider: "opencode"}},
			}
			fake := &fakeAdmission{granted: true}
			d.admission = newAdmissionController(fake, d.logger)
			lease, allowed, err := d.admission.reserve(ctx, "rt", "morph/deepseek")
			if !allowed || err != nil || lease == nil {
				t.Fatalf("reserve = %v, %t, %v", lease, allowed, err)
			}
			d.runner = taskRunnerFunc(func(_ context.Context, task Task, _ string, _ int, _ *slog.Logger) (TaskResult, error) {
				if task.admission != lease || fake.releases != 0 {
					t.Fatal("lease must remain held while the runner is active")
				}
				task.admission.countAttempt(true)
				if status == "shutdown" {
					cancel()
					return TaskResult{Status: "cancelled"}, nil
				}
				return TaskResult{Status: status}, nil
			})
			d.handleTask(ctx, Task{ID: "task", RuntimeID: "rt", admission: lease}, newTestTaskSlot(0))
			if fake.releases != 1 || !reflect.DeepEqual(fake.observations, [][2]int64{{1, 1}}) {
				t.Fatalf("releases=%d, observations=%v", fake.releases, fake.observations)
			}
		})
	}
}

func TestAdmissionDenialShortensClaimPoll(t *testing.T) {
	d := New(Config{PollInterval: 30 * time.Second, WSClaimPollInterval: 3 * time.Minute}, slog.New(slog.NewTextHandler(noopWriter{}, nil)))
	generation := d.wsRPC.attach(func([]byte) (*wsOutbound, error) { return nil, nil })
	d.wsRPC.markRPCV1Supported(generation)
	result := claimTasksResult{ClaimedWithAdmission: true, ClaimPollHintSupported: true}
	if interval := d.taskClaimPollInterval(result); interval <= d.cfg.PollInterval {
		t.Fatalf("empty preview shortened normal WS wait to %s", interval)
	}
	result.AdmissionDenied = true
	if interval := d.taskClaimPollInterval(result); interval != d.cfg.PollInterval {
		t.Fatalf("denied candidate wait = %s, want %s", interval, d.cfg.PollInterval)
	}
}
