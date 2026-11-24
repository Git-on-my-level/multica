package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

type fakeAdmission struct {
	granted      bool
	err          error
	holder       string
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

func (f *fakeAdmission) acquire(_ context.Context, _, holder string, pid int) (bool, error) {
	f.acquires++
	f.holder = holder
	if pid != os.Getpid() {
		return false, fmt.Errorf("wrong PID: %d", pid)
	}
	return f.granted, f.err
}

func (f *fakeAdmission) release(ctx context.Context, _, holder string) error {
	if holder != f.holder || ctx.Err() != nil {
		return fmt.Errorf("release lost its holder or cleanup context")
	}
	f.releases++
	return nil
}

func (f *fakeAdmission) observe(_ context.Context, _ string, attempts, throttled int64) error {
	f.observations = append(f.observations, [2]int64{attempts, throttled})
	return nil
}

func TestAdmissionClaimBoundary(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		granted, hasTask                     bool
		acquireErr                           error
		wantScoped, wantRegular, wantRelease int
	}{
		{name: "denied"},
		{name: "granted without task", granted: true, wantScoped: 1, wantRelease: 1},
		{name: "granted with task", granted: true, hasTask: true, wantScoped: 1, wantRelease: 1},
		{name: "acquire error fails open", acquireErr: errors.New("agentctl unavailable"), wantRegular: 1, wantRelease: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var scopedClaims, regularClaims atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == openCodeAdmissionPath("rt"):
					_ = json.NewEncoder(w).Encode([]protocol.AdmissionCandidate{{AgentID: "agent", Model: "morph/deepseek#low"}})
				case r.Method == http.MethodPost && r.URL.Path == openCodeAdmissionPath("rt"):
					scopedClaims.Add(1)
					var candidate protocol.AdmissionCandidate
					if err := json.NewDecoder(r.Body).Decode(&candidate); err != nil || candidate.AgentID != "agent" || candidate.Model != "morph/deepseek#low" {
						t.Errorf("unexpected claim candidate: %+v, %v", candidate, err)
					}
					if tc.hasTask {
						_, _ = fmt.Fprint(w, `{"task":{"id":"task","runtime_id":"rt","agent_id":"agent"}}`)
					} else {
						_, _ = fmt.Fprint(w, `{"task":null}`)
					}
				case r.URL.Path == "/api/daemon/tasks/claim":
					regularClaims.Add(1)
					_, _ = fmt.Fprint(w, `{"tasks":[{"id":"task","runtime_id":"rt","agent_id":"agent"}]}`)
				default:
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			}))
			defer srv.Close()

			d := New(Config{ServerBaseURL: srv.URL, MaxConcurrentTasks: 1}, slog.New(slog.NewTextHandler(noopWriter{}, nil)))
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
			if fake.matches != 1 || fake.acquires != 1 {
				t.Fatalf("matches = %d, acquires = %d", fake.matches, fake.acquires)
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
		switch r.Method {
		case http.MethodGet:
			_, _ = fmt.Fprint(w, `[{"agent_id":"denied","model":"morph/deepseek"},{"agent_id":"other","model":"provider/other"}]`)
		case http.MethodPost:
			scopedClaims.Add(1)
			var candidate protocol.AdmissionCandidate
			if err := json.NewDecoder(r.Body).Decode(&candidate); err != nil || candidate.AgentID != "other" {
				t.Errorf("attempted to claim the wrong agent: %+v, %v", candidate, err)
			}
			_, _ = fmt.Fprint(w, `{"task":{"id":"task","runtime_id":"rt","agent_id":"other"}}`)
		}
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
