package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestRetryEligibleRuntimeOffline(t *testing.T) {
	text := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	linked := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	for _, tc := range []struct {
		name string
		edit func(*db.AgentTaskQueue)
		want bool
	}{
		{"issue session", func(*db.AgentTaskQueue) {}, true},
		{"chat session", func(q *db.AgentTaskQueue) { q.IssueID = pgtype.UUID{}; q.ChatSessionID = linked }, true},
		{"larger budget is preserved", func(q *db.AgentTaskQueue) { q.MaxAttempts = 5 }, true},
		{"one recovery only", func(q *db.AgentTaskQueue) { q.Attempt = 2; q.MaxAttempts = 5 }, false},
		{"exhausted budget", func(q *db.AgentTaskQueue) { q.Attempt = 2 }, false},
		{"disabled budget", func(q *db.AgentTaskQueue) { q.MaxAttempts = 1 }, false},
		{"missing session", func(q *db.AgentTaskQueue) { q.SessionID = pgtype.Text{} }, false},
		{"empty session", func(q *db.AgentTaskQueue) { q.SessionID = text(" ") }, false},
		{"missing rollout", func(q *db.AgentTaskQueue) { q.SessionRolloutMissing = true }, false},
		{"retired session", func(q *db.AgentTaskQueue) { q.RetiredSessionID = q.SessionID }, false},
		{"different retired session", func(q *db.AgentTaskQueue) { q.RetiredSessionID = text("older") }, true},
		{"fresh session requested", func(q *db.AgentTaskQueue) { q.ForceFreshSession = true }, false},
		{"poisoned session", func(q *db.AgentTaskQueue) { q.FailureReason = text("agent_error.context_overflow") }, false},
		{"poisoned raw error", func(q *db.AgentTaskQueue) { q.Error = text("400 invalid_request_error") }, false},
		{"autopilot", func(q *db.AgentTaskQueue) { q.AutopilotRunID = linked }, false},
		{"triage", func(q *db.AgentTaskQueue) { q.Context = []byte(`{"type":"triage"}`) }, false},
		{"unlinked", func(q *db.AgentTaskQueue) { q.IssueID = pgtype.UUID{} }, false},
		{"quick create", func(q *db.AgentTaskQueue) {
			q.IssueID = pgtype.UUID{}
			q.Context = []byte(`{"type":"quick_create","source_context_id":"source"}`)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := db.AgentTaskQueue{Attempt: 1, MaxAttempts: 2, IssueID: linked, SessionID: text("session")}
			tc.edit(&q)
			if got := retryEligible("runtime_offline", q); got != tc.want {
				t.Fatalf("retryEligible = %v, want %v", got, tc.want)
			}
			if got := retryAttemptCeiling("runtime_offline", q.MaxAttempts); got != q.MaxAttempts {
				t.Fatalf("budget changed to %d from %d", got, q.MaxAttempts)
			}
		})
	}
	for _, reason := range []string{"cancelled", "agent_error.context_overflow", "agent_error.provider_auth"} {
		q := db.AgentTaskQueue{Attempt: 1, MaxAttempts: 2, IssueID: linked, SessionID: text("session")}
		if retryEligible(reason, q) {
			t.Errorf("%s unexpectedly retries", reason)
		}
	}
	if retryDelayForAttempt("runtime_offline", 1) <= 0 || resumeUnsafeFailureReason("runtime_offline") {
		t.Fatal("offline recovery must defer and preserve the session")
	}
}

func TestRetryEligibleRuntimeOfflineReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name             string
		chat, concurrent bool
	}{{"issue", false, false}, {"chat", true, false}, {"concurrent issue", false, true}, {"concurrent chat", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newResolveOriginatorPool(t)
			ctx := context.Background()
			q := db.New(pool)
			workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
			var runtimeID string
			if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id=$1`, agentID).Scan(&runtimeID); err != nil {
				t.Fatal(err)
			}
			var issue, chatID pgtype.UUID
			issue = util.MustParseUUID(issueID)
			if tc.chat {
				issue = pgtype.UUID{}
				if err := pool.QueryRow(ctx, `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title) VALUES ($1,$2,$3,'offline recovery') RETURNING id`, workspaceID, agentID, userID).Scan(&chatID); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM chat_session WHERE id=$1`, chatID) })
			}
			var parentID pgtype.UUID
			if err := pool.QueryRow(ctx, `INSERT INTO agent_task_queue (agent_id,runtime_id,issue_id,chat_session_id,status,attempt,max_attempts,session_id,work_dir) VALUES ($1,$2,$3,$4,'running',1,5,'existing-session','/existing-workdir') RETURNING id`, agentID, runtimeID, issue, chatID).Scan(&parentID); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id=$1 OR parent_task_id=$1`, parentID)
			})
			if _, err := pool.Exec(ctx, `UPDATE agent_runtime SET status='offline',last_seen_at=now()-interval '1 hour',updated_at=now()-interval '1 hour' WHERE id=$1`, runtimeID); err != nil {
				t.Fatal(err)
			}
			svc := NewTaskService(q, pool, nil, events.New())
			failed, err := svc.FailTasksForOfflineRuntimes(ctx, db.FailTasksForOfflineRuntimesParams{ReconnectGraceSecs: 60, OfflineConfirmSecs: 30, MaxPerTick: 100})
			if err != nil {
				t.Fatal(err)
			}
			var parent *db.AgentTaskQueue
			for i := range failed {
				if failed[i].ID == parentID {
					parent = &failed[i]
				}
			}
			if parent == nil {
				t.Fatal("reconciliation did not fail the interrupted attempt")
			}
			if tc.concurrent {
				type result struct {
					child *db.AgentTaskQueue
					err   error
				}
				results := make(chan result, 4)
				start := make(chan struct{})
				for i := 0; i < 4; i++ {
					go func() {
						<-start
						child, err := svc.MaybeRetryFailedTask(ctx, *parent)
						results <- result{child, err}
					}()
				}
				close(start)
				created := 0
				for i := 0; i < 4; i++ {
					r := <-results
					if r.err != nil {
						t.Error(r.err)
					}
					if r.child != nil {
						created++
					}
				}
				if created != 1 {
					t.Fatalf("concurrent recoveries = %d, want 1", created)
				}
			} else if got := svc.HandleFailedTasks(ctx, []db.AgentTaskQueue{*parent}); got != 1 {
				t.Fatalf("retried = %d, want 1", got)
			}
			if _, err := svc.MaybeRetryFailedTask(ctx, *parent); err != nil {
				t.Fatal(err)
			}
			var childID pgtype.UUID
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1`, parentID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("children = %d, want 1", count)
			}
			if err := pool.QueryRow(ctx, `SELECT id FROM agent_task_queue WHERE parent_task_id=$1`, parentID).Scan(&childID); err != nil {
				t.Fatal(err)
			}
			child, err := q.GetAgentTask(ctx, childID)
			if err != nil {
				t.Fatal(err)
			}
			if child.Status != "deferred" || !child.FireAt.Valid || child.Attempt != 2 || child.MaxAttempts != 5 || child.SessionID != parent.SessionID || child.WorkDir != parent.WorkDir || child.RuntimeID != parent.RuntimeID || child.ForceFreshSession {
				t.Fatalf("retry did not preserve deferred session/budget: %+v", child)
			}
			if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET fire_at=now()-interval '1 second' WHERE id=$1`, childID); err != nil {
				t.Fatal(err)
			}
			claimed, err := svc.ClaimTaskForRuntime(ctx, util.MustParseUUID(runtimeID))
			if err != nil || claimed != nil {
				t.Fatalf("offline claim = %v, %v", claimed, err)
			}
			if _, err := pool.Exec(ctx, `UPDATE agent_runtime SET status='online',last_seen_at=now(),updated_at=now() WHERE id=$1`, runtimeID); err != nil {
				t.Fatal(err)
			}
			claimed, err = svc.ClaimTaskForRuntime(ctx, util.MustParseUUID(runtimeID))
			if err != nil || claimed == nil || claimed.ID != childID {
				t.Fatalf("reconnected claim = %v, %v", claimed, err)
			}
			if _, err := svc.FailTask(ctx, childID, "runtime went offline", "existing-session", "/existing-workdir", "", "runtime_offline", false, "", ""); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1`, childID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatal("second offline failure exceeded the one-recovery limit")
			}
			if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status='cancelled' WHERE id=$1`, childID); err != nil {
				t.Fatal(err)
			}
			if retry, err := svc.MaybeRetryFailedTask(ctx, *parent); err != nil || retry != nil {
				t.Fatalf("cancelled child was replaced: %v, %v", retry, err)
			}
		})
	}
}

func TestRetryEligibleRuntimeOfflineFailureReport(t *testing.T) {
	for _, tc := range []struct {
		name, message, retired string
		missing, want          bool
	}{
		{"first session pointer", "runtime went offline", "", false, true},
		{"missing rollout", "runtime went offline", "", true, false},
		{"retired session", "runtime went offline", "reported-session", false, false},
		{"unsafe report", "400 invalid_request_error", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newResolveOriginatorPool(t)
			ctx := context.Background()
			q := db.New(pool)
			_, _, agentID, issueID := seedAttributionFixture(t, pool)
			var parentID pgtype.UUID
			if err := pool.QueryRow(ctx, `INSERT INTO agent_task_queue (agent_id,runtime_id,issue_id,status,attempt,max_attempts) SELECT id,runtime_id,$2,'running',1,2 FROM agent WHERE id=$1 RETURNING id`, agentID, issueID).Scan(&parentID); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id=$1 OR parent_task_id=$1`, parentID)
			})
			svc := NewTaskService(q, pool, nil, events.New())
			if _, err := svc.FailTask(ctx, parentID, tc.message, "reported-session", "/reported-workdir", "", "runtime_offline", tc.missing, tc.retired, ""); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1`, parentID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.want {
				want = 1
			}
			if count != want {
				t.Fatalf("recovery children = %d, want %d", count, want)
			}
		})
	}
}
