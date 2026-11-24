package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestOpenCodeAdmissionScopesQueuedAndReclaimedTasks(t *testing.T) {
	runtime := dbfx.Runtime(t, "OpenCode admission", testutil.Cols{"provider": "opencode"})
	target := dbfx.Agent(t, "Admitted OpenCode", runtime, testutil.Cols{"model": "morph/admitted"})
	other := dbfx.Agent(t, "Other OpenCode", runtime, testutil.Cols{"model": "morph/other"})
	queued := dbfx.Task(t, target, testutil.Cols{
		"runtime_id": runtime, "issue_id": dbfx.Issue(t, "Admitted queued task"),
	})
	deferred := dbfx.Task(t, other, testutil.Cols{
		"runtime_id": runtime, "issue_id": dbfx.Issue(t, "Other deferred task"),
		"status": "deferred", "fire_at": time.Now().Add(-time.Minute),
	})
	oldDispatch := time.Now().UTC().Add(-15 * time.Minute).Truncate(time.Microsecond)
	stale := dbfx.Task(t, other, testutil.Cols{
		"runtime_id": runtime, "issue_id": dbfx.Issue(t, "Other stale dispatch"),
		"status": "dispatched", "dispatched_at": oldDispatch,
	})

	path := "/api/daemon/runtimes/" + runtime + "/tasks/admission"
	get := withURLParam(newOwnerDaemonRequest(http.MethodGet, path, nil), "runtimeId", runtime)
	candidates := testutil.Decode[[]protocol.AdmissionCandidate](t, testHandler.ListOpenCodeAdmissionCandidates, get, http.StatusOK)
	for _, want := range []protocol.AdmissionCandidate{{AgentID: target, Model: "morph/admitted"}, {AgentID: other, Model: "morph/other"}} {
		if !slices.Contains(candidates, want) {
			t.Fatalf("preview missing %+v: %+v", want, candidates)
		}
	}

	claim := func(agentID, model string) *AgentTaskResponse {
		t.Helper()
		req := withURLParam(newOwnerDaemonRequest(http.MethodPost, path, protocol.AdmissionCandidate{
			AgentID: agentID, Model: model,
		}), "runtimeId", runtime)
		resp := testutil.Decode[struct {
			Task *AgentTaskResponse `json:"task"`
		}](t, testHandler.ClaimOpenCodeAdmission, req, http.StatusOK)
		return resp.Task
	}

	for _, probe := range []struct{ agentID, model string }{{target, "morph/old"}, {other, "morph/old"}} {
		if got := claim(probe.agentID, probe.model); got != nil {
			t.Fatalf("stale model unexpectedly claimed %s", got.ID)
		}
	}
	var queuedStatus string
	dbfx.QueryRow(t, "SELECT status FROM agent_task_queue WHERE id=$1", queued).Scan(&queuedStatus)
	if queuedStatus != "queued" {
		t.Fatalf("preview or mismatched model changed queued task to %s", queuedStatus)
	}

	if got := claim(target, "morph/admitted"); got == nil || got.ID != queued {
		t.Fatalf("scoped claim did not deliver admitted queued task %s: %+v", queued, got)
	}
	var dispatchedAt time.Time
	dbfx.QueryRow(t, "SELECT dispatched_at FROM agent_task_queue WHERE id=$1", stale).Scan(&dispatchedAt)
	if !dispatchedAt.Equal(oldDispatch) {
		t.Fatalf("other agent's dispatch was reclaimed without admission: %s", dispatchedAt)
	}
	var deferredStatus string
	dbfx.QueryRow(t, "SELECT status FROM agent_task_queue WHERE id=$1", deferred).Scan(&deferredStatus)
	if deferredStatus != "deferred" {
		t.Fatalf("queued candidate promoted other agent's deferred work to %s", deferredStatus)
	}
	if got := claim(other, "morph/other"); got == nil || got.ID != stale {
		t.Fatalf("scoped claim did not recover admitted dispatch %s: %+v", stale, got)
	}
}

func TestOpenCodeAdmissionRequeuesWhenPayloadModelChanges(t *testing.T) {
	const originalModel = "morph/original"
	const changedModel = "morph/changed"
	runtime := dbfx.Runtime(t, "OpenCode model drift", testutil.Cols{"provider": "opencode"})
	agentID := dbfx.Agent(t, "Changing model", runtime, testutil.Cols{"model": originalModel})
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtime, "issue_id": dbfx.Issue(t, "Model changes after claim"),
	})

	// Force the edit after the claim checked the locked model, but before the
	// handler reads the delivery payload. Key the trigger to this task.
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	functionName := "admission_model_drift_" + suffix
	triggerName := "admission_model_drift_trg_" + suffix
	dbfx.Exec(t, fmt.Sprintf(`
CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
	UPDATE agent SET model = '%s' WHERE id = NEW.agent_id;
	RETURN NEW;
END
$fn$;`, functionName, changedModel))
	t.Cleanup(func() { dbfx.Exec(t, "DROP FUNCTION "+functionName+"()") })
	dbfx.Exec(t, fmt.Sprintf(`
CREATE TRIGGER %s AFTER UPDATE ON agent_task_queue
FOR EACH ROW WHEN (OLD.id = '%s'::uuid AND OLD.status = 'queued' AND NEW.status = 'dispatched')
EXECUTE FUNCTION %s();`, triggerName, taskID, functionName))
	t.Cleanup(func() { dbfx.Exec(t, "DROP TRIGGER "+triggerName+" ON agent_task_queue") })

	path := "/api/daemon/runtimes/" + runtime + "/tasks/admission"
	req := withURLParam(newOwnerDaemonRequest(http.MethodPost, path, protocol.AdmissionCandidate{
		AgentID: agentID, Model: originalModel,
	}), "runtimeId", runtime)
	var response map[string]json.RawMessage
	testutil.Call(t, testHandler.ClaimOpenCodeAdmission, req).Want(http.StatusOK).JSON(&response)
	if string(response["task"]) != "null" {
		t.Fatalf("claim response task = %s, want JSON null", response["task"])
	}
	var status, model string
	var tokenCount int
	dbfx.QueryRow(t, `SELECT t.status, a.model, (SELECT count(*) FROM task_token WHERE task_id = t.id)
		FROM agent_task_queue t JOIN agent a ON a.id = t.agent_id WHERE t.id = $1`, taskID).Scan(&status, &model, &tokenCount)
	if status != "queued" || model != changedModel || tokenCount != 0 {
		t.Fatalf("task after payload change = %s, model %s, tokens %d", status, model, tokenCount)
	}
}
