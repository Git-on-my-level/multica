package handler

import (
	"net/http"
	"testing"

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
