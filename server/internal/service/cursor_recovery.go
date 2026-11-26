package service

import (
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

func cursorRetrySession(errorText, sessionID string) pgtype.Text {
	return pgtype.Text{String: sessionID, Valid: taskfailure.CursorTransientNetworkError(errorText)}
}

// CursorRecoverySession reads the custody recorded at retry insertion. An
// empty session for a marked retry means delivery must refuse the claim.
func CursorRecoverySession(task db.AgentTaskQueue) (required bool, sessionID string) {
	return pinnedRetrySession(task, "cursor_recovery")
}

// SameSessionRetry returns an empty session for a retry whose custody was lost.
func SameSessionRetry(task db.AgentTaskQueue) (required bool, sessionID string) {
	return pinnedRetrySession(task, "same_session_retry")
}

func requiresSameSessionRetry(task db.AgentTaskQueue) bool {
	return task.FailureReason.String == taskfailure.ReasonAgentProviderCapacityOrRateLimit.String()
}

func pinnedRetrySession(task db.AgentTaskQueue, key string) (required bool, sessionID string) {
	var context map[string]json.RawMessage
	if json.Unmarshal(task.Context, &context) != nil || len(context[key]) == 0 {
		return false, ""
	}
	var custody struct {
		RuntimeID pgtype.UUID `json:"runtime_id"`
		SessionID string      `json:"session_id"`
	}
	if json.Unmarshal(context[key], &custody) != nil ||
		!custody.RuntimeID.Valid || custody.RuntimeID != task.RuntimeID ||
		!task.SessionID.Valid || strings.TrimSpace(custody.SessionID) == "" ||
		custody.SessionID != task.SessionID.String || task.ForceFreshSession {
		return true, ""
	}
	return true, custody.SessionID
}
