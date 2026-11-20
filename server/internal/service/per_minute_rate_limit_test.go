package service

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

func TestRetryEligiblePerMinuteRateLimit(t *testing.T) {
	t.Run("failure lifecycle and chat ordering", testPerMinuteRateLimitLifecycle)
	const raw = "provider.rate-limit: Rate limit exceeded. Please slow down your requests. Limit: 24290 uncached input tokens/minute."
	reason := taskfailure.NormalizeDaemonReason("agent_error.context_overflow", raw).String()
	if ResumeUnsafeFailure(reason, raw) {
		t.Fatal("transient throttle must preserve the session")
	}
	if got := retryDelayForAttempt(reason, 1); got < 60*time.Second {
		t.Fatalf("retry delay = %s, want >=60s", got)
	}
	id := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	base := db.AgentTaskQueue{Attempt: 1, MaxAttempts: 2, IssueID: id, Error: pgtype.Text{String: raw, Valid: true}, SessionID: pgtype.Text{String: "existing-session", Valid: true}}
	for _, tc := range []struct {
		name   string
		change func(*db.AgentTaskQueue)
		want   bool
	}{
		{"issue", func(t *db.AgentTaskQueue) {}, true},
		{"chat", func(t *db.AgentTaskQueue) { t.IssueID = pgtype.UUID{}; t.ChatSessionID = id }, true},
		{"disabled", func(t *db.AgentTaskQueue) { t.MaxAttempts = 1 }, false},
		{"exhausted", func(t *db.AgentTaskQueue) { t.Attempt = 2 }, false},
		{"larger budget still only one retry", func(t *db.AgentTaskQueue) { t.Attempt = 2; t.MaxAttempts = 5 }, false},
		{"larger budget first attempt", func(t *db.AgentTaskQueue) { t.MaxAttempts = 5 }, true},
		{"autopilot", func(t *db.AgentTaskQueue) { t.AutopilotRunID = id }, false},
		{"triage", func(t *db.AgentTaskQueue) { t.Context = []byte(`{"type":"triage"}`) }, false},
		{"unlinked", func(t *db.AgentTaskQueue) { t.IssueID = pgtype.UUID{} }, false},
		{"quick create", func(t *db.AgentTaskQueue) {
			t.IssueID = pgtype.UUID{}
			t.Context = []byte(`{"type":"quick_create","source_context_id":"source"}`)
		}, false},
		{"unsafe history", func(t *db.AgentTaskQueue) { t.Error.String += " 400 invalid_request_error" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := base
			tc.change(&task)
			if got := retryEligible(reason, task); got != tc.want {
				t.Fatalf("eligible=%v, want %v", got, tc.want)
			}
			if got := retryAttemptCeiling(reason, task.MaxAttempts); got != task.MaxAttempts {
				t.Fatalf("budget changed: %d", got)
			}
		})
	}
	for _, signal := range []string{"An api key is required", "401 unauthorized", "403 forbidden", "invalid API key", "refresh token expired", "HTTP 402", "monthly usage limit reached", "You've hit your session limit", "You've hit your weekly limit", "insufficient_balance"} {
		for _, mixed := range []string{signal + ": " + raw, raw + " " + signal} {
			for _, chat := range []bool{false, true} {
				task := base
				task.Error.String = mixed
				if chat {
					task.IssueID = pgtype.UUID{}
					task.ChatSessionID = id
				}
				// Test the raw-text guard even when normalization was bypassed and the
				// stored reason still claims this was a capacity failure.
				for _, reported := range []string{reason, taskfailure.Classify(mixed).String(), taskfailure.NormalizeDaemonReason("agent_error.context_overflow", mixed).String()} {
					if retryEligible(reported, task) {
						t.Errorf("mixed config/auth/quota retried: reason=%s chat=%v error=%q", reported, chat, mixed)
					}
				}
			}
		}
	}
	for _, text := range []string{"HTTP 429", "monthly usage limit", "You've hit your session limit", "401 unauthorized", "maximum context length: token limit", "Rate limit exceeded", "concurrent request limit"} {
		task := base
		task.Error.String = text
		if retryEligible(taskfailure.Classify(text).String(), task) {
			t.Errorf("unrelated error retried: %q", text)
		}
		if retryEligible(reason, task) {
			t.Errorf("capacity reason alone retried: %q", text)
		}
	}
}

