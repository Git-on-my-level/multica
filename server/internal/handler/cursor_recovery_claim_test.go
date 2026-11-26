package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestClaimTaskCursorRecoveryCustody(t *testing.T) {
	testClaimPinnedRetryCustody(t, false)
}

func TestClaimTaskSameSessionRetryCustody(t *testing.T) {
	testClaimPinnedRetryCustody(t, true)
}

func testClaimPinnedRetryCustody(t *testing.T, sameSession bool) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name         string
		change       string
		legacy, chat bool
	}{
		{name: "issue uses pinned session"},
		{name: "chat uses pinned session", chat: true},
		{name: "rebound after insertion", change: "runtime"},
		{name: "chat rebound after insertion", change: "runtime", chat: true},
		{name: "session lost after insertion", change: "missing"},
		{name: "session replaced after insertion", change: "replaced"},
		{name: "legacy daemon", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
			runtime := parseUUID(runtimeID)
			const session = "original-cursor-session"
			cols := testutil.Cols{
				"runtime_id": runtimeID, "status": "failed",
				"failure_reason": "agent_error.provider_network",
				"session_id":     session, "work_dir": "/tmp/cursor-parent",
				"completed_at": testutil.Raw("now() - interval '5 minutes'"),
			}
			if tc.chat {
				chatID := dbfx.ChatSession(t, agentID, testutil.Cols{
					"runtime_id": runtimeID, "session_id": "unrelated-session", "work_dir": "/tmp/unrelated",
				})
				dbfx.Exec(t, "INSERT INTO chat_message (chat_session_id, role, content) VALUES ($1, 'user', 'resume cursor')", chatID)
				cols["chat_session_id"] = chatID
			} else {
				cols["issue_id"] = dbfx.Issue(t, "Cursor recovery claim", testutil.Cols{"status": "in_progress"})
				dbfx.Task(t, agentID, testutil.Cols{
					"runtime_id": runtimeID, "issue_id": cols["issue_id"], "status": "completed",
					"session_id": "unrelated-session", "work_dir": "/tmp/unrelated",
					"completed_at": testutil.Raw("now() - interval '1 minute'"),
				})
			}
			parent := dbfx.Task(t, agentID, cols)
			params := db.CreateRetryTaskParams{
				ID: parseUUID(parent), RequiredResumeRuntimeID: runtime,
				RequiredResumeSessionID: pgtype.Text{String: session, Valid: true},
			}
			capability := protocol.DaemonCapabilityCursorRecoveryV1
			if sameSession {
				params.RequiredResumeSessionID = pgtype.Text{}
				params.RequireSameSession = pgtype.Bool{Bool: true, Valid: true}
				capability = protocol.DaemonCapabilitySameSessionRetryV1
				dbfx.Exec(t, `UPDATE agent_runtime SET metadata = jsonb_build_object('capabilities', jsonb_build_array($2::text)) WHERE id = $1`, runtime, capability)
			}
			child, err := testHandler.Queries.CreateRetryTask(ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.change {
			case "runtime":
				runtimeID = createRuntimeGuardRuntime(t, ctx, "cursor")
				dbfx.Exec(t, "UPDATE agent_runtime SET status = 'online' WHERE id = $1", runtimeID)
				dbfx.Exec(t, "UPDATE agent SET runtime_id = $1 WHERE id = $2", runtimeID, agentID)
				if _, err := testHandler.Queries.ReassignPendingAgentTasksToRuntime(ctx, db.ReassignPendingAgentTasksToRuntimeParams{
					AgentID: parseUUID(agentID), NewRuntimeID: parseUUID(runtimeID),
				}); err != nil {
					t.Fatal(err)
				}
			case "missing":
				dbfx.Exec(t, "UPDATE agent_task_queue SET session_id = NULL WHERE id = $1", child.ID)
			case "replaced":
				dbfx.Exec(t, "UPDATE agent_task_queue SET session_id = 'different-session' WHERE id = $1", child.ID)
			}
			w := httptest.NewRecorder()
			req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/claim", nil)
			req = withURLParam(req, "runtimeId", runtimeID)
			if !tc.legacy {
				req.Header.Set("X-Client-Capabilities", capability)
			}
			testHandler.ClaimTaskByRuntime(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("claim returned %d: %s", w.Code, w.Body.String())
			}
			var result struct{ Task *AgentTaskResponse }
			if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if tc.change != "" || tc.legacy {
				if result.Task != nil {
					t.Fatal("delivered a recovery without custody")
				}
				stored, err := testHandler.Queries.GetAgentTask(ctx, child.ID)
				if err != nil || stored.Status != "failed" {
					t.Fatalf("rejected recovery not settled: status=%q err=%v", stored.Status, err)
				}
				return
			}

			if result.Task == nil || result.Task.RequireSameSession != sameSession || result.Task.RequireCursorResume == sameSession ||
				result.Task.PriorSessionID != session || result.Task.PriorWorkDir != "/tmp/cursor-parent" {
				t.Fatalf("claim did not pin the original session: %+v", result.Task)
			}
		})
	}
}
