package service

import (
	"context"
	"fmt"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

const hermesStartupError = `hermes session/resume failed: session/resume: Internal error (code=-32603, data={"details":"Gateway starting: deadline"})`

func TestHermesGatewayResumeRetryEligibility(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*db.AgentTaskQueue)
		want   bool
	}{
		{"issue", func(t *db.AgentTaskQueue) {}, true},
		{"chat", func(t *db.AgentTaskQueue) { t.ChatSessionID = t.IssueID; t.IssueID = pgtype.UUID{} }, true},
		{"no session", func(t *db.AgentTaskQueue) { t.SessionID = pgtype.Text{} }, false},
		{"fresh forced", func(t *db.AgentTaskQueue) { t.ForceFreshSession = true }, false},
		{"retired", func(t *db.AgentTaskQueue) { t.RetiredSessionID = t.SessionID }, false},
		{"rollout missing", func(t *db.AgentTaskQueue) { t.SessionRolloutMissing = true }, false},
		{"budget exhausted", func(t *db.AgentTaskQueue) { t.Attempt = 2 }, false},
		{"retry disabled", func(t *db.AgentTaskQueue) { t.MaxAttempts = 1 }, false},
		{"one recovery despite larger budget", func(t *db.AgentTaskQueue) { t.Attempt = 2; t.MaxAttempts = 10 }, false},
		{"autopilot", func(t *db.AgentTaskQueue) { t.AutopilotRunID = t.IssueID }, false},
		{"triage", func(t *db.AgentTaskQueue) { t.Context = []byte(`{"type":"triage"}`) }, false},
		{"cancelled", func(t *db.AgentTaskQueue) { t.Status = "cancelled" }, false},
		{"no issue or chat", func(t *db.AgentTaskQueue) { t.IssueID = pgtype.UUID{} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := db.AgentTaskQueue{Status: "running", Attempt: 1, MaxAttempts: 2, IssueID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true}, SessionID: pgtype.Text{String: "existing-session", Valid: true}}
			tc.mutate(&task)
			reason := taskfailure.Classify(hermesStartupError).String()
			if got := hermesGatewayResumeRetryEligible(reason, hermesStartupError, task); got != tc.want {
				t.Fatalf("eligible = %v, want %v", got, tc.want)
			}
			if hermesGatewayResumeRetryEligible("agent_error.context_overflow", hermesStartupError, task) {
				t.Fatal("unsafe reason retried")
			}
			if hermesGatewayResumeRetryEligible(reason, "hermes session/resume failed: Internal error", task) {
				t.Fatal("unrelated error retried")
			}
			if hermesGatewayResumeRetryEligible(reason, "hermes session/resume failed: authentication failed", task) {
				t.Fatal("auth error retried")
			}
			if retryEligible(reason, task) {
				t.Fatal("historical sweeper must not retry this class")
			}
		})
	}
}

func TestHermesGatewayRetryDelay(t *testing.T) {
	for i := 0; i < 100; i++ {
		if delay := hermesGatewayRetryDelay(); delay < 5*time.Second || delay >= 10*time.Second {
			t.Fatalf("delay = %v", delay)
		}
	}
}

