package service

import (
	"context"
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

// Both retry writers must enforce custody against the live database row, not
// just the parent snapshot that had a readable session when it was loaded.
func TestCursorRecoveryRetryInsertionCustody(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, agentID, issueID := seedAttributionFixture(t, pool)
	fixture := testutil.New(pool, workspaceID, userID)
	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	agent, err := q.GetAgent(ctx, util.MustParseUUID(agentID))
	if err != nil {
		t.Fatal(err)
	}
	otherRuntime := fixture.Runtime(t, "rebound")
	const sessionID = "original-cursor-session"
	const preAction = "cursor-agent exited with error: exit status 1 (result_seen=false, exit_code=1, scanner_error=false, event_count=0, invalid_event_count=0, last_event_type=none); actions completed before finalization may already have taken effect; cursor stderr: Error: getaddrinfo ENOTFOUND api2.cursor.sh"
	postAction := strings.Replace(strings.Replace(preAction, "event_count=0", "event_count=14", 1), "last_event_type=none", "last_event_type=connection", 1)
	for _, sweeper := range []bool{false, true} {
		path := "FailTaskWithTransition"
		if sweeper {
			path = "MaybeRetryFailedTask"
		}
		t.Run(path, func(t *testing.T) {
			for _, tc := range []struct {
				name               string
				rebound            bool
				errorText, session string
				wantChild          bool
			}{
				{name: "original runtime", errorText: preAction, session: sessionID, wantChild: true},
				{name: "rebound runtime", rebound: true, errorText: preAction, session: sessionID},
				{name: "missing session", errorText: preAction},
				{name: "post action", errorText: postAction, session: sessionID},
				{name: "ordinary network retry still follows rebind", rebound: true, errorText: "API Error: Connection closed mid-response.", session: sessionID, wantChild: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					newRuntime := agent.RuntimeID
					if tc.rebound {
						newRuntime = util.MustParseUUID(otherRuntime)
					}
					if _, err := pool.Exec(ctx, "UPDATE agent SET runtime_id = $1 WHERE id = $2", newRuntime, agentID); err != nil {
						t.Fatal(err)
					}
					parentID := util.MustParseUUID(fixture.Task(t, agentID, testutil.Cols{
						"runtime_id": agent.RuntimeID, "issue_id": issueID, "status": "running",
						"attempt": 1, "max_attempts": 2,
					}))
					t.Cleanup(func() {
						if _, err := pool.Exec(ctx, "DELETE FROM agent_task_queue WHERE parent_task_id = $1 OR id = $1", parentID); err != nil {
							t.Error(err)
						}
					})

					if sweeper {
						parent, err := q.FailAgentTask(ctx, db.FailAgentTaskParams{
							ID:            parentID,
							Error:         pgtype.Text{String: tc.errorText, Valid: true},
							SessionID:     pgtype.Text{String: tc.session, Valid: tc.session != ""},
							FailureReason: pgtype.Text{String: taskfailure.Classify(tc.errorText).String(), Valid: true},
						})
						if err != nil {
							t.Fatal(err)
						}
						// The sweeper may have read a session that is now gone.
						if tc.session == "" {
							parent.SessionID = pgtype.Text{String: sessionID, Valid: true}
						}
						if _, err := svc.MaybeRetryFailedTask(ctx, parent); err != nil {
							t.Fatal(err)
						}
					} else {
						_, transitioned, err := svc.FailTaskWithTransition(ctx, parentID, tc.errorText, tc.session, "/tmp/cursor-fixture", "", "", false, "", "")
						if err != nil || !transitioned {
							t.Fatalf("failure did not commit: transitioned=%v, err=%v", transitioned, err)
						}
					}

					var count int
					if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_task_queue WHERE parent_task_id = $1", parentID).Scan(&count); err != nil {
						t.Fatal(err)
					}
					if !tc.wantChild {
						if count != 0 {
							t.Fatalf("created %d children without custody", count)
						}
						return
					}
					if count != 1 {
						t.Fatalf("created %d children, want one", count)
					}
					var childID pgtype.UUID
					if err := pool.QueryRow(ctx, "SELECT id FROM agent_task_queue WHERE parent_task_id = $1", parentID).Scan(&childID); err != nil {
						t.Fatal(err)
					}
					child, err := q.GetAgentTask(ctx, childID)
					if err != nil {
						t.Fatal(err)
					}
					required, pinned := CursorRecoverySession(child)
					if tc.rebound {
						if required || child.RuntimeID != newRuntime || child.SessionID.Valid {
							t.Fatalf("ordinary retry did not follow rebind: %+v", child)
						}
					} else {
						if !required || pinned != sessionID || child.RuntimeID != agent.RuntimeID || child.Status != "deferred" {
							t.Fatalf("retry did not retain custody: %+v", child)
						}
						child.Error = pgtype.Text{String: "API Error: Connection closed mid-response.", Valid: true}
						if retryEligible(taskfailure.ReasonAgentProviderNetwork.String(), child) {
							t.Fatal("a subsequent provider-network failure extended the Cursor recovery budget")
						}
					}
				})
			}
		})
	}
}
