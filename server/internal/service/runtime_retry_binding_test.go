package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A retry survives a change to the agent's runtime binding. A rebound agent's
// retry goes to the new runtime, which is the only one its claim fence admits
// (#10717); an unbound agent's retry keeps the parent's runtime until the agent
// is bound again.
func TestRetryPreservesChildAcrossRuntimeBindingChanges(t *testing.T) {
	for _, tt := range []struct {
		name           string
		atomicFailure  bool
		unbound        bool
		shutdown       bool
		moveAfterRetry bool
	}{
		{name: "FailTask/rebound", atomicFailure: true},
		{name: "FailTask/unbound", atomicFailure: true, unbound: true},
		{name: "MaybeRetryFailedTask/rebound"},
		{name: "MaybeRetryFailedTask/unbound", unbound: true},
		{name: "FailTask/shutdown after rebind", atomicFailure: true, shutdown: true},
		{name: "MaybeRetryFailedTask/shutdown after rebind", shutdown: true},
		{name: "FailTask/shutdown before rebind", atomicFailure: true, shutdown: true, moveAfterRetry: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newResolveOriginatorPool(t)
			q := db.New(pool)
			bootstrap := testutil.New(pool, "", "")
			suffix := time.Now().UnixNano()
			userID := bootstrap.User(t, "retry-owner", fmt.Sprintf("retry-owner-%d@example.com", suffix))
			workspaceID := bootstrap.Workspace(t, "runtime retry binding", fmt.Sprintf("runtime-retry-%d", suffix))
			fx := testutil.New(pool, workspaceID, userID)
			fx.Member(t, workspaceID, userID, "owner")
			runtimeA := fx.Runtime(t, "original runtime", testutil.Cols{"metadata": sameSessionRetryCapabilities})
			runtimeB := fx.Runtime(t, "replacement runtime", testutil.Cols{"metadata": sameSessionRetryCapabilities})
			agentID := fx.Agent(t, "retry agent", runtimeA)
			issueID := fx.Issue(t, "retry after runtime binding change")
			status := "failed"
			if tt.atomicFailure {
				status = "running"
			}
			reason, failureMessage := "timeout", "runtime timed out"
			if tt.shutdown {
				reason, failureMessage = "runtime_recovery", "daemon shut down while task was in flight"
			}
			parentID := fx.Task(t, agentID, testutil.Cols{
				"runtime_id":     runtimeA,
				"issue_id":       issueID,
				"status":         status,
				"attempt":        1,
				"max_attempts":   2,
				"failure_reason": reason,
				"error":          failureMessage,
				"session_id":     "existing-session",
			})
			fx.Cleanup(t, `DELETE FROM agent_task_queue WHERE parent_task_id = $1`, parentID)

			// Both private runtimes belong to this agent's owner, so the rebind
			// is legal. Unbinding is the other state the enqueue guard rejected.
			var nextRuntime any = runtimeB
			if tt.unbound {
				nextRuntime = nil
			}
			if !tt.moveAfterRetry {
				fx.Exec(t, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, nextRuntime, agentID)
			}
			svc := NewTaskService(q, pool, nil, events.New())
			parentUUID := util.MustParseUUID(parentID)
			triggerRetry := func() {
				t.Helper()
				if tt.atomicFailure {
					if _, err := svc.FailTask(ctx, parentUUID, failureMessage, "", "", "", reason, false, "", ""); err != nil {
						t.Fatalf("FailTask: %v", err)
					}
					return
				}
				parent, err := q.GetAgentTask(ctx, parentUUID)
				if err != nil {
					t.Fatalf("load failed parent: %v", err)
				}
				if _, err := svc.MaybeRetryFailedTask(ctx, parent); err != nil {
					t.Fatalf("MaybeRetryFailedTask: %v", err)
				}
			}
			triggerRetry()
			triggerRetry()

			var childID pgtype.UUID
			var children int
			fx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id = $1`, parentID).Scan(&children)
			if tt.shutdown && !tt.moveAfterRetry {
				if children != 0 {
					t.Fatalf("shutdown recovery created %d children after the agent moved", children)
				}
				return
			}
			if children != 1 {
				t.Fatalf("retry children = %d, want exactly one after repeated failure/recovery", children)
			}
			fx.QueryRow(t, `SELECT id FROM agent_task_queue WHERE parent_task_id = $1`, parentID).Scan(&childID)
			child, err := q.GetAgentTask(ctx, childID)
			if err != nil {
				t.Fatalf("load retry child: %v", err)
			}
			if tt.moveAfterRetry {
				if child.Status != "deferred" || child.SessionID.String != "existing-session" || child.RuntimeID != util.MustParseUUID(runtimeA) {
					t.Fatalf("shutdown retry did not preserve its original session and runtime: %+v", child)
				}
				fx.Exec(t, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, runtimeB, agentID)
				if result, err := q.ReassignPendingAgentTasksToRuntime(ctx, db.ReassignPendingAgentTasksToRuntimeParams{
					AgentID: util.MustParseUUID(agentID), NewRuntimeID: util.MustParseUUID(runtimeB),
				}); err != nil || !result.FenceOk || result.ReassignedTasks != 1 {
					t.Fatalf("reassign pending tasks = %+v, %v", result, err)
				}
				child, err = q.GetAgentTask(ctx, childID)
				if err != nil || child.Status != "deferred" || child.RuntimeID != util.MustParseUUID(runtimeB) || child.SessionID.Valid {
					t.Fatalf("rebound recovery must be awaiting settlement without a reusable session: %+v, %v", child, err)
				}
				if required, pinned := SameSessionRetry(child); !required || pinned != "" {
					t.Fatalf("rebound recovery was not fenced at claim: %v, %q", required, pinned)
				}
				return
			}
			wantRuntime := runtimeB
			if tt.unbound {
				wantRuntime = runtimeA
			}
			if child.RuntimeID != util.MustParseUUID(wantRuntime) || child.Status != "queued" || child.Attempt != 2 {
				t.Fatalf("retry child = runtime:%s status:%q attempt:%d, want runtime %s, queued, attempt 2",
					util.UUIDToString(child.RuntimeID), child.Status, child.Attempt, wantRuntime)
			}
			var parentStatus string
			fx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, parentID).Scan(&parentStatus)
			if parentStatus != "failed" {
				t.Fatalf("parent status = %q, want failed", parentStatus)
			}

			claimOn := func(runtimeID string) db.ClaimAgentTaskParams {
				return db.ClaimAgentTaskParams{
					AgentID:          util.MustParseUUID(agentID),
					RuntimeID:        util.MustParseUUID(runtimeID),
					PrepareLeaseSecs: 60,
					RuntimeStaleSecs: RuntimeClaimFreshnessSeconds,
				}
			}
			if _, err := q.ClaimAgentTask(ctx, claimOn(runtimeA)); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("claim on old runtime with changed binding = %v, want no rows", err)
			}
			var childStatus string
			fx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, childID).Scan(&childStatus)
			if childStatus != "queued" {
				t.Fatalf("rejected claim changed retry status to %q, want queued", childStatus)
			}

			if tt.unbound {
				fx.Exec(t, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, runtimeA, agentID)
			}
			claimed, err := q.ClaimAgentTask(ctx, claimOn(wantRuntime))
			if err != nil {
				t.Fatalf("claim retry on runtime %s: %v", wantRuntime, err)
			}
			if claimed.ID != childID {
				t.Fatalf("claimed task = %s, want existing retry %s", util.UUIDToString(claimed.ID), util.UUIDToString(childID))
			}
		})
	}
}
