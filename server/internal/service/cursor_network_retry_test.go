package service

import (
	"context"
	"strings"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
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

func TestCursorTransientLaunchRetryPolicy(t *testing.T) {
	t.Parallel()
	const header = "cursor-agent exited with error: exit status 1 (result_seen=false, exit_code=1, scanner_error=false, event_count=0, invalid_event_count=0, last_event_type=none); actions completed before finalization may already have taken effect; cursor stderr: "
	for _, witness := range []string{"RetriableError: [unavailable] Error", "Error: getaddrinfo ENOTFOUND api2.cursor.sh"} {
		t.Run(witness, func(t *testing.T) {
			base := db.AgentTaskQueue{
				Attempt: 1, MaxAttempts: 2,
				IssueID:   pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
				SessionID: pgtype.Text{String: "existing-session", Valid: true},
				Error:     pgtype.Text{String: header + witness, Valid: true},
			}
			reason := taskfailure.NormalizeDaemonReason("agent_error.process_failure", base.Error.String).String()
			if !retryEligible(reason, base) {
				t.Fatal("safe pre-action failure should retry")
			}
			if taskRetryCeiling(reason, base) != 2 || taskRetryDelay(reason, base) != 5*time.Second {
				t.Fatal("expected one recovery deferred five seconds")
			}
			cases := map[string]func(*db.AgentTaskQueue){
				"chat": func(q *db.AgentTaskQueue) { q.ChatSessionID = q.IssueID; q.IssueID.Valid = false },
				"ambiguous connection": func(q *db.AgentTaskQueue) {
					q.Error.String = strings.Replace(q.Error.String, "event_count=0", "event_count=14", 1)
				},
				"no diagnostics": func(q *db.AgentTaskQueue) { q.Error.String = witness },
				"invalid event": func(q *db.AgentTaskQueue) {
					q.Error.String = strings.Replace(q.Error.String, "invalid_event_count=0", "invalid_event_count=1", 1)
				},
				"missing session":             func(q *db.AgentTaskQueue) { q.SessionID.Valid = false },
				"empty session":               func(q *db.AgentTaskQueue) { q.SessionID.String = "" },
				"autopilot":                   func(q *db.AgentTaskQueue) { q.AutopilotRunID = q.IssueID },
				"triage":                      func(q *db.AgentTaskQueue) { q.Context = []byte(`{"type":"triage"}`) },
				"disabled":                    func(q *db.AgentTaskQueue) { q.MaxAttempts = 1 },
				"exhausted":                   func(q *db.AgentTaskQueue) { q.Attempt = 2 },
				"larger budget still bounded": func(q *db.AgentTaskQueue) { q.Attempt = 2; q.MaxAttempts = 10 },
				"unlinked":                    func(q *db.AgentTaskQueue) { q.IssueID.Valid = false },
			}
			for name, mutate := range cases {
				t.Run(name, func(t *testing.T) {
					q := base
					mutate(&q)
					if got := retryEligible(reason, q); got != (name == "chat") {
						t.Fatalf("retryEligible = %v", got)
					}
				})
			}
			for _, excluded := range []string{"cancelled", "agent_error.provider_auth_or_access", "agent_error.provider_quota_limit", "agent_error.process_failure"} {
				if retryEligible(excluded, base) {
					t.Errorf("excluded reason %s retried", excluded)
				}
			}
		})
	}
}

// Exercise the real failure transaction with rows that have no persisted error
// or session yet, as on both a cold parent row and a newly-created retry row.
func TestFailTaskCursorTransientLaunchRecovery(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	dbfx := testutil.New(pool, workspaceID, userID)
	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	var runtimeID pgtype.UUID
	if err := pool.QueryRow(ctx, "SELECT runtime_id FROM agent WHERE id = $1", agentID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	const header = "cursor-agent exited with error: exit status 1 (result_seen=false, exit_code=1, scanner_error=false, event_count=0, invalid_event_count=0, last_event_type=none); actions completed before finalization may already have taken effect; cursor stderr: "
	for _, witness := range []string{"RetriableError: [unavailable] Error", "Error: getaddrinfo ENOTFOUND api2.cursor.sh"} {
		t.Run(witness, func(t *testing.T) {
			parentID := util.MustParseUUID(dbfx.Task(t, agentID, testutil.Cols{
				"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "attempt": 1, "max_attempts": 2,
			}))
			t.Cleanup(func() {
				pool.Exec(context.Background(), "DELETE FROM agent_task_queue WHERE parent_task_id IN (SELECT id FROM agent_task_queue WHERE parent_task_id = $1)", parentID)
				pool.Exec(context.Background(), "DELETE FROM agent_task_queue WHERE parent_task_id = $1 OR id = $1", parentID)
			})
			before := time.Now()
			_, transitioned, err := svc.FailTaskWithTransition(ctx, parentID, header+witness, "requested-session", "/tmp/cursor-fixture", "", "agent_error.process_failure", false, "", "")
			if err != nil || !transitioned {
				t.Fatalf("FailTaskWithTransition: transitioned=%v, err=%v", transitioned, err)
			}
			var childID pgtype.UUID
			if err := pool.QueryRow(ctx, "SELECT id FROM agent_task_queue WHERE parent_task_id = $1", parentID).Scan(&childID); err != nil {
				t.Fatal(err)
			}
			child, err := q.GetAgentTask(ctx, childID)
			if err != nil {
				t.Fatal(err)
			}
			if child.Attempt != 2 || child.MaxAttempts != 2 || child.Status != "deferred" ||
				!child.SessionID.Valid || child.SessionID.String != "requested-session" || child.ForceFreshSession ||
				!child.FireAt.Valid || child.FireAt.Time.Before(before.Add(5*time.Second)) || child.FireAt.Time.After(time.Now().Add(5*time.Second)) {
				t.Fatalf("incorrect recovery child: %+v", child)
			}
			// A duplicate terminal report must not create another deferred child.
			if _, changed, err := svc.FailTaskWithTransition(ctx, parentID, header+witness, "requested-session", "/tmp/cursor-fixture", "", "", false, "", ""); err != nil || changed {
				t.Fatalf("duplicate failure report: changed=%v, err=%v", changed, err)
			}
			var n int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_task_queue WHERE parent_task_id = $1", parentID).Scan(&n); err != nil || n != 1 {
				t.Fatalf("retry count=%d, err=%v", n, err)
			}
			// Retry rows start with NULL errors. Clear the stored session too
			// so this failure report must supply both inputs to the retry gate.
			if _, err := pool.Exec(ctx, "UPDATE agent_task_queue SET status = 'running', error = NULL, session_id = NULL WHERE id = $1", childID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := svc.FailTaskWithTransition(ctx, childID, header+witness, "requested-session", "/tmp/cursor-fixture", "", "", false, "", ""); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_task_queue WHERE parent_task_id = $1", childID).Scan(&n); err != nil || n != 0 {
				t.Fatalf("exhausted child spawned %d retries, err=%v", n, err)
			}
		})
	}
}
