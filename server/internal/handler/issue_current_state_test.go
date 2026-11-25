package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
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
		Metadata     map[string]any     `json:"metadata"`
		CurrentState *IssueCurrentState `json:"current_state"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &issue); err != nil {
		t.Fatalf("decode issue get: %v", err)
	}
	stored := issue.CurrentState
	if stored == nil {
		t.Fatalf("issue get omitted additive current_state sibling: %#v", issue)
	}
	if stored.Blocker != "" || stored.Expiry != nil || stored.State != "in_progress" {
		t.Fatalf("issue get current state has stale fields: %+v", stored)
	}
	if _, ok := issue.Metadata[issueCurrentStateMetadataKey]; ok {
		t.Fatalf("structured value leaked into primitive-only metadata: %#v", issue.Metadata)
	}

	legacySet := httptest.NewRecorder()
	legacyReq := newRequest("PUT", "/api/issues/"+issueID+"/metadata/current_state", json.RawMessage(`{"value":"old-client-value"}`))
	legacyReq = withURLParams(legacyReq, "id", issueID, "key", issueCurrentStateMetadataKey)
	testHandler.SetIssueMetadataKey(legacySet, legacyReq)
	if legacySet.Code != http.StatusBadRequest {
		t.Fatalf("generic setter replaced structured current state: status=%d body=%s", legacySet.Code, legacySet.Body.String())
	}

	legacyDelete := httptest.NewRecorder()
	legacyDeleteReq := newRequest("DELETE", "/api/issues/"+issueID+"/metadata/current_state", nil)
	legacyDeleteReq = withURLParams(legacyDeleteReq, "id", issueID, "key", issueCurrentStateMetadataKey)
	testHandler.DeleteIssueMetadataKey(legacyDelete, legacyDeleteReq)
	if legacyDelete.Code != http.StatusBadRequest {
		t.Fatalf("generic deleter removed structured current state: status=%d body=%s", legacyDelete.Code, legacyDelete.Body.String())
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

	legacyWrite := httptest.NewRecorder()
	legacyWriteReq := newRequest("PUT", "/api/issues/"+issueID+"/metadata/current_state", json.RawMessage(`{"value":"legacy"}`))
	legacyWriteReq = withURLParams(legacyWriteReq, "id", issueID, "key", issueCurrentStateMetadataKey)
	testHandler.SetIssueMetadataKey(legacyWrite, legacyWriteReq)
	if legacyWrite.Code != http.StatusOK {
		t.Fatalf("generic primitive write compatibility: status=%d body=%s", legacyWrite.Code, legacyWrite.Body.String())
	}
}

func TestIssueCurrentStatePreservesLegacyPrimitiveMetadata(t *testing.T) {
	issueID := createMetadataTestIssue(t, "Legacy primitive current state")
	if _, err := testPool.Exec(context.Background(), `
		UPDATE issue SET metadata = jsonb_set(metadata, '{current_state}', '"legacy-value"'::jsonb, true)
		WHERE id = $1::uuid
	`, issueID); err != nil {
		t.Fatalf("seed legacy current_state metadata: %v", err)
	}

	getState := httptest.NewRecorder()
	getStateReq := newRequest("GET", "/api/issues/"+issueID+"/current-state", nil)
	getStateReq = withURLParam(getStateReq, "id", issueID)
	testHandler.GetIssueCurrentState(getState, getStateReq)
	var stateResponse issueCurrentStateResponse
	if getState.Code != http.StatusOK || json.Unmarshal(getState.Body.Bytes(), &stateResponse) != nil || stateResponse.CurrentState != nil {
		t.Fatalf("legacy primitive current-state read: status=%d body=%s", getState.Code, getState.Body.String())
	}

	getIssue := httptest.NewRecorder()
	getIssueReq := newRequest("GET", "/api/issues/"+issueID, nil)
	getIssueReq = withURLParam(getIssueReq, "id", issueID)
	testHandler.GetIssue(getIssue, getIssueReq)
	var issue struct {
		Metadata     map[string]any     `json:"metadata"`
		CurrentState *IssueCurrentState `json:"current_state"`
	}
	if err := json.Unmarshal(getIssue.Body.Bytes(), &issue); err != nil {
		t.Fatalf("decode legacy issue response: %v", err)
	}
	if issue.Metadata[issueCurrentStateMetadataKey] != "legacy-value" || issue.CurrentState != nil {
		t.Fatalf("legacy primitive response changed shape: %+v", issue)
	}

	setLegacy := httptest.NewRecorder()
	setReq := newRequest("PUT", "/api/issues/"+issueID+"/metadata/current_state", json.RawMessage(`{"value":"updated-legacy"}`))
	setReq = withURLParams(setReq, "id", issueID, "key", issueCurrentStateMetadataKey)
	testHandler.SetIssueMetadataKey(setLegacy, setReq)
	if setLegacy.Code != http.StatusOK {
		t.Fatalf("update legacy primitive: status=%d body=%s", setLegacy.Code, setLegacy.Body.String())
	}
	var updatedLegacy struct {
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(setLegacy.Body.Bytes(), &updatedLegacy); err != nil {
		t.Fatalf("decode updated legacy metadata: %v", err)
	}
	if updatedLegacy.Metadata[issueCurrentStateMetadataKey] != "updated-legacy" {
		t.Fatalf("legacy primitive update lost value: %#v", updatedLegacy.Metadata)
	}

	deleteLegacy := httptest.NewRecorder()
	deleteReq := newRequest("DELETE", "/api/issues/"+issueID+"/metadata/current_state", nil)
	deleteReq = withURLParams(deleteReq, "id", issueID, "key", issueCurrentStateMetadataKey)
	testHandler.DeleteIssueMetadataKey(deleteLegacy, deleteReq)
	if deleteLegacy.Code != http.StatusOK {
		t.Fatalf("delete legacy primitive: status=%d body=%s", deleteLegacy.Code, deleteLegacy.Body.String())
	}
	var deletedLegacy struct {
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(deleteLegacy.Body.Bytes(), &deletedLegacy); err != nil {
		t.Fatalf("decode deleted legacy metadata: %v", err)
	}
	if _, ok := deletedLegacy.Metadata[issueCurrentStateMetadataKey]; ok {
		t.Fatalf("legacy primitive delete retained value: %#v", deletedLegacy.Metadata)
	}
}

func TestIssueListRowProjectsCurrentStateOutsideMetadata(t *testing.T) {
	row := db.ListIssuesRow{Metadata: []byte(`{"pipeline_status":"testing","current_state":{"state":"blocked","blocker":"waiting","next_action":"review","owner":"agent-1","expiry":null,"updated_at":"2026-11-25T12:00:00Z","updated_by":{"type":"agent","id":"agent-1"},"issue_revision":4}}`)}
	response := issueListRowToResponse(row, "SCA")
	if response.Metadata["pipeline_status"] != "testing" {
		t.Fatalf("list metadata lost primitive values: %#v", response.Metadata)
	}
	if _, ok := response.Metadata[issueCurrentStateMetadataKey]; ok {
		t.Fatalf("structured value leaked into primitive-only list metadata: %#v", response.Metadata)
	}
	if response.CurrentState == nil || response.CurrentState.State != "blocked" {
		t.Fatalf("list response omitted structured current state: %+v", response.CurrentState)
	}
}
