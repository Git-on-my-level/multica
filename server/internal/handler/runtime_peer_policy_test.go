package handler

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func runtimePeerPolicyHandler(t *testing.T, workspace, first, second string) *Handler {
	t.Helper()
	cfg := testPool.Config()
	cfg.ConnConfig.RuntimeParams["multica.private_runtime_peers"] = workspace + "," + first + "," + second
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h := *testHandler
	h.Queries = db.New(pool)
	h.TxStarter = pool
	h.TaskService = service.NewTaskService(h.Queries, pool, h.Hub, h.Bus)
	return &h
}

func TestPrivateRuntimePeerPolicyPlacement(t *testing.T) {
	runtimeID, owner, peer := runtimeVisibilityFixture(t)
	h := runtimePeerPolicyHandler(t, testWorkspaceID, owner, peer)
	peerRuntime := dbfx.Runtime(t, "Peer's runtime", testutil.Cols{"owner_id": peer})
	unrelatedRuntime := dbfx.Runtime(t, "Unrelated admin's runtime")
	thirdAgent := dbfx.Agent(t, "Agent of an unrelated member", unrelatedRuntime)
	anotherWorkspace := dbfx.Workspace(t, "Other workspace", "other-private-runtime-workspace")
	dbfx.Member(t, anotherWorkspace, owner, "owner")
	dbfx.Member(t, anotherWorkspace, peer, "member")
	anotherRuntime := dbfx.Runtime(t, "Another workspace runtime", testutil.Cols{"workspace_id": anotherWorkspace, "owner_id": owner})

	create := func(handler *Handler, actor, runtime, name string) *testutil.Response {
		req := newRequestAs(actor, http.MethodPost, "/api/agents", map[string]any{
			"name": name, "runtime_id": runtime,
		})
		return testutil.Call(t, handler.CreateAgent, req)
	}
	create(testHandler, peer, runtimeID, "Unset policy denied").Want(http.StatusForbidden)
	create(h, testUserID, runtimeID, "Unrelated admin denied").Want(http.StatusForbidden)
	create(h, peer, unrelatedRuntime, "Outside the pair denied").Want(http.StatusForbidden)

	req := newRequestAs(peer, http.MethodPost, "/api/agents", map[string]any{"name": "Other workspace denied", "runtime_id": anotherRuntime})
	req.Header.Set("X-Workspace-ID", anotherWorkspace)
	testutil.Call(t, h.CreateAgent, req).Want(http.StatusForbidden)

	for _, grant := range []struct{ runtime, recipient string }{
		{runtimeID, peer},
		{peerRuntime, owner},
	} {
		t.Run(grant.recipient, func(t *testing.T) {
			var runtimes []AgentRuntimeResponse
			testutil.Call(t, h.ListAgentRuntimes, newRequestAs(grant.recipient, http.MethodGet, "/api/runtimes", nil)).Want(http.StatusOK).JSON(&runtimes)
			index := slices.IndexFunc(runtimes, func(rt AgentRuntimeResponse) bool { return rt.ID == grant.runtime })
			if index < 0 || runtimes[index].Visibility != "private" || runtimes[index].PolicyPeerID != grant.recipient || len(runtimes[index].SharedWith) != 0 {
				t.Fatalf("runtime missing or incorrect policy readback: %+v", runtimes)
			}
			var created AgentResponse
			create(h, grant.recipient, grant.runtime, "Reciprocal peer "+grant.recipient).Want(http.StatusCreated).JSON(&created)
			dbfx.Cleanup(t, "DELETE FROM agent WHERE id = $1", created.ID)

			var stored AgentResponse
			req := newRequestAs(grant.recipient, http.MethodGet, "/api/agents/"+created.ID, nil)
			testutil.Call(t, h.GetAgent, withURLParam(req, "id", created.ID)).Want(http.StatusOK).JSON(&stored)
			if stored.OwnerID == nil || *stored.OwnerID != grant.recipient || stored.RuntimeID != grant.runtime {
				t.Fatalf("wrong placement readback: %+v", stored)
			}
		})
	}

	mover := dbfx.Agent(t, "Move a peer-owned agent", runtimeID, testutil.Cols{"owner_id": owner})
	move := func(actor, id, runtime string) *testutil.Response {
		req := newRequestAs(actor, http.MethodPatch, "/api/agents/"+id, map[string]any{"runtime_id": runtime})
		return testutil.Call(t, h.UpdateAgent, withURLParam(req, "id", id))
	}
	move(owner, mover, peerRuntime).Want(http.StatusOK)
	for _, recipients := range [][]string{{owner}, {}} {
		req := newRequestAs(peer, http.MethodPatch, "/api/runtimes/"+peerRuntime, map[string]any{"shared_with": recipients})
		testutil.Call(t, h.UpdateAgentRuntime, withURLParam(req, "runtimeId", peerRuntime)).Want(http.StatusOK)
	}
	agent, err := h.Queries.GetAgent(t.Context(), parseUUID(mover))
	if err != nil || uuidToString(agent.RuntimeID) != peerRuntime {
		t.Fatalf("explicit share removal disturbed the policy-only binding: %+v, %v", agent, err)
	}
	// Workspace admin owns this target, but the agent owner has no grant to it.
	move(testUserID, mover, unrelatedRuntime).Want(http.StatusForbidden)
	move(testUserID, thirdAgent, runtimeID).Want(http.StatusForbidden)

	patch := newRequestAs(peer, http.MethodPatch, "/api/runtimes/"+runtimeID, map[string]any{
		"shared_with": []string{testUserID},
	})
	testutil.Call(t, h.UpdateAgentRuntime, withURLParam(patch, "runtimeId", runtimeID)).Want(http.StatusForbidden)
	usage := newRequestAs(peer, http.MethodGet, "/api/runtimes/"+runtimeID+"/usage", nil)
	testutil.Call(t, h.GetRuntimeUsage, withURLParam(usage, "runtimeId", runtimeID)).Want(http.StatusNotFound)
}

