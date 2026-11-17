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
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

func TestLauncherHealthIndependentRunsDoNotReplayObservedWork(t *testing.T) {
	fx := newLauncherHealthFixture(t)
	d := fx.daemon
	exit127 := launcherProbeExit(t, 127)
	var failures []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/fail") {
			var body struct {
				Reason string `json:"failure_reason"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			failures = append(failures, body.Reason)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"running"}`)
	}))
	defer srv.Close()
	d.client = NewClient(srv.URL)
	runs := 0
	d.runner = taskRunnerFunc(func(_ context.Context, task Task, _ string, _ int, _ *slog.Logger) (TaskResult, error) {
		runs++
		fx.setProbeErr(func(string, int) error { return exit127 })
		if task.TriggerCommentID != "original-comment" {
			t.Error("lost trigger context")
		}
		return TaskResult{Status: "blocked", Comment: "opencode exited with error: exit status 127", SessionID: "observed-session", WorkDir: "preserved-work", FailureReason: taskfailure.ReasonAgentProcessFailure.String()}, nil
	})
	rid := fx.runtimeIDFor("ws-1", "opencode")
	d.handleTask(context.Background(), Task{ID: "first", RuntimeID: rid, TriggerCommentID: "original-comment"}, 0)
	d.handleTask(context.Background(), Task{ID: "independent-second", RuntimeID: rid}, 0)
	if runs != 1 || len(failures) != 2 || failures[0] != taskfailure.ReasonAgentProcessFailure.String() || failures[1] != taskfailure.ReasonRuntimeOffline.String() {
		t.Fatalf("runs=%d failure bookkeeping=%v", runs, failures)
	}
	if len(d.allRuntimeIDs()) != 0 {
		t.Fatal("third same-agent dispatch remained eligible")
	}
}

func TestLauncherHealthExitHelper(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "launcher-health-helper" {
		return
	}
	code, _ := strconv.Atoi(os.Args[len(os.Args)-1])
	os.Exit(code)
}

