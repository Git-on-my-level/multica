package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

func TestCursorConnectionStalledRetriesWithSession(t *testing.T) {
	t.Parallel()
	const raw = "cursor-agent exited with error: exit status 1 (result_seen=false, exit_code=1, scanner_error=false, event_count=463, invalid_event_count=0, last_event_type=system); actions completed before finalization may already have taken effect; cursor stderr: RetriableError: Connection stalled"
	for _, reported := range []string{"", "agent_error.process_failure", "agent_error.unknown", "agent_error"} {
		t.Run(reported, func(t *testing.T) {
			reason := reported
			if reason == "" {
				reason = taskfailure.Classify(raw).String()
			}
			reason = taskfailure.NormalizeDaemonReason(reason, raw).String()
			if reason != taskfailure.ReasonAgentProviderNetwork.String() {
				t.Fatalf("resolved reason = %q, want provider_network", reason)
			}
			if resumeUnsafeFailureReason(reason) {
				t.Fatal("stalled transport must preserve the session for retry")
			}
			for _, tc := range []struct {
				name         string
				attempt, max int32
				want         bool
			}{
				{"first failure", 1, 2, true},
				{"second failure", 2, 2, true},
				{"retry ceiling", providerNetworkMaxAttempts, 2, false},
				{"explicitly disabled", 1, 1, false},
			} {
				task := db.AgentTaskQueue{
					Attempt: tc.attempt, MaxAttempts: tc.max,
					IssueID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
				}
				if got := retryEligible(reason, task); got != tc.want {
					t.Errorf("%s: retryEligible = %v, want %v", tc.name, got, tc.want)
				}
			}
		})
	}
}

const cursorStartupFailure = "cursor-agent exited with error: exit status 1 (result_seen=false, exit_code=1, scanner_error=false, event_count=14, invalid_event_count=0, last_event_type=connection); actions completed before finalization may already have taken effect; cursor stderr: RetriableError: [unavailable] Error"

func TestCursorStartupRetryPolicy(t *testing.T) {
	t.Parallel()
	for _, witness := range []string{cursorStartupFailure, strings.ReplaceAll(cursorStartupFailure, "RetriableError: [unavailable] Error", "Error: getaddrinfo ENOTFOUND api2.cursor.sh")} {
		for _, tc := range []struct {
			name   string
			change func(*db.AgentTaskQueue)
			reason string
			want   bool
		}{
			{"issue", func(t *db.AgentTaskQueue) {}, "", true},
			{"chat", func(t *db.AgentTaskQueue) { t.ChatSessionID = t.IssueID; t.IssueID = pgtype.UUID{} }, "", true},
			{"second attempt", func(t *db.AgentTaskQueue) { t.Attempt = 2 }, "", false},
			{"larger budget still one recovery", func(t *db.AgentTaskQueue) { t.Attempt = 2; t.MaxAttempts = 5 }, "", false},
			{"disabled", func(t *db.AgentTaskQueue) { t.MaxAttempts = 1 }, "", false},
			{"exhausted", func(t *db.AgentTaskQueue) { t.Attempt = 5; t.MaxAttempts = 5 }, "", false},
			{"missing session", func(t *db.AgentTaskQueue) { t.SessionID = pgtype.Text{} }, "", false},
			{"empty session", func(t *db.AgentTaskQueue) { t.SessionID.String = " " }, "", false},
			{"fresh session", func(t *db.AgentTaskQueue) { t.ForceFreshSession = true }, "", false},
			{"missing rollout", func(t *db.AgentTaskQueue) { t.SessionRolloutMissing = true }, "", false},
			{"retired session", func(t *db.AgentTaskQueue) { t.RetiredSessionID = t.SessionID }, "", false},
			{"autopilot", func(t *db.AgentTaskQueue) { t.AutopilotRunID = t.IssueID }, "", false},
			{"triage", func(t *db.AgentTaskQueue) { t.Context = []byte(`{"type":"triage"}`) }, "", false},
			{"cancelled", func(t *db.AgentTaskQueue) { t.Status = "cancelled" }, "", false},
			{"cancel actor", func(t *db.AgentTaskQueue) { t.CancelledByType = pgtype.Text{String: "member", Valid: true} }, "", false},
			{"unsafe reason", func(t *db.AgentTaskQueue) {}, "codex_semantic_inactivity", false},
			{"auth reason", func(t *db.AgentTaskQueue) {}, taskfailure.ReasonAgentProviderAuthOrAccess.String(), false},
			{"model reason", func(t *db.AgentTaskQueue) {}, taskfailure.ReasonAgentModelNotFoundOrUnavailable.String(), false},
			{"unlinked", func(t *db.AgentTaskQueue) { t.IssueID = pgtype.UUID{} }, "", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				task := db.AgentTaskQueue{Attempt: 1, MaxAttempts: 2, Status: "running", IssueID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true}, SessionID: pgtype.Text{String: "existing-cursor-session", Valid: true}, Error: pgtype.Text{String: witness, Valid: true}}
				tc.change(&task)
				reason := tc.reason
				if reason == "" {
					reason = taskfailure.NormalizeDaemonReason(taskfailure.ReasonAgentProcessFailure.String(), witness).String()
				}
				if got := retryEligible(reason, task); got != tc.want {
					t.Fatalf("retryEligible=%v want %v", got, tc.want)
				}
				if tc.want {
					if taskRetryDelay(reason, task) != 5*time.Second {
						t.Fatal("recovery must be deferred 5 seconds")
					}
					if taskRetryAttemptCeiling(reason, task) != 2 {
						t.Fatal("recovery must not raise attempt budget")
					}
				}
			})
		}
	}
}

