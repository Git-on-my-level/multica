package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMergeReturnUpdateSendsRevisionAndSuppressesRun(t *testing.T) {
	t.Chdir(t.TempDir())
	const issue = "11111111-1111-4111-8111-111111111111"
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/api/issues/"+issue+"/merge-return" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": issue})
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	cmd := newIssueUpdateTestCmd()
	cmd.Flags().Int64("expected-revision", 0, "")
	cmd.Flags().String("merge-return-custody", "", "")
	_ = cmd.Flags().Set("expected-revision", "7")
	_ = cmd.Flags().Set("no-start", "true")
	_ = cmd.Flags().Set("status", "in_progress")
	_ = cmd.Flags().Set("merge-return-custody", `{"revision":8}`)
	if err := runIssueUpdate(cmd, []string{issue}); err != nil {
		t.Fatal(err)
	}
	if body["expected_revision"] != float64(7) || body["suppress_run"] != true {
		t.Fatalf("unsafe update: %#v", body)
	}
}

func TestMergeReturnCLITransmitsServerCondition(t *testing.T) {
	t.Chdir(t.TempDir())
	const issue = "11111111-1111-4111-8111-111111111111"
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "guard", "kind": "event", "mode": "once"})
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	cmd := newIssueWakeupCommand()
	cmd.SetArgs([]string{"create", issue, "--mode", "once", "--until-merge-return", `{"revision":8}`, "--instruction", "assert custody before edit"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	c, ok := body["condition"].(map[string]any)
	if !ok || c["type"] != "merge_return" {
		t.Fatalf("unguarded request: %#v", body)
	}
}

func TestMergeReturnCLIRefusesMixedConditions(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("mixed conditions reached the server") }))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	cmd := newIssueWakeupCommand()
	cmd.SetArgs([]string{"create", "11111111-1111-4111-8111-111111111111", "--mode", "once", "--until-merge-return", `{"revision":8}`, "--until-status", "in_progress", "--instruction", "assert custody"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "use one --until-*") {
		t.Fatalf("mixed condition accepted: %v", err)
	}
}
