package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestRetryEligibleDaemonShutdownBudget(t *testing.T) {
	for _, tc := range []struct {
		reason       string
		attempt, max int32
		want         bool
	}{
		{"runtime_recovery", 1, 2, true},
		{"runtime_recovery", 2, 2, false},
		{"runtime_recovery", 1, 1, false},
		{"cancelled", 1, 2, false},
	} {
		task := db.AgentTaskQueue{Attempt: tc.attempt, MaxAttempts: tc.max, IssueID: pgtype.UUID{Valid: true}, SessionID: pgtype.Text{String: "existing-session", Valid: true}, Error: pgtype.Text{String: "daemon shut down while task was in flight", Valid: true}}
		if got := retryEligible(tc.reason, task); got != tc.want {
			t.Errorf("retryEligible(%s, %d/%d) = %v, want %v", tc.reason, tc.attempt, tc.max, got, tc.want)
		}
		if ceiling := retryAttemptCeiling(tc.reason, tc.max); ceiling != tc.max {
			t.Errorf("%s widened budget to %d", tc.reason, ceiling)
		}
	}
	if resumeUnsafeFailureReason("runtime_recovery") {
		t.Fatal("shutdown must preserve the resumable session")
	}
}

func TestRetryEligibleDaemonShutdown(t *testing.T) {
	base := db.AgentTaskQueue{Attempt: 1, MaxAttempts: 2, IssueID: pgtype.UUID{Valid: true}, SessionID: pgtype.Text{String: "existing-session", Valid: true}, Error: pgtype.Text{String: "daemon shut down while task was in flight", Valid: true}}
	for _, tc := range []struct {
		name   string
		change func(*db.AgentTaskQueue)
		want   bool
	}{
		{"issue", func(*db.AgentTaskQueue) {}, true},
		{"chat", func(t *db.AgentTaskQueue) { t.IssueID.Valid = false; t.ChatSessionID.Valid = true }, true},
		{"missing session", func(t *db.AgentTaskQueue) { t.SessionID.Valid = false }, false},
		{"empty session", func(t *db.AgentTaskQueue) { t.SessionID.String = " " }, false},
		{"rollout missing", func(t *db.AgentTaskQueue) { t.SessionRolloutMissing = true }, false},
		{"fresh session", func(t *db.AgentTaskQueue) { t.ForceFreshSession = true }, false},
		{"ordinary manual rerun", func(t *db.AgentTaskQueue) { t.ForceFreshSession = true; t.RerunOfTaskID.Valid = true }, true},
		{"explicit fresh rerun", func(t *db.AgentTaskQueue) {
			t.ForceFreshSession = true
			t.RerunOfTaskID.Valid = true
			t.Context = []byte(`{"fresh_rerun":true}`)
		}, false},
		{"retired session", func(t *db.AgentTaskQueue) { t.RetiredSessionID = t.SessionID }, false},
		{"poisoned history", func(t *db.AgentTaskQueue) {
			t.Error = pgtype.Text{String: "daemon shut down while task was in flight; 400 invalid_request_error", Valid: true}
		}, false},
		{"exhausted budget", func(t *db.AgentTaskQueue) { t.Attempt = 2 }, false},
		{"disabled budget", func(t *db.AgentTaskQueue) { t.MaxAttempts = 1 }, false},
		{"one recovery even with larger budget", func(t *db.AgentTaskQueue) { t.Attempt = 2; t.MaxAttempts = 5 }, false},
		{"autopilot", func(t *db.AgentTaskQueue) { t.AutopilotRunID.Valid = true }, false},
		{"triage", func(t *db.AgentTaskQueue) { t.Context = []byte(`{"type":"triage"}`) }, false},
		{"unlinked", func(t *db.AgentTaskQueue) { t.IssueID.Valid = false }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := base
			tc.change(&task)
			if got := retryEligible("runtime_recovery", task); got != tc.want {
				t.Fatalf("eligible = %v, want %v", got, tc.want)
			}
		})
	}
	if delay := retryDelayForAttempt("runtime_recovery", 1); delay <= 0 {
		t.Fatal("daemon-wide recovery must be deferred")
	}
	for _, reason := range []string{"cancelled", "agent_error.unknown", "agent_error.provider_auth"} {
		if retryEligible(reason, base) {
			t.Fatalf("unexpected retry for %s", reason)
		}
	}
}

