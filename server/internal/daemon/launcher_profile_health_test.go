package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestLauncherHealthMorphProfileSuppressesFreshRunsAndRecovers(t *testing.T) {
	ctx := context.Background()
	profile := RuntimeProfile{ID: "morph-profile", ProtocolFamily: "opencode", RuntimeType: "opencode", CommandName: "opencode-morph-fixture", FixedArgs: []string{"managed-route"}, Enabled: true}
	fx := newProfileRegisterFixture(t, []RuntimeProfile{profile}, http.StatusOK)
	d := fx.daemon
	d.cfg.Agents = map[string]AgentEntry{}
	stubLookPath(t, map[string]string{profile.CommandName: "/fake/opencode-morph-fixture"})
	original := detectAgentVersion
	t.Cleanup(func() { detectAgentVersion = original })
	broken := false
	exit127 := launcherProbeExit(t, 127)
	probes := 0
	detectAgentVersion = func(_ context.Context, command agent.Command) (string, error) {
		probes++
		if command.Path != "/fake/opencode-morph-fixture" || strings.Join(command.Prefix, " ") != "managed-route" {
			t.Fatalf("health check did not use profile launcher and fixed args: %+v", command)
		}
		if broken {
			return "", exit127
		}
		return "9.9.9", nil
	}
	resp, _, _, err := d.registerRuntimesForWorkspaceLocked(ctx, "ws-1")
	if err != nil || len(resp.Runtimes) != 1 {
		t.Fatalf("register profile: %+v %v", resp, err)
	}
	rt := resp.Runtimes[0]
	d.runtimeIndex[rt.ID] = rt
	d.workspaces["ws-1"] = newWorkspaceState("ws-1", []string{rt.ID}, "", nil, nil)
	d.runner = taskRunnerFunc(func(context.Context, Task, string, int, *slog.Logger) (TaskResult, error) {
		t.Fatal("broken Morph launcher received task input")
		return TaskResult{}, nil
	})
	broken = true
	d.handleTask(ctx, Task{ID: "initial", RuntimeID: rt.ID}, 0)
	d.activeTasks.Add(1)
	d.launcherActive = map[string]int{launcherKey(rt): 1}
	if _, _, _, err := d.registerRuntimesForWorkspaceLocked(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	if len(fx.sentFailures) != 0 || len(fx.sentRuntimes) != 1 {
		t.Fatal("profile registration took a running sibling offline")
	}
	delete(d.launcherActive, launcherKey(rt))
	defer d.activeTasks.Add(-1) // unrelated runtime stays busy throughout recovery
	checksAfterFailure := probes
	d.handleTask(ctx, Task{ID: "independent-fresh", RuntimeID: rt.ID}, 0)
	if len(d.claimableRuntimeIDs()) != 0 || probes != checksAfterFailure {
		t.Fatal("independent fresh run retried held profile launcher")
	}
	if len(d.allRuntimeIDs()) != 1 {
		t.Fatal("held profile lost heartbeat coverage before settlement")
	}
	d.refreshHeldProfileLaunchers(ctx) // existing discovery tick reports the hold
	if len(fx.sentFailures) != 1 || fx.sentFailures[0]["failure_code"] != protocol.LauncherUnavailableCode || fx.sentFailures[0]["reason"] != launcherUnavailableDetail {
		t.Fatalf("profile registration did not record sanitized launch failure: %+v", fx.sentFailures)
	}
	if len(d.claimableRuntimeIDs()) != 0 {
		t.Fatal("third same-profile dispatch remained eligible")
	}
	broken = false
	fx.registerStatus = http.StatusServiceUnavailable
	d.refreshHeldProfileLaunchers(ctx)
	if len(d.claimableRuntimeIDs()) != 0 {
		t.Fatal("failed recovery registration lost the pending health gate")
	}
	fx.registerStatus = 0
	d.refreshHeldProfileLaunchers(ctx)
	if ids := d.claimableRuntimeIDs(); len(ids) != 1 || ids[0] != rt.ID {
		t.Fatalf("healthy profile did not restore its existing route: %v", ids)
	}
}

func TestLauncherHealthUnavailableProfileSnapshotPreservesRuntimes(t *testing.T) {
	fx := newProfileRegisterFixture(t, nil, http.StatusServiceUnavailable)
	d := fx.daemon
	rt := Runtime{ID: "morph", Provider: "opencode", ProfileID: "morph-profile"}
	d.runtimeIndex[rt.ID] = rt
	d.runtimeIndex["other"] = Runtime{ID: "other", Provider: "codex"}
	d.workspaces["ws-1"] = newWorkspaceState("ws-1", []string{rt.ID, "other"}, "", nil, nil)
	d.recordProfileLaunch("ws-1", rt.ProfileID, "/fake/morph", "9.9.9", nil)
	d.holdLauncher(launcherKey(rt))
	d.refreshHeldProfileLaunchers(context.Background())
	if len(d.allRuntimeIDs()) != 2 || len(d.claimableRuntimeIDs()) != 1 || len(fx.sentRuntimes) != 0 || len(fx.sentFailures) != 0 {
		t.Fatal("unavailable profile snapshot altered runtime registration")
	}
}

func TestLauncherHealthDisabledProfilePrunesOnlyAfterItsTaskSettles(t *testing.T) {
	profile := RuntimeProfile{ID: "morph-profile", ProtocolFamily: "opencode", CommandName: "fixture", Enabled: false}
	fx := newProfileRegisterFixture(t, []RuntimeProfile{profile}, http.StatusOK)
	d := fx.daemon
	rt := Runtime{ID: "morph", Provider: "opencode", ProfileID: profile.ID}
	d.runtimeIndex[rt.ID] = rt
	d.runtimeIndex["other"] = Runtime{ID: "other", Provider: "codex"}
	d.workspaces["ws-1"] = newWorkspaceState("ws-1", []string{rt.ID, "other"}, "", nil, nil)
	d.recordProfileLaunch("ws-1", profile.ID, "/fake/morph", "9.9.9", nil)
	d.holdLauncher(launcherKey(rt))
	d.launcherActive = map[string]int{launcherKey(rt): 1}
	d.refreshHeldProfileLaunchers(context.Background())
	if d.findRuntime(rt.ID) == nil {
		t.Fatal("disabled profile dropped an active task's runtime")
	}
	delete(d.launcherActive, launcherKey(rt))
	d.refreshHeldProfileLaunchers(context.Background())
	if d.findRuntime(rt.ID) != nil || d.findRuntime("other") == nil || len(d.profileLaunchSpecs) != 0 || len(d.demotedProviders) != 0 {
		t.Fatal("disabled profile left a permanent recovery loop or altered unrelated runtime")
	}
}
