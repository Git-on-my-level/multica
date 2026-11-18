package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

func TestCodexModelAdmissionFailureAndAgentUpdate(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := dbfx.Runtime(t, "Codex model admission", testutil.Cols{"provider": "codex"})
	agentID := dbfx.Agent(t, "Sol", runtimeID, testutil.Cols{
		"model": "gpt-6.2-sol", "runtime_config": testutil.Raw(`'{"other_setting":"keep"}'::jsonb`),
	})
	svc := testHandler.TaskService
	issueID := dbfx.Issue(t, "Codex model admission")
	taskCols := testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID}
	firstID := dbfx.Task(t, agentID, taskCols)
	claimed, err := svc.ClaimTask(ctx, parseUUID(agentID))
	if err != nil || claimed == nil || uuidToString(claimed.ID) != firstID {
		t.Fatalf("claim initial task: %v, %v", claimed, err)
	}
	if _, err := svc.StartTask(ctx, claimed.ID); err != nil {
		t.Fatal(err)
	}

	// Codex stores the extracted turn.error.message in ExecutionResult.Error.
	// This report follows the daemon failure endpoint through to the next claim.
	req := withURLParam(newOwnerDaemonRequest(http.MethodPost, "/api/daemon/tasks/"+firstID+"/fail", TaskFailRequest{
		Error:         "The 'gpt-6.2-sol' model is not supported when using Codex with a ChatGPT account.",
		FailureReason: taskfailure.ReasonAgentUnknown.String(),
	}), "taskId", firstID)
	var failed AgentTaskResponse
	testutil.Call(t, testHandler.FailTask, req).Want(http.StatusOK).JSON(&failed)
	if failed.Status != "failed" || failed.FailureReason != taskfailure.ReasonAgentUnknown.String() {
		t.Fatalf("terminal response changed the daemon classification: %+v", failed)
	}

	nextID := dbfx.Task(t, agentID, taskCols)
	claimHeld := func() {
		t.Helper()
		got, err := svc.ClaimTask(ctx, parseUUID(agentID))
		if err != nil || got != nil {
			t.Fatalf("claim while model admission is held: %v, %v", got, err)
		}
	}
	claimHeld()

	update := func(payload map[string]any) AgentResponse {
		t.Helper()
		req := withURLParam(newRequest(http.MethodPut, "/api/agents/"+agentID, payload), "id", agentID)
		var agent AgentResponse
		testutil.Call(t, testHandler.UpdateAgent, req).Want(http.StatusOK).JSON(&agent)
		return agent
	}
	update(map[string]any{"name": "Sol renamed"})
	claimHeld()

	settings := map[string]string{"other_setting": "keep", "codex_model_admission_revision": "catalog-repair-1"}
	update(map[string]any{"runtime_config": settings})
	resp := update(map[string]any{"description": "Same catalog revision"})
	gotConfig, err := json.Marshal(resp.RuntimeConfig)
	if err != nil {
		t.Fatal(err)
	}
	var actual map[string]string
	if err := json.Unmarshal(gotConfig, &actual); err != nil {
		t.Fatalf("decode runtime config: %v", err)
	}
	for key, want := range settings {
		if actual[key] != want {
			t.Fatalf("retained %s = %q, want %q", key, actual[key], want)
		}
	}
	got, err := svc.ClaimTask(ctx, parseUUID(agentID))
	if err != nil || got == nil || uuidToString(got.ID) != nextID {
		t.Fatalf("claim after catalog revision update: %v, %v", got, err)
	}
}
