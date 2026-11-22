package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

func TestIssueProjectArgvProcess(t *testing.T) {
	if os.Getenv("MULTICA_TEST_PROJECT_ARGV") != "1" {
		return
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		t.Fatal("missing CLI argv")
	}
	t.Setenv("MULTICA_SERVER_URL", os.Getenv("MULTICA_TEST_PROJECT_SERVER"))
	t.Setenv("MULTICA_WORKSPACE_ID", os.Getenv("MULTICA_TEST_PROJECT_WORKSPACE"))
	t.Setenv("MULTICA_TOKEN", "mat_SYNTHETIC_ONLY")
	rootCmd.SetArgs(os.Args[separator+1:])
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, cli.FormatError(err, false))
		os.Exit(cli.ExitCodeFor(err))
	}
	os.Exit(0)
}

func TestIssueListProjectSelectorArgv(t *testing.T) {
	const (
		workspaceID = "11111111-1111-4111-8111-111111111111"
		projectID   = "09792dfd-e349-4aee-a0b4-2ea1c6cc4ab0"
		archivedID  = "22222222-2222-4222-8222-222222222222"
		foreignID   = "33333333-3333-4333-8333-333333333333"
	)
	project := map[string]any{"id": projectID, "title": "Goal: Churn"}
	archived := map[string]any{"id": archivedID, "title": "Goal: Churn", "archived_at": "2026-11-01T00:00:00Z"}
	prefixTitle := map[string]any{"id": archivedID, "title": "09792DFD-e349"}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, selector, offset, wantError string
		pages                             [][]map[string]any
		wantPaths                         []string
	}{
		{name: "documented invocation", selector: "Goal: Churn", pages: [][]map[string]any{{project}}, wantPaths: []string{"/api/projects", "/api/issues"}},
		{name: "next issue page", selector: "Goal: Churn", offset: "1", pages: [][]map[string]any{{project}}, wantPaths: []string{"/api/projects", "/api/issues"}},
		{name: "title on later catalog page", selector: "Goal: Churn", pages: [][]map[string]any{{{"id": archivedID, "title": "Another project"}}, {project}}, wantPaths: []string{"/api/projects", "/api/projects", "/api/issues"}},
		{name: "full UUID", selector: projectID, wantPaths: []string{"/api/issues"}},
		{name: "UUID prefix takes precedence over title", selector: "09792DFD-e349", pages: [][]map[string]any{{project, prefixTitle}}, wantPaths: []string{"/api/projects", "/api/issues"}},
		{name: "unknown", selector: "Goal: Retention", pages: [][]map[string]any{{project}}, wantError: "no visible project with exact title", wantPaths: []string{"/api/projects"}},
		{name: "partial", selector: "Goal: Ch", pages: [][]map[string]any{{project}}, wantError: "no visible project with exact title", wantPaths: []string{"/api/projects"}},
		{name: "case-sensitive", selector: "goal: churn", pages: [][]map[string]any{{project}}, wantError: "no visible project with exact title", wantPaths: []string{"/api/projects"}},
		{name: "duplicate across pages including archived", selector: "Goal: Churn", pages: [][]map[string]any{{project}, {archived}}, wantError: "ambiguous project title", wantPaths: []string{"/api/projects", "/api/projects"}},
		{name: "wrong workspace", selector: "Other Workspace: Churn", pages: [][]map[string]any{{project}}, wantError: "no visible project with exact title", wantPaths: []string{"/api/projects"}},
		{name: "inaccessible", selector: "Restricted: Churn", pages: [][]map[string]any{{project}}, wantError: "no visible project with exact title", wantPaths: []string{"/api/projects"}},
		{name: "foreign UUID prefix", selector: foreignID[:8], pages: [][]map[string]any{{project}}, wantError: "no project found matching id prefix", wantPaths: []string{"/api/projects"}},
		{name: "empty", wantError: "project selector is required"},
		{name: "whitespace", selector: "  ", wantError: "project selector is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var paths []string
			projectPage := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				paths = append(paths, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				query := r.URL.Query()
				if r.Method != http.MethodGet || r.Header.Get("X-Workspace-ID") != workspaceID || query.Get("workspace_id") != workspaceID {
					t.Errorf("unexpected request: %s %s (workspace header %q)", r.Method, r.URL, r.Header.Get("X-Workspace-ID"))
				}
				switch r.URL.Path {
				case "/api/projects":
					if projectPage >= len(tc.pages) {
						t.Errorf("unexpected catalog request: %s", r.URL)
						http.NotFound(w, r)
						return
					}
					wantOffset := ""
					if projectPage > 0 {
						wantOffset = "1"
					}
					if query.Get("include_archived") != "true" || query.Get("offset") != wantOffset || query.Has("q") {
						t.Errorf("unexpected catalog query: %s", r.URL)
					}
					projects := tc.pages[projectPage]
					projectPage++
					_ = json.NewEncoder(w).Encode(map[string]any{"projects": projects, "has_more": projectPage < len(tc.pages)})
				case "/api/issues":
					if query.Get("project_id") != projectID || query.Get("limit") != "1" || query.Get("offset") != tc.offset {
						t.Errorf("unexpected issue query: %s", r.URL)
					}
					identifier := "MUL-1"
					if tc.offset != "" {
						identifier = "MUL-2"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"issues": []map[string]any{{"identifier": identifier, "project_id": projectID}},
						"total":  2,
					})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			t.Setenv("MULTICA_TEST_PROJECT_ARGV", "1")
			t.Setenv("MULTICA_TEST_PROJECT_SERVER", server.URL)
			t.Setenv("MULTICA_TEST_PROJECT_WORKSPACE", workspaceID)
			// The child exits with the CLI's status. Let the parent clean up
			// the scratch home that TestMain creates under its temp directory.
			t.Setenv("TMPDIR", t.TempDir())
			argv := []string{"-test.run=^TestIssueProjectArgvProcess$", "--", "issue", "list",
				"--project", tc.selector, "--limit", "1", "--output", "json"}
			if tc.offset != "" {
				argv = append(argv, "--offset", tc.offset)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, executable, argv...)
			var stderr strings.Builder
			child.Stderr = &stderr
			stdout, runErr := child.Output()
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(paths, tc.wantPaths) {
				t.Errorf("requests = %v, want %v", paths, tc.wantPaths)
			}
			if tc.wantError != "" {
				if runErr == nil || len(stdout) != 0 || !strings.Contains(stderr.String(), tc.wantError) {
					t.Fatalf("exit = %v, stdout = %q, stderr = %q; want diagnostic %q on stderr only", runErr, stdout, stderr.String(), tc.wantError)
				}
				return
			}
			if runErr != nil || stderr.Len() != 0 {
				t.Fatalf("issue list: %v, stderr = %q", runErr, stderr.String())
			}
			wantJSON := fmt.Sprintf(`{"issues":[{"identifier":"MUL-1","project_id":%q}],"total":2,"limit":1,"offset":0,"has_more":true}`, projectID)
			if tc.offset != "" {
				wantJSON = fmt.Sprintf(`{"issues":[{"identifier":"MUL-2","project_id":%q}],"total":2,"limit":1,"offset":1,"has_more":false}`, projectID)
			}
			var got, want any
			if err := json.Unmarshal(stdout, &got); err != nil {
				t.Fatalf("decode stdout %q: %v", stdout, err)
			}
			if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("stdout = %s, want %s", stdout, wantJSON)
			}
		})
	}
}
