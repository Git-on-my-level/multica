package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

func TestLauncherHealthDoesNotChangeProcessFailureRetryPolicy(t *testing.T) {
	for _, text := range []string{"opencode exited with error: exit status 127", "opencode exited with error: exit status 1"} {
		reason := taskfailure.Classify(text)
		if reason != taskfailure.ReasonAgentProcessFailure || retryableReasons[reason.String()] {
			t.Fatalf("%q: reason=%s retryable=%v", text, reason, retryableReasons[reason.String()])
		}
	}
}

func TestLauncherHealthHeartbeatCannotRestoreClaims(t *testing.T) {
	ctx := context.Background()
	pool := sharedTestPool(t)
	q := db.New(pool)
	// Use a dedicated runtime and task so assertions stay scoped to this test.
	bootstrap := testutil.New(pool, "", "")
	userID := bootstrap.User(t, "launcher-health", "launcher-health-"+uuid.NewString()+"@example.com")
	workspaceID := bootstrap.Workspace(t, "launcher health", "launcher-health-"+userID)
	fx := testutil.New(pool, workspaceID, userID)
	fx.Member(t, workspaceID, userID, "owner")
	runtimeID := fx.Runtime(t, "broken launcher")
	agentID := fx.Agent(t, "launcher agent", runtimeID)
	issueID := fx.Issue(t, "preserve unstarted outcome")
	taskID := fx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "queued",
		"context": `{"input":"preserve this outcome","handoff_note":"same model only"}`,
	})
	rid := util.MustParseUUID(runtimeID)
	if err := q.SetAgentRuntimeOfflineWithReason(ctx, db.SetAgentRuntimeOfflineWithReasonParams{
		ID: rid, OfflineReason: []byte(`{"code":"not_executable","detail":"launcher health check exited 127 before task input"}`),
	}); err != nil {
		t.Fatal(err)
	}
	for independentEnqueue := 0; independentEnqueue < 2; independentEnqueue++ {
		if changed, err := q.MarkAgentRuntimeOnlineIfOffline(ctx, rid); err != nil || changed != 0 {
			t.Fatalf("heartbeat transition: changed=%d error=%v", changed, err)
		}
		if rt, err := q.MarkAgentRuntimeOnline(ctx, rid); err != nil || rt.Status != "offline" {
			t.Fatalf("heartbeat revived launcher: status=%s error=%v", rt.Status, err)
		}
		_, err := q.ClaimAgentTask(ctx, db.ClaimAgentTaskParams{
			AgentID: util.MustParseUUID(agentID), RuntimeID: rid, PrepareLeaseSecs: 60, RuntimeStaleSecs: RuntimeClaimFreshnessSeconds,
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("broken launcher claim: %v", err)
		}
	}
	var status, input, note string
	if err := pool.QueryRow(ctx, `SELECT status, context->>'input', context->>'handoff_note' FROM agent_task_queue WHERE id=$1`, taskID).Scan(&status, &input, &note); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || input != "preserve this outcome" || note != "same model only" {
		t.Fatalf("unstarted outcome changed: %q %q %q", status, input, note)
	}
}