func TestPrivateRuntimePeerPolicyExecution(t *testing.T) {
	for _, mode := range []string{"claim", "reclaim", "batch reclaim"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			peer := dbfx.User(t, "Runtime policy peer", "peer-"+uuid.NewString()+"@example.test")
			dbfx.Member(t, testWorkspaceID, peer, "member")
			h := runtimePeerPolicyHandler(t, testWorkspaceID, testUserID, peer)
			runtime := createClaimReclaimRuntime(t, ctx, "Peer policy runtime")
			agentID, issue := createClaimReclaimAgentAndIssue(t, ctx, runtime, "Peer policy agent")
			dbfx.Exec(t, "UPDATE agent SET owner_id = $1 WHERE id = $2", peer, agentID)
			agent, err := h.Queries.GetAgent(ctx, parseUUID(agentID))
			if err != nil {
				t.Fatal(err)
			}
			ready, err := service.AgentReadiness(ctx, service.RuntimeLookup{Queries: h.Queries}, agent)
			if err != nil || !ready.Ready() {
				t.Fatalf("policy-only agent is not ready: %+v, %v", ready, err)
			}
			baseline, err := service.AgentReadiness(ctx, service.RuntimeLookup{Queries: testHandler.Queries}, agent)
			if err != nil || !baseline.Blocked() {
				t.Fatalf("the unset policy should block the cross-owner agent: %+v, %v", baseline, err)
			}
			if mode == "claim" {
				foreign := agent
				foreign.WorkspaceID = parseUUID(uuid.NewString())
				ready, err := service.AgentReadiness(ctx, service.RuntimeLookup{Queries: h.Queries}, foreign)
				if err != nil || !ready.Blocked() {
					t.Fatalf("the policy crossed the agent workspace boundary: %+v, %v", ready, err)
				}
			}
			task := seedQueuedIssueTask(t, ctx, agentID, runtime, issue)
			if mode != "claim" {
				dbfx.Exec(t, "UPDATE agent_task_queue SET status = 'dispatched', dispatched_at = now() - interval '10 minutes', prepare_lease_expires_at = NULL WHERE id = $1", task)
			}
			if mode == "batch reclaim" {
				daemon := uuid.NewString()
				dbfx.Exec(t, "UPDATE agent_runtime SET daemon_id = $1 WHERE id = $2", daemon, runtime)
				req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/tasks/claim", map[string]any{
					"daemon_id": daemon, "runtime_ids": []string{runtime}, "max_tasks": 1,
				})
				var body struct {
					Tasks []struct{ ID string } `json:"tasks"`
				}
				testutil.Call(t, h.ClaimTasksByRuntime, req).Want(http.StatusOK).JSON(&body)
				if len(body.Tasks) != 1 || body.Tasks[0].ID != task {
					t.Fatalf("policy-only task not reclaimed by batch: %+v", body.Tasks)
				}
				return
			}
			var body struct {
				Task *struct{ ID string } `json:"task"`
			}
			req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/runtimes/"+runtime+"/tasks/claim", nil)
			testutil.Call(t, h.ClaimTaskByRuntime, withURLParam(req, "runtimeId", runtime)).Want(http.StatusOK).JSON(&body)
			if body.Task == nil || body.Task.ID != task {
				t.Fatalf("policy-only task not delivered: %+v", body.Task)
			}
		})
	}
}

