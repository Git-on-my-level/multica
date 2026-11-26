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
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

func TestRetryEligibleSelectedModelCapacity(t *testing.T) {
	const raw = "Selected model is at capacity. Please try a different model."
	t.Run("failure lifecycle and chat ordering", func(t *testing.T) {
		testTransientCapacityLifecycle(t, raw, "agent_error.model_not_found_or_unavailable")
	})
	t.Run("reported resume boundary", testSelectedCapacityResumeBoundary)
	reason := taskfailure.NormalizeDaemonReason("agent_error.model_not_found_or_unavailable", raw).String()
	id := pgtype.UUID{Valid: true}
	base := db.AgentTaskQueue{Status: "running", Attempt: 1, MaxAttempts: 2, IssueID: id,
		SessionID: pgtype.Text{String: "productive-session", Valid: true}, Error: pgtype.Text{String: raw, Valid: true}}
	if ResumeUnsafeFailure(reason, raw) || retryDelayForAttempt(reason, 1) != 60*time.Second {
		t.Fatal("capacity recovery must resume after the bounded 60s delay")
	}
	for _, tc := range []struct {
		name   string
		change func(*db.AgentTaskQueue)
		want   bool
	}{
		{"issue existing session", func(t *db.AgentTaskQueue) {}, true},
		{"chat existing session", func(t *db.AgentTaskQueue) { t.IssueID = pgtype.UUID{}; t.ChatSessionID = id }, true},
		{"productive output with session", func(t *db.AgentTaskQueue) { t.Result = []byte(`{"output":"partial work"}`) }, true},
		{"productive output without session", func(t *db.AgentTaskQueue) {
			t.Result = []byte(`{"output":"partial work"}`)
			t.SessionID = pgtype.Text{}
		}, false},
		{"missing session", func(t *db.AgentTaskQueue) { t.SessionID = pgtype.Text{} }, false},
		{"blank session", func(t *db.AgentTaskQueue) { t.SessionID.String = " " }, false},
		{"missing rollout", func(t *db.AgentTaskQueue) { t.SessionRolloutMissing = true }, false},
		{"fresh session requested", func(t *db.AgentTaskQueue) { t.ForceFreshSession = true }, false},
		{"retired session", func(t *db.AgentTaskQueue) { t.RetiredSessionID = t.SessionID }, false},
		{"disabled", func(t *db.AgentTaskQueue) { t.MaxAttempts = 1 }, false},
		{"exhausted", func(t *db.AgentTaskQueue) { t.Attempt = 2 }, false},
		{"only one recovery", func(t *db.AgentTaskQueue) { t.Attempt = 2; t.MaxAttempts = 5 }, false},
		{"larger first budget", func(t *db.AgentTaskQueue) { t.MaxAttempts = 5 }, true},
		{"autopilot", func(t *db.AgentTaskQueue) { t.AutopilotRunID = id }, false},
		{"triage", func(t *db.AgentTaskQueue) { t.Context = []byte(`{"type":"triage"}`) }, false},
		{"unlinked", func(t *db.AgentTaskQueue) { t.IssueID = pgtype.UUID{} }, false},
		{"cancelled", func(t *db.AgentTaskQueue) { t.Status = "cancelled" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := base
			tc.change(&task)
			if got := retryEligible(reason, task); got != tc.want {
				t.Fatalf("eligible=%v, want %v", got, tc.want)
			}
			if retryAttemptCeiling(reason, task.MaxAttempts) != task.MaxAttempts {
				t.Fatal("attempt budget widened")
			}
		})
	}
	for _, text := range []string{"model X not found", "Selected model is unavailable", "401 unauthorized", "quota exhausted", "HTTP 429", "maximum context length exceeded", "concurrent request limit", raw + " 400 invalid_request_error", raw + " execution cancelled", "diagnostic: " + raw} {
		for _, mixed := range []string{text, text + ": " + raw, raw + " " + text} {
			for _, chat := range []bool{false, true} {
				task := base
				task.Error.String = mixed
				if chat {
					task.IssueID = pgtype.UUID{}
					task.ChatSessionID = id
				}
				if retryEligible(reason, task) || retryEligible(taskfailure.Classify(mixed).String(), task) {
					t.Errorf("unrelated/mixed failure retried: %q chat=%v", mixed, chat)
				}
			}
		}
	}
	if retryEligible("cancelled", base) || retryEligible("agent_error.context_overflow", base) {
		t.Fatal("stronger cancellation/unsafe reason must remain terminal")
	}
}

func testSelectedCapacityResumeBoundary(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL must explicitly select a managed test database")
	}
	pool := newResolveOriginatorPool(t)
	workspaceID, userID, agentID, _ := seedAttributionFixture(t, pool)
	fx := dbfx.New(pool, workspaceID, userID)
	var runtimeID string
	fx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id=$1`, agentID).Scan(&runtimeID)
	fx.Exec(t, `UPDATE agent_runtime SET metadata=$2 WHERE id=$1`, runtimeID,
		`{"capabilities":["`+protocol.DaemonCapabilitySameSessionRetryV1+`"]}`)
	q := db.New(pool)
	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	for _, tc := range []struct {
		name, stored, reported, retired string
		missing, fresh, cancelled, want bool
	}{
		{name: "session first reported at failure", reported: "new-session", want: true},
		{name: "midflight session retained", stored: "midflight-session", want: true},
		{name: "productive output without session"},
		{name: "missing rollout overrides midflight pin", stored: "midflight-session", missing: true},
		{name: "missing rollout overrides reported session", reported: "new-session", missing: true},
		{name: "retired session", stored: "dead-session", retired: "dead-session"},
		{name: "fresh session requested", stored: "midflight-session", fresh: true},
		{name: "explicit cancellation", stored: "midflight-session", cancelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chatID := fx.ChatSession(t, agentID)
			cols := dbfx.Cols{"runtime_id": runtimeID, "chat_session_id": chatID, "status": "running", "attempt": 1, "max_attempts": 2,
				"result": `{"output":"productive partial work"}`, "force_fresh_session": tc.fresh}
			if tc.stored != "" {
				cols["session_id"] = tc.stored
			}
			if tc.cancelled {
				cols["status"] = "cancelled"
			}
			parentID := fx.Task(t, agentID, cols)
			fx.Cleanup(t, `DELETE FROM agent_task_queue WHERE parent_task_id=$1`, parentID)
			const raw = "Selected model is at capacity. Please try a different model."
			_, err := svc.FailTask(context.Background(), util.MustParseUUID(parentID), raw, tc.reported, "/test/workdir", "", "agent_error.model_not_found_or_unavailable", tc.missing, tc.retired, "")
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.want {
				want = 1
			}
			if n := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1`, parentID); n != want {
				t.Fatalf("retry children=%d, want %d", n, want)
			}
			parent, err := q.GetAgentTask(context.Background(), util.MustParseUUID(parentID))
			if err != nil {
				t.Fatal(err)
			}
			if !tc.cancelled && (parent.Status != "failed" || parent.Error.String != raw) {
				t.Fatal("terminal failure lost its diagnostic")
			}
			if tc.cancelled && parent.Status != "cancelled" {
				t.Fatal("explicit cancellation was overwritten")
			}
			if tc.want {
				var session string
				var fresh bool
				fx.QueryRow(t, `SELECT session_id, force_fresh_session FROM agent_task_queue WHERE parent_task_id=$1`, parentID).Scan(&session, &fresh)
				if session != parent.SessionID.String || session == "" || fresh {
					t.Fatalf("lost resume boundary: session=%s fresh=%v", session, fresh)
				}
			}
		})
	}
}