func TestLauncherHealthRejectsBeforeTaskInput(t *testing.T) {
	fx := newLauncherHealthFixture(t)
	d := fx.daemon
	exit127 := launcherProbeExit(t, 127)
	fx.setProbeErr(func(string, int) error { return exit127 })
	failed := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/fail") {
			t.Error("pre-work failure attempted task work")
		}
		var body struct {
			Reason string `json:"failure_reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Reason != taskfailure.ReasonRuntimeOffline.String() {
			t.Errorf("reason=%s", body.Reason)
		}
		failed++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	d.client = NewClient(srv.URL)
	d.runner = taskRunnerFunc(func(context.Context, Task, string, int, *slog.Logger) (TaskResult, error) {
		t.Fatal("task input reached runner with a broken launcher")
		return TaskResult{}, nil
	})
	rid := fx.runtimeIDFor("ws-1", "opencode")
	for _, id := range []string{"initial", "independent-fresh"} {
		d.handleTask(context.Background(), Task{ID: id, RuntimeID: rid}, 0)
	}
	if failed != 2 || len(d.allRuntimeIDs()) != 0 {
		t.Fatalf("failed=%d eligible=%v", failed, d.allRuntimeIDs())
	}
}

func launcherProbeExit(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command(os.Args[0], "-test.run=^TestLauncherHealthExitHelper$", "launcher-health-helper", strconv.Itoa(code)).Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != code {
		t.Fatalf("fixture exit = %v, want %d", err, code)
	}
	return fmt.Errorf("sensitive probe output must not persist: %w", err)
}

func newLauncherHealthFixture(t *testing.T) *batchFixture {
	t.Helper()
	fx := newBatchFixture(t)
	fx.enableStableRuntimeIDs()
	fx.daemon.cfg.Agents = map[string]AgentEntry{"opencode": {Path: "/fake/opencode"}}
	stubAgentProbe(t, fx.daemon.cfg.Agents)
	fx.setWorkspaces(WorkspaceInfo{ID: "ws-1", Name: "one"})
	if err := fx.daemon.syncWorkspacesFromAPI(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	return fx
}

func TestLauncherHealthSuppressesIndependentDispatchUntilSuccessfulProbe(t *testing.T) {
	fx := newLauncherHealthFixture(t)
	d := fx.daemon
	rid := fx.runtimeIDFor("ws-1", "opencode")
	rt := *d.findRuntime(rid)
	exit127 := launcherProbeExit(t, 127)
	fx.setProbeErr(func(string, int) error { return exit127 })

	// The first process failure stays failed; the probe gets no task input.
	d.activeTasks.Add(1)
	d.checkFailedLauncher(context.Background(), rt, taskfailure.ReasonAgentProcessFailure.String())
	for independentEnqueue := 0; independentEnqueue < 2; independentEnqueue++ {
		if ids := d.allRuntimeIDs(); len(ids) != 0 {
			t.Fatalf("fresh dispatch %d could still claim broken runtime: %v", independentEnqueue, ids)
		}
	}
	// Do not deregister under another task that might have produced work.
	d.demoteHeldLaunchers(context.Background())
	if d.findRuntime(rid) == nil {
		t.Fatal("demoted under a running task")
	}
	d.activeTasks.Add(-1)
	d.demoteHeldLaunchers(context.Background())
	if d.findRuntime(rid) != nil {
		t.Fatal("idle broken runtime was not demoted")
	}
	reason, ok := fx.runtimeOfflineReason(rid)
	if !ok || reason.Detail != launcherUnavailableDetail || reason.Code != RuntimeOfflineCodeNotExecutable {
		t.Fatalf("sanitized diagnostic = %+v, present=%v", reason, ok)
	}
	// Further failing health evidence cannot revive the runtime.
	d.convergeRuntimeRegistrations(context.Background())
	if len(d.allRuntimeIDs()) != 0 {
		t.Fatal("failed probe restored eligibility")
	}
	fx.setProbeErr(nil)
	d.convergeRuntimeRegistrations(context.Background())
	if ids := d.allRuntimeIDs(); len(ids) != 1 || ids[0] != rid {
		t.Fatalf("successful probe did not restore original runtime: %v", ids)
	}
}

func TestLauncherHealthControls(t *testing.T) {
	for _, tc := range []struct {
		name, provider, profile, reason string
		probeErr                        error
		cancel                          bool
	}{
		{name: "task exits 127 but launcher healthy", provider: "opencode", reason: taskfailure.ReasonAgentProcessFailure.String()},
		{name: "ordinary nonzero health probe", provider: "opencode", reason: taskfailure.ReasonAgentProcessFailure.String(), probeErr: launcherProbeExit(t, 1)},
		{name: "unavailable health evidence", provider: "opencode", reason: taskfailure.ReasonAgentProcessFailure.String(), probeErr: errors.New("unavailable")},
		{name: "ambiguous textual exit", provider: "opencode", reason: taskfailure.ReasonAgentProcessFailure.String(), probeErr: errors.New("exit status 127")},
		{name: "auth", provider: "opencode", reason: taskfailure.ReasonAgentProviderAuthOrAccess.String()},
		{name: "timeout", provider: "opencode", reason: taskfailure.ReasonTimeout.String()},
		{name: "cancel", provider: "opencode", reason: taskfailure.ReasonAgentProcessFailure.String(), cancel: true},
		{name: "custom profile", provider: "opencode", profile: "custom", reason: taskfailure.ReasonAgentProcessFailure.String()},
		{name: "other provider", provider: "codex", reason: taskfailure.ReasonAgentProcessFailure.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newLauncherHealthFixture(t)
			fx.setProbeErr(func(string, int) error { return tc.probeErr })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			fx.daemon.checkFailedLauncher(ctx, Runtime{Provider: tc.provider, ProfileID: tc.profile}, tc.reason)
			fx.daemon.demoteHeldLaunchers(context.Background())
			if len(fx.daemon.allRuntimeIDs()) != 1 {
				t.Fatal("ordinary failure changed launcher eligibility")
			}
		})
	}
}
