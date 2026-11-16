package service

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

func TestRetryEligiblePerMinuteRateLimit(t *testing.T) {
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
