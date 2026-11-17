package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The legacy diagnostic came from a generic cancelled agent result. It is not
// evidence of an involuntary interruption, even with a persisted session.
func TestServerCancellationDoesNotAuthorizeRecovery(t *testing.T) {
	for _, chat := range []bool{false, true} {
		for _, reason := range []string{"cancelled", "agent_error.unknown", "agent_error.provider_auth_or_access"} {
			t.Run(fmt.Sprintf("chat=%v/reason=%s", chat, reason), func(t *testing.T) {
				ctx := context.Background()
				pool := sharedTestPool(t)
				bootstrap := testutil.New(pool, "", "")
				suffix := time.Now().UnixNano()
				user := bootstrap.User(t, "cancellation-owner", fmt.Sprintf("cancellation-%d@example.com", suffix))
				workspace := bootstrap.Workspace(t, "cancellation provenance", fmt.Sprintf("cancellation-%d", suffix))
				fx := testutil.New(pool, workspace, user)
				fx.Member(t, workspace, user, "owner")
				runtime := fx.Runtime(t, "cancellation runtime")
				agent := fx.Agent(t, "cancellation agent", runtime)
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
				id := util.MustParseUUID(parent)
				if _, err := svc.FailTask(ctx, id, "task cancelled by server", "existing-session", "/tmp/existing-work", "", reason, false, "", ""); err != nil {
					t.Fatal(err)
				}
				failed, err := q.GetAgentTask(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if failed.Status != "failed" || failed.FailureReason.String != reason || failed.Error.String != "task cancelled by server" || failed.Attempt != 1 || failed.MaxAttempts != 2 {
					t.Fatalf("terminal cancellation changed: %+v", failed)
				}
				if child, err := svc.MaybeRetryFailedTask(ctx, failed); err != nil || child != nil {
					t.Fatalf("ambiguous cancellation retried: %v, %v", child, err)
				}
				var count int
				fx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id = $1`, parent).Scan(&count)
				if count != 0 {
					t.Fatalf("cancellation created %d recovery children", count)
				}
			})
		}
	}
}
