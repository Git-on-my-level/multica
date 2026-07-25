package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestInspectIssueCoordinationIncludesMissingHandoffAndSafeActions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := createManualLinkIssue(t, "in_progress")
	req := withURLParam(
		newRequest("GET", "/api/issues/"+issueID+"/inspect", nil),
		"id",
		issueID,
	)
	w := httptest.NewRecorder()
	testHandler.InspectIssueCoordination(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("InspectIssueCoordination: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		ActiveRuns []any `json:"active_runs"`
		PRHandoff  struct {
			State string `json:"state"`
		} `json:"pr_handoff"`
		SafeActions map[string]safeActionSignal `json:"safe_actions"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.ActiveRuns) != 0 {
		t.Fatalf("active_runs = %#v, want empty", got.ActiveRuns)
	}
	if got.PRHandoff.State != "missing" {
		t.Fatalf("pr_handoff.state = %q, want missing", got.PRHandoff.State)
	}
	if got.SafeActions["create_duplicate"].Allowed {
		t.Fatal("inspect must not recommend creating a duplicate of the issue being inspected")
	}
}

func TestInspectIssueCoordinationDisallowsTerminalActions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issueID := createManualLinkIssue(t, "done")
	req := withURLParam(
		newRequest("GET", "/api/issues/"+issueID+"/inspect", nil),
		"id",
		issueID,
	)
	w := httptest.NewRecorder()
	testHandler.InspectIssueCoordination(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("InspectIssueCoordination: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		SafeActions map[string]safeActionSignal `json:"safe_actions"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	for _, action := range []string{"rerun", "reassign"} {
		if got.SafeActions[action].Allowed {
			t.Fatalf("%s must not be allowed for a terminal issue", action)
		}
	}
	if got.SafeActions["reassign"].ReasonCode != "terminal_issue" {
		t.Fatalf("reassign reason = %q, want terminal_issue", got.SafeActions["reassign"].ReasonCode)
	}
}

var coordinationRouteSeq uint64

func TestRouteIssueCoordinationIsAdvisoryAndDetectsExactDuplicate(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	title := "Coordination route " + strconv.FormatUint(
		atomic.AddUint64(&coordinationRouteSeq, 1),
		10,
	) + " " + time.Now().UTC().Format("150405.000000000")
	created := httptest.NewRecorder()
	testHandler.CreateIssue(created, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": title, "status": "in_progress",
	}))
	if created.Code != http.StatusCreated {
		t.Fatalf("CreateIssue: %d %s", created.Code, created.Body.String())
	}
	var issue IssueResponse
	if err := json.NewDecoder(created.Body).Decode(&issue); err != nil {
		t.Fatalf("decode issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM activity_log WHERE issue_id = $1`, issue.ID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issue.ID)
	})

	var before int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue WHERE workspace_id = $1`, testWorkspaceID,
	).Scan(&before); err != nil {
		t.Fatalf("count issues before route: %v", err)
	}
	req := newRequest("POST", "/api/issues/route?workspace_id="+testWorkspaceID, map[string]any{
		"title": title,
	})
	w := httptest.NewRecorder()
	testHandler.RouteIssueCoordination(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("RouteIssueCoordination: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Decision       string          `json:"decision"`
		ExistingIssue  *IssueResponse  `json:"existing_issue"`
		MatchingIssues []IssueResponse `json:"matching_issues"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode route response: %v", err)
	}
	if got.Decision != "use_existing_issue" {
		t.Fatalf("decision = %q, want use_existing_issue", got.Decision)
	}
	if got.ExistingIssue == nil || got.ExistingIssue.ID != issue.ID || len(got.MatchingIssues) != 1 {
		t.Fatalf("duplicate evidence = %#v", got)
	}
	var after int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue WHERE workspace_id = $1`, testWorkspaceID,
	).Scan(&after); err != nil {
		t.Fatalf("count issues after route: %v", err)
	}
	if after != before {
		t.Fatalf("route mutated issue count: before=%d after=%d", before, after)
	}
}
