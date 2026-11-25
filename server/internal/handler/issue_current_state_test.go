package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIssueCurrentStateFullSnapshotAndIssueGet(t *testing.T) {
	issueID := createMetadataTestIssue(t, "Current state round trip")
	set := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := newRequest("PUT", "/api/issues/"+issueID+"/current-state", json.RawMessage(body))
		req = withURLParam(req, "id", issueID)
		testHandler.SetIssueCurrentState(w, req)
		return w
	}
	first := set(`{"state":"blocked","blocker":"waiting for review","next_action":"check the PR","owner":"agent-1","expiry":"2026-12-01T12:00:00Z"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first set: status=%d body=%s", first.Code, first.Body.String())
	}
	var firstResponse issueCurrentStateResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstResponse); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if firstResponse.CurrentState == nil || firstResponse.CurrentState.Blocker != "waiting for review" || firstResponse.CurrentState.Expiry == nil {
		t.Fatalf("first current state = %+v", firstResponse.CurrentState)
	}
	if firstResponse.CurrentState.UpdatedAt == "" || firstResponse.CurrentState.UpdatedBy.ID == "" || firstResponse.CurrentState.IssueRevision != firstResponse.IssueRevision {
		t.Fatalf("missing update provenance: %+v", firstResponse.CurrentState)
	}

	// A second full snapshot clears both optional live fields explicitly; it
	// cannot inherit the prior blocker or expiry from the stored JSON object.
	second := set(`{"state":"in_progress","blocker":"","next_action":"implement the fix","owner":"agent-1","expiry":null}`)
	if second.Code != http.StatusOK {
		t.Fatalf("second set: status=%d body=%s", second.Code, second.Body.String())
	}
	var secondResponse issueCurrentStateResponse
	if err := json.Unmarshal(second.Body.Bytes(), &secondResponse); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if secondResponse.CurrentState == nil || secondResponse.CurrentState.Blocker != "" || secondResponse.CurrentState.Expiry != nil {
		t.Fatalf("cleared current state = %+v", secondResponse.CurrentState)
	}
	if secondResponse.IssueRevision <= firstResponse.IssueRevision {
		t.Fatalf("revision did not advance: first=%d second=%d", firstResponse.IssueRevision, secondResponse.IssueRevision)
	}

	get := httptest.NewRecorder()
	req := newRequest("GET", "/api/issues/"+issueID, nil)
	req = withURLParam(req, "id", issueID)
	testHandler.GetIssue(get, req)
	if get.Code != http.StatusOK {
		t.Fatalf("issue get: status=%d body=%s", get.Code, get.Body.String())
	}
	var issue struct {
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &issue); err != nil {
		t.Fatalf("decode issue get: %v", err)
	}
	stored, ok := issue.Metadata[issueCurrentStateMetadataKey].(map[string]any)
	if !ok {
		t.Fatalf("issue get omitted structured metadata.current_state: %#v", issue.Metadata)
	}
	if stored["blocker"] != "" || stored["expiry"] != nil || stored["state"] != "in_progress" {
		t.Fatalf("issue get current state has stale fields: %#v", stored)
	}
}

func TestIssueCurrentStateReadWithoutRecordAndValidation(t *testing.T) {
	issueID := createMetadataTestIssue(t, "Issue without current state")
	get := httptest.NewRecorder()
	req := newRequest("GET", "/api/issues/"+issueID+"/current-state", nil)
	req = withURLParam(req, "id", issueID)
	testHandler.GetIssueCurrentState(get, req)
	var emptyResponse issueCurrentStateResponse
	if get.Code != http.StatusOK || json.Unmarshal(get.Body.Bytes(), &emptyResponse) != nil || emptyResponse.CurrentState != nil || emptyResponse.IssueRevision != 1 {
		t.Fatalf("empty state response: status=%d body=%s", get.Code, get.Body.String())
	}

	for _, body := range []string{
		`{"state":"blocked","blocker":"x","next_action":"y","owner":"z"}`,
		`{"state":"blocked","blocker":"x","next_action":"y","owner":"z","expiry":"tomorrow"}`,
		`{"state":" ","blocker":"x","next_action":"y","owner":"z","expiry":null}`,
		`{"state":"blocked","blocker":"x","next_action":"y","owner":"z","expiry":null,"extra":true}`,
	} {
		w := httptest.NewRecorder()
		req := newRequest("PUT", "/api/issues/"+issueID+"/current-state", json.RawMessage(body))
		req = withURLParam(req, "id", issueID)
		testHandler.SetIssueCurrentState(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status=%d body=%s, want 400", body, w.Code, w.Body.String())
		}
	}

	reserved := httptest.NewRecorder()
	reservedReq := newRequest("PUT", "/api/issues/"+issueID+"/metadata/current_state", json.RawMessage(`{"value":"bad"}`))
	reservedReq = withURLParams(reservedReq, "id", issueID, "key", "current_state")
	testHandler.SetIssueMetadataKey(reserved, reservedReq)
	if reserved.Code != http.StatusBadRequest {
		t.Fatalf("generic metadata wrote reserved key: status=%d body=%s", reserved.Code, reserved.Body.String())
	}
}
