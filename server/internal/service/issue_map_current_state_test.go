package service

import (
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestIssueToMapKeepsMetadataPrimitiveAndProjectsCurrentState(t *testing.T) {
	issue := db.Issue{Metadata: []byte(`{"pipeline_status":"testing","current_state":{"state":"blocked","blocker":"waiting","next_action":"review","owner":"agent-1","expiry":null,"updated_at":"2026-11-25T12:00:00Z","updated_by":{"type":"agent","id":"agent-1"},"issue_revision":4}}`)}
	got := IssueToMap(issue, "SCA")
	metadata := got["metadata"].(map[string]any)
	if metadata["pipeline_status"] != "testing" {
		t.Fatalf("metadata lost primitive values: %#v", metadata)
	}
	if _, ok := metadata["current_state"]; ok {
		t.Fatalf("structured value leaked into primitive-only metadata: %#v", metadata)
	}
	state, ok := got["current_state"].(map[string]any)
	if !ok || state["state"] != "blocked" {
		t.Fatalf("current_state projection = %#v", got["current_state"])
	}
}

func TestIssueToMapPreservesLegacyPrimitiveCurrentState(t *testing.T) {
	issue := db.Issue{Metadata: []byte(`{"current_state":"legacy-value"}`)}
	got := IssueToMap(issue, "SCA")
	metadata := got["metadata"].(map[string]any)
	if metadata["current_state"] != "legacy-value" {
		t.Fatalf("legacy primitive metadata changed: %#v", metadata)
	}
	if got["current_state"] != nil {
		t.Fatalf("legacy primitive was projected as structured state: %#v", got["current_state"])
	}
}
