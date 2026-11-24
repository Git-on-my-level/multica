package handler

import (
	"net/http"
	"slices"
	"testing"
	"time"

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
	if got := claim(other, "morph/other"); got == nil || got.ID != stale {
		t.Fatalf("scoped claim did not recover admitted dispatch %s: %+v", stale, got)
	}
}
