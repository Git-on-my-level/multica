package daemon

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

// End to end through runTask: the default Claude account answers with a spent
// weekly window, so the task re-runs once under the pool's second account
// (its own launcher and config dir) and completes there.
func TestRunTaskFailsOverToNextAccountOnQuota(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script agent fixture is POSIX-only")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	capture := t.TempDir()
	t.Setenv("POOL_CAPTURE", capture)
	script := func(name, result string) string {
		path := filepath.Join(t.TempDir(), name)
		writeTestExecutable(t, path, []byte(`#!/bin/sh
case "$*" in *--version*) echo '2.1.292'; exit 0;; esac
printf '%s\n' "${CLAUDE_CONFIG_DIR:-unset}" > "$POOL_CAPTURE/`+name+`-config-dir"
IFS= read -r input
case "$input" in
  *'"type":"control_request"'*)
    printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"multica-supplement-initialize","response":{"commands":[]}}}'
    IFS= read -r input;;
esac
printf '%s\n' '`+result+`'
cat >/dev/null
`))
		return path
	}
	spent := script("claude-default", `{"type":"result","subtype":"success","is_error":true,"result":"You've hit your weekly limit · resets Nov 21","session_id":"s-default"}`)
	second := script("claude-2", `{"type":"result","subtype":"success","session_id":"s-second","result":"done"}`)
	configDir := filepath.Join(t.TempDir(), ".claude-2")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	statePath := filepath.Join(t.TempDir(), "state.json")
	d := &Daemon{
		client: NewClient(srv.URL), logger: logger,
		workspaces: make(map[string]*workspaceState), activeEnvRoots: make(map[string]int),
		runtimeIndex: map[string]Runtime{"test-rt": {ID: "test-rt", Provider: "claude"}},
		cfg: Config{
			WorkspacesRoot: t.TempDir(), AgentTimeout: 5 * time.Second, ServerBaseURL: srv.URL,
			Agents: map[string]AgentEntry{"claude": {Path: spent}},
		},
		accountPool: newAccountPool(&cli.AccountPoolsConfig{Accounts: map[string][]cli.AccountSpec{
			"claude": {{ID: "claude-2", Executable: second, ConfigDir: configDir}},
		}}, statePath, logger),
	}
	task := Task{
		ID: "pool-task", WorkspaceID: "test-ws", RuntimeID: "test-rt",
		AgentID: "test-agent", AuthToken: "mat_test_pool", ChatSessionID: "test-chat",
		ChatMessage: "hello",
		Agent:       &AgentData{ID: "test-agent", Name: "test"},
	}
	res, err := d.runTask(t.Context(), task, "claude", 0, logger)
	if err != nil || res.Status != "completed" || res.SessionID != "s-second" {
		t.Fatalf("run = %+v, err = %v", res, err)
	}
	got, err := os.ReadFile(filepath.Join(capture, "claude-2-config-dir"))
	if err != nil || strings.TrimSpace(string(got)) != configDir {
		t.Fatalf("second account ran with CLAUDE_CONFIG_DIR %q (%v), want %q", got, err, configDir)
	}
	if got, _ := os.ReadFile(filepath.Join(capture, "claude-default-config-dir")); strings.TrimSpace(string(got)) != "unset" {
		t.Fatalf("default account ran with CLAUDE_CONFIG_DIR %q", got)
	}

	// The spent account stays out of the way for the next task.
	next := d.selectTaskAccount("claude", &Task{}, false, logger)
	if next == nil || next.spec.ID != "claude-2" {
		t.Fatalf("next task account = %+v, want claude-2", next)
	}
	if b := d.accountPool.state.Sessions[accountKey("claude", "s-second")]; b.Account != "claude-2" {
		t.Fatalf("session binding = %+v", b)
	}
}

// A resumed run whose resume is rejected retries fresh; when that fresh run
// hits the plan limit, the quota failure still drives failover even though
// the kept result is the rejected resume.
func TestRunTaskFailsOverWhenFreshRetryHitsQuota(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script agent fixture is POSIX-only")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	write := func(name, body string) string {
		path := filepath.Join(t.TempDir(), name)
		writeTestExecutable(t, path, []byte(`#!/bin/sh
case "$*" in *--version*) echo '2.1.292'; exit 0;; esac
IFS= read -r input
case "$input" in
  *'"type":"control_request"'*)
    printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"multica-supplement-initialize","response":{"commands":[]}}}'
    IFS= read -r input;;
esac
`+body+`
cat >/dev/null
`))
		return path
	}
	spent := write("claude-default", `case "$*" in
  *--resume*) printf '%s\n' 'No conversation found with session ID: old-session' >&2
    printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true}';;
  *) printf '%s\n' '{"type":"result","subtype":"success","is_error":true,"result":"You'"'"'ve hit your session limit · resets 2pm (UTC)"}';;
esac`)
	second := write("claude-2", `printf '%s\n' '{"type":"result","subtype":"success","session_id":"s-second","result":"done"}'`)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	d := &Daemon{
		client: NewClient(srv.URL), logger: logger,
		workspaces: make(map[string]*workspaceState), activeEnvRoots: make(map[string]int),
		runtimeIndex: map[string]Runtime{"test-rt": {ID: "test-rt", Provider: "claude"}},
		cfg: Config{
			WorkspacesRoot: t.TempDir(), AgentTimeout: 5 * time.Second, ServerBaseURL: srv.URL,
			Agents: map[string]AgentEntry{"claude": {Path: spent}},
		},
		accountPool: newAccountPool(&cli.AccountPoolsConfig{Accounts: map[string][]cli.AccountSpec{
			"claude": {{ID: "claude-2", Executable: second}},
		}}, "", logger),
	}
	task := Task{
		ID: "pool-task-2", WorkspaceID: "test-ws", RuntimeID: "test-rt",
		AgentID: "test-agent", AuthToken: "mat_test_pool", ChatSessionID: "test-chat",
		ChatMessage: "hello", PriorSessionID: "old-session",
		Agent: &AgentData{ID: "test-agent", Name: "test"},
	}
	res, err := d.runTask(t.Context(), task, "claude", 0, logger)
	if err != nil || res.Status != "completed" || res.SessionID != "s-second" {
		t.Fatalf("run = %+v, err = %v", res, err)
	}
}
