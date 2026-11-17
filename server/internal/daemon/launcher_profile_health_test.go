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
	if _, _, _, err := d.registerRuntimesForWorkspaceLocked(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	if len(fx.sentFailures) != 0 || len(fx.sentRuntimes) != 1 {
		t.Fatal("profile registration took a running sibling offline")
	}
	d.activeTasks.Add(-1)
	checksAfterFailure := probes
	d.handleTask(ctx, Task{ID: "independent-fresh", RuntimeID: rt.ID}, 0)
	if len(d.claimableRuntimeIDs()) != 0 || probes != checksAfterFailure {
		t.Fatal("independent fresh run retried held profile launcher")
	}
	if len(d.allRuntimeIDs()) != 1 {
		t.Fatal("held profile lost heartbeat coverage before settlement")
	}
	d.demoteHeldLaunchers(ctx)
	if len(fx.sentFailures) != 1 || fx.sentFailures[0]["failure_code"] != protocol.LauncherUnavailableCode || fx.sentFailures[0]["reason"] != launcherUnavailableDetail {
		t.Fatalf("profile registration did not record sanitized launch failure: %+v", fx.sentFailures)
	}
	if len(d.claimableRuntimeIDs()) != 0 {
		t.Fatal("third same-profile dispatch remained eligible")
	}
	broken = false
	d.refreshHeldProfileLaunchers(ctx)
	if ids := d.claimableRuntimeIDs(); len(ids) != 1 || ids[0] != rt.ID {
		t.Fatalf("healthy profile did not restore its existing route: %v", ids)
	}
}
