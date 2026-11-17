package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestLauncherHandoffPreservesUnstartedOutcome(t *testing.T) {
	for _, mode := range []string{"healthy-peer", "source-successor", "busy-peer", "no-peer", "wrong-model", "wrong-options", "unauthorized-peer", "same-host", "stale-peer", "held-peer", "started", "message", "session", "workdir", "branch", "result", "cancelled", "stale-claim"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			pool := sharedTestPool(t)
			q := db.New(pool)
			bootstrap := testutil.New(pool, "", "")
			userID := bootstrap.User(t, "handoff", "handoff-"+uuid.NewString()+"@example.com")
			workspaceID := bootstrap.Workspace(t, "launcher handoff", "handoff-"+userID)
			fx := testutil.New(pool, workspaceID, userID)
			fx.Member(t, workspaceID, userID, "owner")
			sourceHost := "broken-" + uuid.NewString()
			runtimeID := fx.Runtime(t, "source launcher", testutil.Cols{"provider": "opencode", "runtime_mode": "local", "daemon_id": sourceHost, "metadata": testutil.Raw(`'{"version":"1.0.0"}'::jsonb`)})
			model := "morph/morph-kimik31"
			agentID := fx.Agent(t, "source", runtimeID, testutil.Cols{"model": model, "thinking_level": "high", "service_tier": "standard"})
			peerAgent, peerRuntime := "", ""
			if mode != "no-peer" {
				host := "healthy-" + uuid.NewString()
				if mode == "same-host" {
					host = sourceHost
				}
				cols := testutil.Cols{"provider": "opencode", "runtime_mode": "local", "daemon_id": host, "metadata": testutil.Raw(`'{"version":"1.0.0"}'::jsonb`)}
				if mode == "stale-peer" {
					cols["last_seen_at"] = testutil.Raw("now() - interval '1 hour'")
				}
				if mode == "held-peer" {
					cols["metadata"] = testutil.Raw(`'{"version":"1.0.0","offline_reason":{"launcher_unavailable":true}}'::jsonb`)
				}
				if mode == "same-host" {
					peerRuntime = runtimeID
				} else {
					peerRuntime = fx.Runtime(t, "peer launcher", cols)
				}
				peerModel := model
				if mode == "wrong-model" {
					peerModel = "different/model"
				}
				peerAgent = fx.Agent(t, "peer", peerRuntime, testutil.Cols{"model": peerModel, "thinking_level": "high", "service_tier": "standard"})
				if mode == "wrong-options" {
					if _, err := pool.Exec(ctx, `UPDATE agent SET thinking_level='low' WHERE id=$1`, peerAgent); err != nil {
						t.Fatal(err)
					}
				}
			}
			issueID := fx.Issue(t, "preserve requested outcome")
			stamp := time.Now().UTC().Truncate(time.Microsecond)
			cols := testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "dispatched", "dispatched_at": stamp, "attempt": 1, "max_attempts": 1,
				"originator_user_id": userID, "accountable_user_id": userID, "originator_source": "direct_human",
				"context": `{"input":"exact preserved input","nested":{"value":42}}`, "handoff_note": "preserve the outcome", "trigger_summary": "original trigger"}
			observed := false
			switch mode {
			case "started":
				cols["started_at"] = stamp
				cols["status"] = "running"
				observed = true
			case "session":
				cols["session_id"] = "observed-session"
				observed = true
			case "workdir":
				cols["work_dir"] = "/observed"
				observed = true
			case "branch":
				cols["branch_name"] = "observed-branch"
				observed = true
			case "result":
				cols["result"] = `{"output":"observed"}`
				observed = true
			case "cancelled":
				cols["status"] = "cancelled"
				observed = true
			case "message", "stale-claim":
				observed = true
			}
			taskID := fx.Task(t, agentID, cols)
			fx.Cleanup(t, `DELETE FROM agent_task_queue WHERE parent_task_id=$1`, taskID)
			if mode == "busy-peer" {
				fx.Task(t, peerAgent, testutil.Cols{"runtime_id": peerRuntime, "issue_id": issueID, "status": "queued"})
			}
			if mode == "source-successor" {
				fx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "deferred"})
			}
			if mode == "unauthorized-peer" {
				other := bootstrap.User(t, "unprivileged", "unprivileged-"+uuid.NewString()+"@example.com")
				fx.Member(t, workspaceID, other, "member")
				if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET originator_user_id=$2, accountable_user_id=$2 WHERE id=$1`, taskID, other); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "message" {
				fx.Insert(t, "task_message", testutil.Cols{"task_id": taskID, "seq": 1, "type": "assistant", "content": "observed work"})
			}
			claim := protocol.LauncherClaim{DispatchedAt: stamp.Format(time.RFC3339Nano), Model: model, ThinkingLevel: "high", ServiceTier: "standard"}
			if mode == "stale-claim" {
				claim.DispatchedAt = stamp.Add(-time.Second).Format(time.RFC3339Nano)
			}
			svc := NewTaskService(q, pool, nil, events.New())
			parent, transitioned, err := svc.FailUnstartedLauncherClaim(ctx, util.MustParseUUID(taskID), claim)
			if err != nil {
				t.Fatal(err)
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1`, taskID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if observed {
				if transitioned || count != 0 || parent.Status != cols["status"] {
					t.Fatalf("observed/stale claim replayed: transitioned=%v children=%d status=%s", transitioned, count, parent.Status)
				}
				return
			}
			if !transitioned || parent.Status != "failed" || parent.FailureReason.String != "runtime_offline" || count != 1 {
				t.Fatalf("recovery not atomic: parent=%+v transitioned=%v children=%d", parent, transitioned, count)
			}
			var child db.AgentTaskQueue
			var childID string
			if err := pool.QueryRow(ctx, `SELECT id::text FROM agent_task_queue WHERE parent_task_id=$1`, taskID).Scan(&childID); err != nil {
				t.Fatal(err)
			}
			child, err = q.GetAgentTask(ctx, util.MustParseUUID(childID))
			if err != nil {
				t.Fatal(err)
			}
			if string(child.Context) != string(parent.Context) || child.HandoffNote != parent.HandoffNote || child.TriggerSummary != parent.TriggerSummary || child.OriginatorUserID != parent.OriginatorUserID || child.AccountableUserID != parent.AccountableUserID || child.OriginatorSource != parent.OriginatorSource || child.Attempt != 1 {
				t.Fatalf("unstarted outcome changed: %+v", child)
			}
			if mode == "healthy-peer" || mode == "source-successor" {
				if util.UUIDToString(child.AgentID) != peerAgent || util.UUIDToString(child.RuntimeID) != peerRuntime || child.Status != "queued" || !child.ForceFreshSession {
					t.Fatalf("not routed to healthy same-model host: %+v", child)
				}
				claimed, err := q.ClaimAgentTask(ctx, db.ClaimAgentTaskParams{AgentID: child.AgentID, RuntimeID: child.RuntimeID, PrepareLeaseSecs: 60, RuntimeStaleSecs: RuntimeClaimFreshnessSeconds})
				if err != nil || claimed.ID != child.ID {
					t.Fatalf("peer cannot claim preserved outcome: %v", err)
				}
			} else if child.AgentID != parent.AgentID || child.RuntimeID != parent.RuntimeID || child.Status != "deferred" {
				t.Fatalf("no eligible peer must preserve deferred outcome: %+v", child)
			}
			if mode == "no-peer" {
				if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET fire_at=now()-interval '1 second' WHERE id=$1`, childID); err != nil {
					t.Fatal(err)
				}
				promoted, err := q.PromoteDueDeferredTasksForRuntimes(ctx, db.PromoteDueDeferredTasksForRuntimesParams{RuntimeIds: []pgtype.UUID{parent.RuntimeID}, RuntimeStaleSecs: RuntimeClaimFreshnessSeconds})
				if err != nil || len(promoted) != 0 {
					t.Fatalf("elapsed time restored unhealthy launcher: rows=%d err=%v", len(promoted), err)
				}
			}
			// Heartbeats and independent enqueues cannot clear the launcher gate.
			freshIssue := fx.Issue(t, "independent fresh enqueue")
			fx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": freshIssue, "status": "queued"})
			if _, err := q.MarkAgentRuntimeOnline(ctx, parent.RuntimeID); err != nil {
				t.Fatal(err)
			}
			_, err = q.ClaimAgentTask(ctx, db.ClaimAgentTaskParams{AgentID: parent.AgentID, RuntimeID: parent.RuntimeID, PrepareLeaseSecs: 60, RuntimeStaleSecs: RuntimeClaimFreshnessSeconds})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("source launcher re-entered dispatch: %v", err)
			}
			_, again, err := svc.FailUnstartedLauncherClaim(ctx, util.MustParseUUID(taskID), claim)
			if err != nil || again {
				t.Fatalf("callback replay: transitioned=%v err=%v", again, err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1`, taskID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("callback duplicated outcome: count=%d err=%v", count, err)
			}
			if mode == "no-peer" {
				rt, err := q.GetAgentRuntime(ctx, parent.RuntimeID)
				if err != nil {
					t.Fatal(err)
				}
				_, err = q.UpsertAgentRuntime(ctx, db.UpsertAgentRuntimeParams{WorkspaceID: rt.WorkspaceID, DaemonID: rt.DaemonID, Name: rt.Name, RuntimeMode: rt.RuntimeMode, Provider: rt.Provider, Status: "online", DeviceInfo: rt.DeviceInfo, Metadata: []byte(`{"version":"1.0.1"}`), OwnerID: rt.OwnerID, RequiredOwnerID: rt.OwnerID})
				if err != nil {
					t.Fatal(err)
				}
				rows, err := q.PromoteDueDeferredTasksForRuntimes(ctx, db.PromoteDueDeferredTasksForRuntimesParams{RuntimeIds: []pgtype.UUID{parent.RuntimeID}, RuntimeStaleSecs: RuntimeClaimFreshnessSeconds})
				if err != nil || len(rows) != 1 || rows[0].ID != child.ID {
					t.Fatalf("positive health did not restore preserved deferred outcome: rows=%d err=%v", len(rows), err)
				}
			}
		})
	}
}