func TestHermesGatewayResumeRetryTransaction(t *testing.T) {
	for _, name := range []string{"issue", "chat", "exhausted", "disabled", "second attempt", "no session", "unsafe", "triage", "autopilot", "cancelled", "unrelated", "authentication", "pending successor", "missing rollout", "retired session", "historical"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			pool := newResolveOriginatorPool(t)
			q := db.New(pool)
			bootstrap := testutil.New(pool, "", "")
			suffix := time.Now().UnixNano()
			user := bootstrap.User(t, "Hermes retry owner", fmt.Sprintf("hermes-retry-%d@example.com", suffix))
			workspace := bootstrap.Workspace(t, "Hermes retry", fmt.Sprintf("hermes-retry-%d", suffix))
			fx := testutil.New(pool, workspace, user)
			fx.Member(t, workspace, user, "owner")
			runtime := fx.Runtime(t, "fixture runtime")
			agent := fx.Agent(t, "Hermes fixture", runtime)
			issue := fx.Issue(t, "resume startup deadline")
			cols := testutil.Cols{"runtime_id": runtime, "issue_id": issue, "status": "running", "attempt": 1, "max_attempts": 2, "work_dir": "/fixture/workdir"}
			raw, reason, sessionID := hermesStartupError, "", "ses_existing"
			missing, retired := false, ""
			switch name {
			case "chat":
				cols["issue_id"] = nil
				cols["chat_session_id"] = fx.ChatSession(t, agent)
			case "exhausted":
				cols["attempt"] = 2
			case "disabled":
				cols["max_attempts"] = 1
			case "second attempt":
				cols["attempt"] = 2
				cols["max_attempts"] = 10
			case "no session":
				sessionID = ""
			case "unsafe":
				cols["force_fresh_session"] = true
			case "triage":
				cols["context"] = []byte(`{"type":"triage"}`)
			case "autopilot":
				autopilot := fx.Insert(t, "autopilot", testutil.Cols{
					"workspace_id": workspace, "title": "fixture autopilot", "assignee_type": "agent", "assignee_id": agent,
					"status": "active", "execution_mode": "run_only", "created_by_type": "member", "created_by_id": user,
				})
				cols["autopilot_run_id"] = fx.Insert(t, "autopilot_run", testutil.Cols{"autopilot_id": autopilot, "source": "manual", "status": "running"})
			case "cancelled":
				cols["status"] = "cancelled"
			case "unrelated":
				raw = "hermes session/resume failed: Internal error (code=-32603)"
			case "authentication":
				raw = "hermes session/resume failed: authentication failed"
			case "missing rollout":
				missing = true
			case "retired session":
				retired = "ses_existing"
			case "historical":
				cols["status"] = "failed"
				cols["failure_reason"] = "agent_error.provider_server_error"
				cols["error"] = raw
				cols["session_id"] = sessionID
			}
			parentID := fx.Task(t, agent, cols)
			fx.Cleanup(t, `DELETE FROM agent_task_queue WHERE parent_task_id = $1`, parentID)
			if name == "pending successor" {
				fx.Task(t, agent, testutil.Cols{"runtime_id": runtime, "issue_id": issue, "status": "queued"})
			}
			svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
			before := time.Now()
			var parent *db.AgentTaskQueue
			var err error
			if name == "historical" {
				row, e := q.GetAgentTask(ctx, mustHermesTestUUID(parentID))
				if e != nil {
					t.Fatal(e)
				}
				parent, err = svc.MaybeRetryFailedTask(ctx, row)
				if parent != nil {
					t.Fatal("historical failure retried")
				}
			} else {
				parent, err = svc.FailTask(ctx, mustHermesTestUUID(parentID), raw, sessionID, "/fixture/workdir", "", reason, missing, retired, "")
			}
			if err != nil && name != "cancelled" {
				t.Fatal(err)
			}
			var n int
			fx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id = $1`, parentID).Scan(&n)
			want := name == "issue" || name == "chat"
			if !want {
				if n != 0 {
					t.Fatalf("unexpected children: %d", n)
				}
				return
			}
			if n != 1 {
				t.Fatalf("children = %d, want 1", n)
			}
			var childID string
			fx.QueryRow(t, `SELECT id::text FROM agent_task_queue WHERE parent_task_id = $1`, parentID).Scan(&childID)
			child, e := q.GetAgentTask(ctx, mustHermesTestUUID(childID))
			if e != nil {
				t.Fatal(e)
			}
			if child.Status != "deferred" || child.SessionID.String != sessionID || child.WorkDir.String != "/fixture/workdir" || child.ForceFreshSession || child.Attempt != 2 || child.MaxAttempts != 2 || child.RuntimeID != parent.RuntimeID {
				t.Fatalf("bad retry: %+v", child)
			}
			if !child.FireAt.Valid || child.FireAt.Time.Before(before.Add(5*time.Second)) || child.FireAt.Time.After(time.Now().Add(10*time.Second)) {
				t.Fatalf("fire_at = %+v", child.FireAt)
			}
			// Repeated completion callbacks and orphan sweeping cannot add a sibling.
			_, _ = svc.FailTask(ctx, mustHermesTestUUID(parentID), raw, sessionID, "/fixture/workdir", "", reason, false, "", "")
			if retry, e := svc.MaybeRetryFailedTask(ctx, *parent); e != nil || retry != nil {
				t.Fatalf("sweeper: retry=%v err=%v", retry, e)
			}
			fx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id = $1`, parentID).Scan(&n)
			if n != 1 {
				t.Fatalf("duplicate children: %d", n)
			}
			// Even a larger configured budget permits no second startup recovery.
			fx.Exec(t, `UPDATE agent_task_queue SET status='running', max_attempts=10 WHERE id=$1`, childID)
			if _, e := svc.FailTask(ctx, child.ID, raw, sessionID, "/fixture/workdir", "", reason, false, "", ""); e != nil {
				t.Fatal(e)
			}
			fx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1`, childID).Scan(&n)
			if n != 0 {
				t.Fatalf("second recovery created %d children", n)
			}
		})
	}
}

func mustHermesTestUUID(raw string) pgtype.UUID {
	id, err := util.ParseUUID(raw)
	if err != nil {
		panic(err)
	}
	return id
}
