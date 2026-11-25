package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func currentStateTestServer(t *testing.T, handler http.HandlerFunc) *[]string {
	t.Helper()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodGet && r.URL.Path == "/api/issues/"+testIssueUUID {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": testIssueUUID, "identifier": "MUL-1", "title": "test issue"})
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "test-token")
	return &paths
}

func TestRunIssueCurrentStateSetSendsFullSnapshotAndClearValues(t *testing.T) {
	var body map[string]any
	paths := currentStateTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/issues/"+testIssueUUID+"/current-state" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_, _ = io.WriteString(w, `{"current_state":{"state":"in_progress","blocker":"","next_action":"ship fix","owner":"agent-1","expiry":null,"updated_at":"2026-11-25T12:00:00Z","updated_by":{"type":"agent","id":"agent-1"},"issue_revision":8},"issue_revision":8}`)
	})
	cmd := &cobra.Command{Use: "set"}
	cmd.Flags().String("state", "in_progress", "")
	cmd.Flags().String("blocker", "", "")
	cmd.Flags().String("next-action", "ship fix", "")
	cmd.Flags().String("owner", "agent-1", "")
	cmd.Flags().String("expiry", "", "")
	cmd.Flags().String("output", "json", "")
	out, err := captureStdout(t, func() error { return runIssueCurrentStateSet(cmd, []string{testIssueUUID}) })
	if err != nil {
		t.Fatalf("runIssueCurrentStateSet: %v", err)
	}
	if body["state"] != "in_progress" || body["blocker"] != "" || body["next_action"] != "ship fix" || body["owner"] != "agent-1" || body["expiry"] != nil {
		t.Fatalf("request omitted full snapshot or clear values: %#v", body)
	}
	if !strings.Contains(out, `"issue_revision": 8`) || !strings.Contains(out, `"expiry": null`) {
		t.Fatalf("JSON output missing record/provenance: %s", out)
	}
	if len(*paths) != 1 || (*paths)[0] != "PUT /api/issues/"+testIssueUUID+"/current-state" {
		t.Fatalf("requests = %v", *paths)
	}
}

func TestRunIssueCurrentStateGetReadsTypedRecord(t *testing.T) {
	paths := currentStateTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/issues/"+testIssueUUID+"/current-state" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"current_state":{"state":"blocked","blocker":"review","next_action":"wait","owner":"agent-2","expiry":null,"updated_at":"2026-11-25T12:00:00Z","updated_by":{"type":"agent","id":"agent-2"},"issue_revision":9},"issue_revision":9}`)
	})
	cmd := &cobra.Command{Use: "get"}
	cmd.Flags().String("output", "json", "")
	out, err := captureStdout(t, func() error { return runIssueCurrentStateGet(cmd, []string{testIssueUUID}) })
	if err != nil {
		t.Fatalf("runIssueCurrentStateGet: %v", err)
	}
	if !strings.Contains(out, `"state": "blocked"`) || !strings.Contains(out, `"issue_revision": 9`) {
		t.Fatalf("JSON output missing typed record: %s", out)
	}
	if len(*paths) != 1 || (*paths)[0] != "GET /api/issues/"+testIssueUUID+"/current-state" {
		t.Fatalf("requests = %v", *paths)
	}
}

func TestRunIssueCurrentStateSetRejectsInvalidExpiryLocally(t *testing.T) {
	cmd := &cobra.Command{Use: "set"}
	cmd.Flags().String("state", "in_progress", "")
	cmd.Flags().String("blocker", "", "")
	cmd.Flags().String("next-action", "ship fix", "")
	cmd.Flags().String("owner", "agent-1", "")
	cmd.Flags().String("expiry", "tomorrow", "")
	cmd.Flags().String("output", "json", "")
	if err := runIssueCurrentStateSet(cmd, []string{testIssueUUID}); err == nil || !strings.Contains(err.Error(), "RFC3339") {
		t.Fatalf("invalid expiry error = %v", err)
	}
}