func TestPrivateRuntimePeerPolicyRevokesOnDeparture(t *testing.T) {
	for _, leaveFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("leaveFirst=", leaveFirst), func(t *testing.T) {
			runtime, first, second := runtimeVisibilityFixture(t)
			h := runtimePeerPolicyHandler(t, testWorkspaceID, first, second)
			otherRuntime := dbfx.Runtime(t, "Second peer's runtime", testutil.Cols{"owner_id": second})
			departing, stayingRuntime := second, runtime
			if leaveFirst {
				departing, stayingRuntime = first, otherRuntime
			}
			bound := dbfx.Agent(t, "Departing peer's agent", stayingRuntime, testutil.Cols{"owner_id": departing})
			taskID := dbfx.Task(t, bound, testutil.Cols{"runtime_id": stayingRuntime, "status": "running"})
			member, err := h.Queries.GetMemberByUserAndWorkspace(t.Context(), db.GetMemberByUserAndWorkspaceParams{
				WorkspaceID: parseUUID(testWorkspaceID), UserID: parseUUID(departing),
			})
			if err != nil {
				t.Fatal(err)
			}
			req := newRequest(http.MethodDelete, "/api/workspaces/"+testWorkspaceID+"/members/"+uuidToString(member.ID), nil)
			testutil.Call(t, h.DeleteMember, testutil.WithURLParams(req, "id", testWorkspaceID, "memberId", uuidToString(member.ID))).Want(http.StatusNoContent)
			members, err := h.Queries.GetPrivateRuntimePolicyMembers(t.Context(), parseUUID(testWorkspaceID))
			if err != nil || len(members) != 0 {
				t.Fatalf("policy survived a peer's departure: %+v, %v", members, err)
			}
			agent, err := h.Queries.GetAgent(t.Context(), parseUUID(bound))
			if err != nil || agent.RuntimeID.Valid {
				t.Fatalf("departed owner's agent remains bound: %+v, %v", agent, err)
			}
			task, err := h.Queries.GetAgentTask(t.Context(), parseUUID(taskID))
			if err != nil || task.Status != "cancelled" {
				t.Fatalf("departed owner's task remains active: %+v, %v", task, err)
			}
		})
	}
}

func TestPrivateRuntimePeerPolicyPlacementDuringRemoval(t *testing.T) {
	runtime, owner, peer := runtimeVisibilityFixture(t)
	h := runtimePeerPolicyHandler(t, testWorkspaceID, owner, peer)
	ctx := t.Context()
	removal, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer removal.Rollback(ctx)
	if _, err := testHandler.Queries.WithTx(removal).LockRuntimeShareMemberRemoval(ctx, db.LockRuntimeShareMemberRemovalParams{
		WorkspaceID: parseUUID(testWorkspaceID), UserID: parseUUID(peer),
	}); err != nil {
		t.Fatal(err)
	}
	const name = "Placement during policy peer departure"
	req := newRequestAs(peer, http.MethodPost, "/api/agents", map[string]any{"name": name, "runtime_id": runtime})
	testutil.Call(t, h.CreateAgent, req).Want(http.StatusConflict)
	var count int
	dbfx.QueryRow(t, "SELECT count(*) FROM agent WHERE runtime_id = $1 AND name = $2", runtime, name).Scan(&count)
	if count != 0 {
		t.Fatalf("placement raced member removal and wrote %d agents", count)
	}
}

