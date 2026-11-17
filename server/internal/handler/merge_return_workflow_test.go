package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

func TestMergeReturnWorkflowExplicitOriginalOwnerAndNoStartBoundary(t *testing.T) {
	seedTestCatalog(t)
	merger := seededReadyAgentID(t)
	author := dbfx.Agent(t, "Original merge implementer", handlerTestRuntimeID(t))
	project := deliveryProject(t, "Merge return no-start boundary", merger)
	issue := dbfx.Issue(t, "Merge return fixture", testutil.Cols{"project_id": project, "status": "merge", "assignee_type": "agent", "assignee_id": merger})
	before, err := testHandler.Queries.GetIssue(t.Context(), util.MustParseUUID(issue))
	if err != nil {
		t.Fatal(err)
	}
	updateIssueForTest(t, issue, map[string]any{"status": "in_progress", "assignee_type": "agent", "assignee_id": author,
		"expected_revision": before.Revision, "suppress_run": true}).Want(http.StatusOK)
	status, kind, assigned := issueStatusAndAssignee(t, issue)
	if status != "in_progress" || kind != "agent" || assigned != author {
		t.Fatalf("stage handler stole repair: %s %s %s", status, kind, assigned)
	}
	tasks, err := testHandler.Queries.ListTasksByIssue(t.Context(), util.MustParseUUID(issue))
	if err != nil || len(tasks) != 0 {
		t.Fatalf("no-start boundary queued %d tasks: %v", len(tasks), err)
	}
	updateIssueForTest(t, issue, map[string]any{"status": "in_progress", "assignee_type": "agent", "assignee_id": merger,
		"expected_revision": before.Revision, "suppress_run": true}).Want(http.StatusConflict)
	_, _, assigned = issueStatusAndAssignee(t, issue)
	if assigned != author {
		t.Fatal("stale custody update overwrote implementer")
	}
}

func TestMergeReturnWorkflowAdmissionResumesOriginalWorktree(t *testing.T) {
	seedTestCatalog(t)
	author, runtime, daemon := createRuntimeGuardAgent(t, t.Context())
	merger := dbfx.Agent(t, "Merger cannot own source", handlerTestRuntimeID(t))
	project := deliveryProject(t, "Guarded merge-return workflow", merger)
	pin, _ := json.Marshal(map[string]any{"flow_review_author": map[string]any{"agent_id": author, "pr": "fixture/merge-return#7", "at": "2020-01-01T00:00:00Z"}})
	issue := dbfx.Issue(t, "Real merge-return path fixture", testutil.Cols{"project_id": project, "status": "merge", "assignee_type": "agent", "assignee_id": merger, "metadata": pin})
	dbfx.Task(t, author, testutil.Cols{"issue_id": issue, "runtime_id": runtime, "status": "completed", "started_at": time.Now().Add(-time.Hour),
		"completed_at": time.Now().Add(-time.Minute), "session_id": "original-implementer-session", "work_dir": "/fixture/original-worktree"})
	pr := dbfx.Insert(t, "github_pull_request", testutil.Cols{"workspace_id": testWorkspaceID, "installation_id": 1,
		"repo_owner": "fixture", "repo_name": "merge-return", "pr_number": 7, "title": "Merge return fixture", "state": "open",
		"html_url": "https://example.test/pr/7", "head_sha": "reviewed-head", "branch": "original-branch",
		"pr_created_at": "2020-01-01T00:00:00Z", "pr_updated_at": "2020-01-01T00:00:00Z"})
	dbfx.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, pr)
	t.Cleanup(func() { testPool.Exec(context.Background(), "DELETE FROM issue_pull_request WHERE issue_id=$1", issue) })
	i, err := testHandler.Queries.GetIssue(t.Context(), util.MustParseUUID(issue))
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC()
	custody := service.MergeReturnCustody{Revision: i.Revision + 1, AgentID: author, RuntimeID: runtime, ProjectID: project, PR: "fixture/merge-return#7",
		Head: "reviewed-head", Branch: "original-branch", ObservedAt: observed, MergerID: merger}
	prepare := withURLParam(newRequest(http.MethodPut, "/api/issues/"+issue+"/merge-return", map[string]any{
		"status": "in_progress", "assignee_type": "agent", "assignee_id": author, "expected_revision": i.Revision,
		"suppress_run": true, "merge_return_custody": custody}), "id", issue)
	testutil.Call(t, testHandler.PrepareMergeReturn, prepare).Want(http.StatusOK)
	i, err = testHandler.Queries.GetIssue(t.Context(), i.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"type": "merge_return", "value": custody})
	svc := service.IssueWakeupService{Tasks: testHandler.TaskService}
	w, err := svc.Create(t.Context(), i.ID, util.MustParseUUID(testUserID), pgtype.UUID{}, service.WakeupInput{AgentID: author,
		Kind: "event", Mode: "once", Condition: raw, Instruction: "Before any edit or push assert the original agent, same branch and existing worktree"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.TickWorkspaces(t.Context(), i.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	w, err = testHandler.Queries.LocklessWakeup(t.Context(), w.ID)
	if err != nil || !w.LastTaskID.Valid {
		t.Fatalf("repair not admitted: %+v %v", w, err)
	}
	queued, err := testHandler.Queries.GetAgentTask(t.Context(), w.LastTaskID)
	if err != nil || queued.AgentID != util.MustParseUUID(author) || queued.ForceFreshSession {
		t.Fatalf("wrong repair admission: %+v %v", queued, err)
	}
	claimed := claimTaskForRuntimeGuard(t, runtime, daemon)
	if claimed.PriorSessionID != "original-implementer-session" || claimed.PriorWorkDir != "/fixture/original-worktree" {
		t.Fatalf("repair lost source custody: session=%s worktree=%s", claimed.PriorSessionID, claimed.PriorWorkDir)
	}
}
