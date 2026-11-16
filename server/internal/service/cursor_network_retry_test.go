package service

import (
	"strings"
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