func TestRetryEligibleDaemonShutdownTransitions(t *testing.T) {
	for _, callback := range []bool{true, false} {
		for _, chat := range []bool{false, true} {
			t.Run(fmt.Sprintf("callback=%v/chat=%v", callback, chat), func(t *testing.T) {
				ctx := context.Background()
				pool := sharedTestPool(t)
				bootstrap := testutil.New(pool, "", "")
				suffix := time.Now().UnixNano()
				user := bootstrap.User(t, "shutdown-owner", fmt.Sprintf("shutdown-%d@example.com", suffix))
				workspace := bootstrap.Workspace(t, "shutdown recovery", fmt.Sprintf("shutdown-%d", suffix))
				fx := testutil.New(pool, workspace, user)
				fx.Member(t, workspace, user, "owner")
				runtime := fx.Runtime(t, "shutdown runtime")
				agent := fx.Agent(t, "shutdown agent", runtime)
				cols := testutil.Cols{"runtime_id": runtime, "status": "running", "attempt": 1, "max_attempts": 2}
				if chat {
					cols["chat_session_id"] = fx.ChatSession(t, agent)
				} else {
					cols["issue_id"] = fx.Issue(t, "interrupted work")
				}
				parent := fx.Task(t, agent, cols)
				fx.Cleanup(t, `DELETE FROM agent_task_queue WHERE parent_task_id = $1`, parent)
				q := db.New(pool)
				svc := NewTaskService(q, pool, nil, events.New())
				parentUUID := util.MustParseUUID(parent)
				if callback {
					if _, err := svc.FailTask(ctx, parentUUID, "daemon shut down while task was in flight", "existing-session", "/tmp/existing-work", "", "runtime_recovery", false, "", ""); err != nil {
						t.Fatal(err)
					}
				} else {
					fx.Exec(t, `UPDATE agent_task_queue SET session_id = 'existing-session', work_dir = '/tmp/existing-work' WHERE id = $1`, parentUUID)
					lost, err := svc.RecoverOrphanedTasksForRuntime(ctx, util.MustParseUUID(runtime))
					if err != nil || len(lost) != 1 {
						t.Fatalf("recover orphan: %v, %v", lost, err)
					}
					if child, err := svc.MaybeRetryFailedTask(ctx, lost[0]); err != nil || child == nil {
						t.Fatalf("recover interrupted attempt: %v, %v", child, err)
					}
				}
				failed, err := q.GetAgentTask(ctx, parentUUID)
				if err != nil {
					t.Fatal(err)
				}
				var wg sync.WaitGroup
				for i := 0; i < 2; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if child, err := svc.MaybeRetryFailedTask(ctx, failed); err != nil || child != nil {
							t.Errorf("repeated recovery = %v, %v", child, err)
						}
					}()
				}
				wg.Wait()
				var count int
				fx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id = $1`, parent).Scan(&count)
				if count != 1 {
					t.Fatalf("children = %d, want 1", count)
				}
				var childID pgtype.UUID
				fx.QueryRow(t, `SELECT id FROM agent_task_queue WHERE parent_task_id = $1`, parent).Scan(&childID)
				child, err := q.GetAgentTask(ctx, childID)
				if err != nil {
					t.Fatal(err)
				}
				wantStatus := "queued"
				if callback {
					wantStatus = "deferred"
				}
				if child.Status != wantStatus || child.FireAt.Valid != callback || child.Attempt != 2 || child.MaxAttempts != 2 || child.SessionID.String != "existing-session" || child.WorkDir.String != "/tmp/existing-work" || child.ForceFreshSession || child.RuntimeID != failed.RuntimeID {
					t.Fatalf("invalid recovery child: %+v", child)
				}
				if callback {
					// A healthy runtime wins the expiry race. If it remains offline,
					// the deferred recovery must terminate after reconnect grace.
					fx.Exec(t, `UPDATE agent_task_queue SET fire_at = now() - interval '2 hours' WHERE id = $1`, childID)
					fx.Exec(t, `UPDATE agent_runtime SET status = 'online', last_seen_at = now() WHERE id = $1`, runtime)
					expiry := db.FailExpiredRuntimeReconnectRetriesParams{ReconnectGraceSecs: 60, RuntimeStaleSecs: 30, MaxPerTick: 100}
					if _, err := svc.FailExpiredRuntimeReconnectRetries(ctx, expiry); err != nil {
						t.Fatal(err)
					}
					current, err := q.GetAgentTask(ctx, childID)
					if err != nil || current.Status != "deferred" {
						t.Fatalf("healthy runtime lost recovery: %v, %v", current.Status, err)
					}
					fx.Exec(t, `UPDATE agent_runtime SET status = 'offline', last_seen_at = now() - interval '2 hours' WHERE id = $1`, runtime)
					if _, err := svc.FailExpiredRuntimeReconnectRetries(ctx, expiry); err != nil {
						t.Fatal(err)
					}
					current, err = q.GetAgentTask(ctx, childID)
					if err != nil || current.Status != "failed" || current.FailureReason.String != "runtime_reconnect_timeout" {
						t.Fatalf("offline recovery did not expire: %v, %v, %v", current.Status, current.FailureReason, err)
					}
					if retry, err := svc.MaybeRetryFailedTask(ctx, current); err != nil || retry != nil {
						t.Fatalf("expired recovery retried: %v, %v", retry, err)
					}
				}
				// Explicitly cancelling the child consumes recovery, including for chat.
				fx.Exec(t, `UPDATE agent_task_queue SET status = 'cancelled' WHERE id = $1`, childID)
				if retry, err := svc.MaybeRetryFailedTask(ctx, failed); err != nil || retry != nil {
					t.Fatalf("cancelled recovery recreated: %v, %v", retry, err)
				}
			})
		}
	}
}

func TestRetryEligibleDaemonShutdownPreservesStartupRecovery(t *testing.T) {
	for _, status := range []string{"dispatched", "waiting_local_directory", "running"} {
		task := db.AgentTaskQueue{Status: status, Attempt: 1, MaxAttempts: 2, IssueID: pgtype.UUID{Valid: true}, Error: pgtype.Text{String: "daemon restarted while task was in flight", Valid: true}}
		if !retryEligible("runtime_recovery", task) || retryDelayForTask("runtime_recovery", task) != 0 {
			t.Fatalf("startup recovery changed for %s", status)
		}
	}
}

func TestRetryEligibleDaemonShutdownChatOrdering(t *testing.T) {
	for _, release := range []string{"promote", "cancel", "expire", "startup"} {
		t.Run(release, func(t *testing.T) {
			ctx := context.Background()
			pool := sharedTestPool(t)
			bootstrap := testutil.New(pool, "", "")
			suffix := time.Now().UnixNano()
			user := bootstrap.User(t, "chat-recovery", fmt.Sprintf("chat-recovery-%d@example.com", suffix))
			workspace := bootstrap.Workspace(t, "chat recovery", fmt.Sprintf("chat-recovery-%d", suffix))
			fx := testutil.New(pool, workspace, user)
			fx.Member(t, workspace, user, "owner")
			runtime := fx.Runtime(t, "chat runtime", testutil.Cols{"status": "online", "last_seen_at": time.Now()})
			agent := fx.Agent(t, "chat recovery", runtime)
			chat := fx.ChatSession(t, agent)
			parent := fx.Task(t, agent, testutil.Cols{"runtime_id": runtime, "chat_session_id": chat, "status": "running", "attempt": 1, "max_attempts": 2})
			fx.Cleanup(t, `DELETE FROM agent_task_queue WHERE parent_task_id=$1`, parent)
			// The queued follow-up exists before the callback inserts its recovery child.
			followup := fx.Task(t, agent, testutil.Cols{"runtime_id": runtime, "chat_session_id": chat, "status": "queued", "priority": 4})
			q := db.New(pool)
			svc := NewTaskService(q, pool, nil, events.New())
			failureMessage, session := "daemon shut down while task was in flight", "existing-session"
			if release == "startup" {
				failureMessage, session = "daemon restarted while task was in flight", ""
			}
			if _, err := svc.FailTask(ctx, util.MustParseUUID(parent), failureMessage, session, "/tmp/existing-work", "", "runtime_recovery", false, "", ""); err != nil {
				t.Fatal(err)
			}
			var recovery pgtype.UUID
			if err := pool.QueryRow(ctx, `SELECT id FROM agent_task_queue WHERE parent_task_id=$1`, parent).Scan(&recovery); err != nil {
				t.Fatal(err)
			}
			params := db.ClaimAgentTaskParams{AgentID: util.MustParseUUID(agent), RuntimeID: util.MustParseUUID(runtime), PrepareLeaseSecs: 60, RuntimeStaleSecs: 30}
			if release == "startup" {
				got, err := q.ClaimAgentTask(ctx, params)
				if err != nil || got.ID != util.MustParseUUID(followup) {
					t.Fatalf("startup retry changed existing chat priority: %v, %v", got.ID, err)
				}
				return
			}
			fx.Exec(t, `UPDATE agent_task_queue SET fire_at=now()+interval '1 hour' WHERE id=$1`, recovery)
			if got, err := q.ClaimAgentTask(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("follow-up overtook deferred recovery: %v, %v", got.ID, err)
			}
			otherChat := fx.ChatSession(t, agent)
			other := fx.Task(t, agent, testutil.Cols{"runtime_id": runtime, "chat_session_id": otherChat, "status": "queued"})
			got, err := q.ClaimAgentTask(ctx, params)
			if err != nil || got.ID != util.MustParseUUID(other) {
				t.Fatalf("unrelated chat blocked: %v, %v", got.ID, err)
			}
			fx.Exec(t, `UPDATE agent_task_queue SET status='completed' WHERE id=$1`, other)
			switch release {
			case "promote":
				fx.Exec(t, `UPDATE agent_task_queue SET fire_at=now()-interval '1 second' WHERE id=$1`, recovery)
				if err := svc.PromoteDueDeferredTasksForRuntime(ctx, params.RuntimeID); err != nil {
					t.Fatal(err)
				}
				got, err = q.ClaimAgentTask(ctx, params)
				if err != nil || got.ID != recovery || got.SessionID.String != "existing-session" {
					t.Fatalf("recovery did not claim first: %v, %v", got.ID, err)
				}
				fx.Exec(t, `UPDATE agent_task_queue SET status='completed' WHERE id=$1`, recovery)
			case "cancel":
				fx.Exec(t, `UPDATE agent_task_queue SET status='cancelled' WHERE id=$1`, recovery)
			case "expire":
				fx.Exec(t, `UPDATE agent_task_queue SET fire_at=now()-interval '2 hours' WHERE id=$1`, recovery)
				fx.Exec(t, `UPDATE agent_runtime SET status='offline',last_seen_at=now()-interval '2 hours' WHERE id=$1`, runtime)
				if _, err := svc.FailExpiredRuntimeReconnectRetries(ctx, db.FailExpiredRuntimeReconnectRetriesParams{ReconnectGraceSecs: 60, RuntimeStaleSecs: 30, MaxPerTick: 100}); err != nil {
					t.Fatal(err)
				}
				fx.Exec(t, `UPDATE agent_runtime SET status='online',last_seen_at=now() WHERE id=$1`, runtime)
			}
			got, err = q.ClaimAgentTask(ctx, params)
			if err != nil || got.ID != util.MustParseUUID(followup) {
				t.Fatalf("follow-up remained blocked: %v, %v", got.ID, err)
			}
		})
	}
}
