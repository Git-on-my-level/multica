package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const coordinationIssueID = "11111111-1111-1111-1111-111111111111"

func setCoordinationTestEnv(t *testing.T, serverURL string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MULTICA_SERVER_URL", serverURL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test")
	t.Setenv("MULTICA_APP_URL", "https://multica.example.test/base")
	t.Setenv("FRONTEND_ORIGIN", "")
}

func coordinationTestCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("output", "json", "")
	return cmd
}

func TestRunIssueURLUsesConfiguredAppURLAndWorkspaceSlug(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/issues/" + coordinationIssueID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": coordinationIssueID, "identifier": "SCA-112",
			})
		case "/api/workspaces/ws-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "ws-1", "slug": "acme",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	setCoordinationTestEnv(t, srv.URL)

	cmd := coordinationTestCommand()
	out, err := captureStdout(t, func() error {
		return runIssueURL(cmd, []string{coordinationIssueID})
	})
	if err != nil {
		t.Fatalf("runIssueURL: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if got["url"] != "https://multica.example.test/base/acme/issues/"+coordinationIssueID {
		t.Fatalf("url = %q", got["url"])
	}
	if got["identifier"] != "SCA-112" {
		t.Fatalf("identifier = %q", got["identifier"])
	}
}

func TestRunIssueURLRequiresConfiguredAppURL(t *testing.T) {
	setCoordinationTestEnv(t, "https://api.example.test")
	t.Setenv("MULTICA_APP_URL", "")
	cmd := coordinationTestCommand()
	err := runIssueURL(cmd, []string{coordinationIssueID})
	if err == nil || !strings.Contains(err.Error(), "app URL not set") {
		t.Fatalf("error = %v, want configured app URL guidance", err)
	}
}

func TestRunIssueURLRejectsWorkspaceWithoutSlug(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/issues/" + coordinationIssueID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": coordinationIssueID, "identifier": "SCA-112",
			})
		case "/api/workspaces/ws-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "ws-1", "slug": " "})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	setCoordinationTestEnv(t, srv.URL)

	err := runIssueURL(coordinationTestCommand(), []string{coordinationIssueID})
	if err == nil || !strings.Contains(err.Error(), "no URL slug") {
		t.Fatalf("error = %v, want missing workspace slug guidance", err)
	}
}

func TestRunIssueInspectReturnsServerOwnedSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/issues/" + coordinationIssueID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": coordinationIssueID, "identifier": "SCA-112",
			})
		case "/api/issues/" + coordinationIssueID + "/inspect":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issue":       map[string]any{"id": coordinationIssueID},
				"active_runs": []any{},
				"pr_handoff":  map[string]any{"state": "awaiting_mirror"},
				"safe_actions": map[string]any{
					"rerun": map[string]any{"allowed": false, "reason_code": "active_or_unroutable"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	setCoordinationTestEnv(t, srv.URL)

	out, err := captureStdout(t, func() error {
		return runIssueInspect(coordinationTestCommand(), []string{coordinationIssueID})
	})
	if err != nil {
		t.Fatalf("runIssueInspect: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	handoff := got["pr_handoff"].(map[string]any)
	if handoff["state"] != "awaiting_mirror" {
		t.Fatalf("pr_handoff = %#v", handoff)
	}
}

func TestRunIssueRouteIsReadOnlyAndReturnsDecision(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/issues/route" {
			t.Fatalf("request = %s %s, want advisory route endpoint", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"decision":         "use_existing_issue",
			"existing_issue":   map[string]any{"id": coordinationIssueID},
			"candidate_agents": []any{},
		})
	}))
	defer srv.Close()
	setCoordinationTestEnv(t, srv.URL)

	cmd := coordinationTestCommand()
	cmd.Flags().String("title", "", "")
	cmd.Flags().String("description", "", "")
	cmd.Flags().String("repo", "", "")
	cmd.Flags().String("project", "", "")
	_ = cmd.Flags().Set("title", "Do the thing")
	_ = cmd.Flags().Set("repo", "https://github.com/acme/widget.git")
	out, err := captureStdout(t, func() error { return runIssueRoute(cmd, nil) })
	if err != nil {
		t.Fatalf("runIssueRoute: %v", err)
	}
	if body["title"] != "Do the thing" || body["repository"] != "https://github.com/acme/widget.git" {
		t.Fatalf("request body = %#v", body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if got["decision"] != "use_existing_issue" {
		t.Fatalf("decision = %q", got["decision"])
	}
}

func TestRunCoordinationContextRedactsAgentConfiguration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspaces/ws-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "ws-1", "slug": "acme", "name": "Acme",
				"repos": []any{map[string]any{"url": "https://github.com/acme/widget.git"}},
			})
		case "/api/projects":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"projects": []any{map[string]any{
					"id": "project-1", "title": "Widget", "description": "Ship it",
				}},
			})
		case "/api/projects/project-1/resources":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resources": []any{map[string]any{
					"id": "resource-1", "resource_type": "github_repo",
					"resource_ref": map[string]any{
						"url": "https://github.com/acme/widget.git", "unexpected_secret": "do-not-return",
					},
				}},
			})
		case "/api/agents":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{
				"id": "agent-1", "name": "Builder", "status": "active",
				"runtime_id": "runtime-1", "max_concurrent_tasks": 2,
				"instructions": "private instructions", "custom_env": map[string]any{"TOKEN": "secret"},
			}})
		case "/api/agent-task-snapshot":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{
				"agent_id": "agent-1", "status": "running",
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	setCoordinationTestEnv(t, srv.URL)

	out, err := captureStdout(t, func() error {
		return runCoordinationContext(coordinationTestCommand(), nil)
	})
	if err != nil {
		t.Fatalf("runCoordinationContext: %v", err)
	}
	for _, forbidden := range []string{"private instructions", "do-not-return", `"TOKEN"`, `"custom_env"`} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("context leaked %q: %s", forbidden, out)
		}
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	agents := got["agents"].([]any)
	agent := agents[0].(map[string]any)
	if agent["available"] != true || agent["active_task_count"] != float64(1) {
		t.Fatalf("agent availability = %#v", agent)
	}
}