func TestCursorStartupSweeperDoesNotReplay(t *testing.T) {
	t.Parallel()
	svc := &TaskService{}
	parent := db.AgentTaskQueue{Status: "failed", Attempt: 1, MaxAttempts: 2, Error: pgtype.Text{String: cursorStartupFailure, Valid: true}, FailureReason: pgtype.Text{String: taskfailure.ReasonAgentProviderNetwork.String(), Valid: true}}
	child, err := svc.MaybeRetryFailedTask(context.Background(), parent)
	if err != nil || child != nil {
		t.Fatalf("sweeper must not duplicate atomic startup recovery: child=%v err=%v", child, err)
	}
}

// Exercise the actual failure transaction, session inheritance, delayed queue,
// idempotent reporting, and the sweeper with isolated fixture rows.
func TestCursorStartupFailTaskRecovery(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	q := db.New(pool)
	ctx := context.Background()
	for _, tc := range []struct {
		name            string
		incomingSession bool
		dns             bool
		max             int32
		want            bool
	}{
		{"connection pinned session", false, false, 2, true},
		{"connection incoming session", true, false, 2, true},
		{"dns pinned session", false, true, 2, true},
		{"dns incoming resumed session", true, true, 2, true},
		{"disabled budget", false, false, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bootstrap := testutil.New(pool, "", "")
			suffix := time.Now().UnixNano()
			userID := bootstrap.User(t, "cursor-retry", fmt.Sprintf("cursor-retry-%d@example.com", suffix))
			workspaceID := bootstrap.Workspace(t, "Cursor startup retry", fmt.Sprintf("cursor-retry-%d", suffix))
			fx := testutil.New(pool, workspaceID, userID)
			fx.Member(t, workspaceID, userID, "owner")
			runtimeID := fx.Runtime(t, "Cursor test runtime")
			agentID := fx.Agent(t, "Cursor test agent", runtimeID)
			issueID := fx.Issue(t, "Cursor startup failure")
			cols := testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "attempt": 1, "max_attempts": tc.max, "work_dir": "/test/cursor-work"}
			if !tc.incomingSession {
				cols["session_id"] = "existing-cursor-session"
			}
			parentID := fx.Task(t, agentID, cols)
			fx.Cleanup(t, `DELETE FROM agent_task_queue WHERE parent_task_id=$1`, parentID)
			raw := cursorStartupFailure
			if tc.dns {
				raw = strings.ReplaceAll(raw, "RetriableError: [unavailable] Error", "Error: getaddrinfo ENOTFOUND api2.cursor.sh")
			}
			session := ""
			if tc.incomingSession {
				session = "existing-cursor-session"
			}
			svc := NewTaskService(q, pool, nil, events.New())
			before := time.Now()
			for i := 0; i < 2; i++ {
				if _, err := svc.FailTask(ctx, util.MustParseUUID(parentID), raw, session, "", "", "agent_error.process_failure", false, "", ""); err != nil {
					t.Fatal(err)
				}
			}
			parent, err := q.GetAgentTask(ctx, util.MustParseUUID(parentID))
			if err != nil {
				t.Fatal(err)
			}
			if parent.Status != "failed" || parent.FailureReason.String != taskfailure.ReasonAgentProviderNetwork.String() {
				t.Fatalf("parent failure lost: %+v", parent)
			}
			if child, err := svc.MaybeRetryFailedTask(ctx, parent); err != nil || child != nil {
				t.Fatalf("sweeper duplicated recovery: %v %v", child, err)
			}
			var count int
			fx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1`, parentID).Scan(&count)
			want := 0
			if tc.want {
				want = 1
			}
			if count != want {
				t.Fatalf("children=%d want %d", count, want)
			}
			if !tc.want {
				return
			}
			var childID pgtype.UUID
			fx.QueryRow(t, `SELECT id FROM agent_task_queue WHERE parent_task_id=$1`, parentID).Scan(&childID)
			child, err := q.GetAgentTask(ctx, childID)
			if err != nil {
				t.Fatal(err)
			}
			if child.Status != "deferred" || child.Attempt != 2 || child.MaxAttempts != tc.max || child.ForceFreshSession || child.SessionID.String != "existing-cursor-session" || child.WorkDir.String != "/test/cursor-work" {
				t.Fatalf("recovery lost budget/session/deferral: %+v", child)
			}
			if !child.FireAt.Valid || child.FireAt.Time.Before(before.Add(5*time.Second)) || child.FireAt.Time.After(time.Now().Add(5*time.Second)) {
				t.Fatalf("unexpected backoff: %+v", child.FireAt)
			}
			fx.Exec(t, `UPDATE agent_task_queue SET status='running' WHERE id=$1`, childID)
			if _, err := svc.FailTask(ctx, childID, raw, "", "", "", "agent_error.process_failure", false, "", ""); err != nil {
				t.Fatal(err)
			}
			fx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1`, childID).Scan(&count)
			if count != 0 {
				t.Fatalf("second failure created %d grandchildren", count)
			}
			// Explicitly cancelling the recovery must not resurrect its failed parent.
			fx.Exec(t, `UPDATE agent_task_queue SET status='cancelled' WHERE id=$1`, childID)
			if next, err := svc.MaybeRetryFailedTask(ctx, parent); err != nil || next != nil {
				t.Fatalf("cancelled recovery resurrected: %v %v", next, err)
			}
		})
	}
}