func TestPrivateRuntimePeerPolicyPreservesPeerOnOtherMemberDeparture(t *testing.T) {
	runtime, owner, peer := runtimeVisibilityFixture(t)
	other := dbfx.User(t, "Unrelated runtime recipient", "other-"+uuid.NewString()+"@example.test")
	member := dbfx.Member(t, testWorkspaceID, other, "member")
	h := runtimePeerPolicyHandler(t, testWorkspaceID, owner, peer)

	patch := newRequestAs(owner, http.MethodPatch, "/api/runtimes/"+runtime, map[string]any{"shared_with": []string{other}})
	testutil.Call(t, h.UpdateAgentRuntime, withURLParam(patch, "runtimeId", runtime)).Want(http.StatusOK)
	peerAgent := dbfx.Agent(t, "Policy peer's agent", runtime, testutil.Cols{"owner_id": peer})
	otherAgent := dbfx.Agent(t, "Explicit recipient's agent", runtime, testutil.Cols{"owner_id": other})
	peerTask := dbfx.Task(t, peerAgent, testutil.Cols{"runtime_id": runtime, "status": "running"})
	otherTask := dbfx.Task(t, otherAgent, testutil.Cols{"runtime_id": runtime, "status": "running"})

	req := newRequest(http.MethodDelete, "/api/workspaces/"+testWorkspaceID+"/members/"+member, nil)
	testutil.Call(t, h.DeleteMember, testutil.WithURLParams(req, "id", testWorkspaceID, "memberId", member)).Want(http.StatusNoContent)

	for _, tc := range []struct {
		agent, task, status string
		bound               bool
	}{
		{peerAgent, peerTask, "running", true},
		{otherAgent, otherTask, "cancelled", false},
	} {
		agent, err := h.Queries.GetAgent(t.Context(), parseUUID(tc.agent))
		if err != nil || agent.RuntimeID.Valid != tc.bound || (tc.bound && uuidToString(agent.RuntimeID) != runtime) {
			t.Fatalf("agent %s got binding %s, want bound=%v: %v", tc.agent, uuidToString(agent.RuntimeID), tc.bound, err)
		}
		task, err := h.Queries.GetAgentTask(t.Context(), parseUUID(tc.task))
		if err != nil || task.Status != tc.status {
			t.Fatalf("task %s got status %q, want %q: %v", tc.task, task.Status, tc.status, err)
		}
	}
}

func TestPrivateRuntimePeerPolicyModelDiscovery(t *testing.T) {
	runtime, owner, peer := runtimeVisibilityFixture(t)
	h := runtimePeerPolicyHandler(t, testWorkspaceID, owner, peer)
	h.ModelListStore = NewInMemoryModelListStore()
	initiate := func(actor string) *testutil.Response {
		req := newRequestAs(actor, http.MethodPost, "/api/runtimes/"+runtime+"/models", nil)
		return testutil.Call(t, h.InitiateListModels, withURLParam(req, "runtimeId", runtime))
	}
	initiate(testUserID).Want(http.StatusNotFound)
	var requested ModelListRequest
	initiate(peer).Want(http.StatusOK).JSON(&requested)
	if requested.RuntimeID != runtime || requested.ID == "" {
		t.Fatalf("incorrect model request: %+v", requested)
	}
	for _, tc := range []struct {
		actor  string
		status int
	}{
		{peer, http.StatusOK},
		{testUserID, http.StatusNotFound},
	} {
		req := newRequestAs(tc.actor, http.MethodGet, "/api/runtimes/"+runtime+"/models/"+requested.ID, nil)
		testutil.Call(t, h.GetModelListRequest, testutil.WithURLParams(req, "runtimeId", runtime, "requestId", requested.ID)).Want(tc.status)
	}
}