// Chat backoff preserves the existing scheduling contract: newer input can run
// during the cooldown, and the retry retains its original input owner.
func testPerMinuteRateLimitLifecycle(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL must explicitly select a managed test database")
	}
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, agentID, _ := seedAttributionFixture(t, pool)
	fx := dbfx.New(pool, workspaceID, userID)
	var runtimeID string
	fx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id=$1`, agentID).Scan(&runtimeID)
	fx.Exec(t, `UPDATE agent_runtime SET status='online', last_seen_at=now() WHERE id=$1`, runtimeID)
	chatID := fx.ChatSession(t, agentID)
	parentID := fx.Task(t, agentID, dbfx.Cols{
		"runtime_id": runtimeID, "chat_session_id": chatID, "status": "running",
		"attempt": 1, "max_attempts": 5, "session_id": "existing-session", "work_dir": "/test/workdir",
	})
	fx.Exec(t, `UPDATE agent_task_queue SET chat_input_task_id=id WHERE id=$1`, parentID)
	fx.Cleanup(t, `DELETE FROM agent_task_queue WHERE parent_task_id=$1`, parentID)
	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	const raw = "provider.rate-limit: Rate limit exceeded. Please slow down your requests. Limit: 24290 uncached input tokens/minute."
	start := time.Now()
	if _, err := svc.FailTask(ctx, util.MustParseUUID(parentID), raw, "existing-session", "/test/workdir", "", "agent_error.context_overflow", false, "", ""); err != nil {
		t.Fatal(err)
	}
	var childID string
	fx.QueryRow(t, `SELECT id::text FROM agent_task_queue WHERE parent_task_id=$1`, parentID).Scan(&childID)
	child, err := q.GetAgentTask(ctx, util.MustParseUUID(childID))
	if err != nil {
		t.Fatal(err)
	}
	if child.Status != "deferred" || !child.FireAt.Valid || child.FireAt.Time.Before(start.Add(60*time.Second)) {
		t.Fatalf("retry must be deferred >=60s: status=%s fire_at=%v", child.Status, child.FireAt)
	}
	if child.Attempt != 2 || child.MaxAttempts != 5 || child.SessionID.String != "existing-session" || child.ForceFreshSession || child.ChatInputTaskID != util.MustParseUUID(parentID) {
		t.Fatalf("retry inheritance changed: attempt=%d max=%d session=%s fresh=%v input=%v", child.Attempt, child.MaxAttempts, child.SessionID.String, child.ForceFreshSession, child.ChatInputTaskID)
	}
	// A replay of the failure notification must not create another pending child.
	if _, err := svc.FailTask(ctx, util.MustParseUUID(parentID), raw, "existing-session", "/test/workdir", "", "agent_error.context_overflow", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if n := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1`, parentID); n != 1 {
		t.Fatalf("retry children=%d, want 1", n)
	}
	newerID := fx.Task(t, agentID, dbfx.Cols{"runtime_id": runtimeID, "chat_session_id": chatID, "status": "queued"})
	fx.Exec(t, `UPDATE agent_task_queue SET chat_input_task_id=id WHERE id=$1`, newerID)
	claimed, err := q.ClaimAgentTask(ctx, db.ClaimAgentTaskParams{AgentID: util.MustParseUUID(agentID), RuntimeID: util.MustParseUUID(runtimeID), PrepareLeaseSecs: 60, RuntimeStaleSecs: RuntimeClaimFreshnessSeconds})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != util.MustParseUUID(newerID) {
		t.Fatalf("claimed %v, want newer turn %s during backoff", claimed.ID, newerID)
	}
	if claimed.ChatInputTaskID == child.ChatInputTaskID {
		t.Fatal("distinct chat turns must keep distinct input owners")
	}
	// Even a larger inherited budget must not permit a second throttle retry.
	fx.Exec(t, `UPDATE agent_task_queue SET status='completed' WHERE id=$1`, newerID)
	fx.Exec(t, `UPDATE agent_task_queue SET status='running' WHERE id=$1`, childID)
	if _, err := svc.FailTask(ctx, child.ID, raw, "existing-session", "/test/workdir", "", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if n := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1`, childID); n != 0 {
		t.Fatalf("unexpected second retry: %d", n)
	}
}
