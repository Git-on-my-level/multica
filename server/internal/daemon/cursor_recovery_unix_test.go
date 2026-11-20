//go:build unix

package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

func TestRunTaskCursorRecoveryNeverStartsCold(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		require, lostWorkdir bool
		wantLaunches         int
	}{
		{name: "resume rejection stays terminal for recovery", require: true, wantLaunches: 1},
		{name: "lost workdir prevents recovery launch", require: true, lostWorkdir: true},
		{name: "ordinary task keeps cold fallback", wantLaunches: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectTestHome(t, t.TempDir())
			t.Setenv("MULTICA_TASK_CONFIG_ROOT", "")
			root := t.TempDir()
			attempts := filepath.Join(root, "attempts")
			fakeCursor := filepath.Join(root, "cursor-agent")
			writeTestExecutable(t, fakeCursor, []byte(`#!/bin/sh
case "$1" in
  --version|-v) printf '1.0.0\n'; exit 0 ;;
esac
cat >/dev/null
printf '%s\n' "$*" >> "$CURSOR_ATTEMPTS"
case "$*" in
  *"--resume original-session"*)
    printf '%s\n' 'invalid conversation history' >&2
    exit 1 ;;
esac
printf '%s\n' '{"type":"result","result":"done","is_error":false}'
`))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			logger := slog.Default()
			d := &Daemon{
				client: NewClient(server.URL), logger: logger,
				workspaces: make(map[string]*workspaceState), activeEnvRoots: make(map[string]int),
				runtimeIndex: map[string]Runtime{"rt-cursor": {ID: "rt-cursor", Provider: "cursor"}},
				cfg: Config{WorkspacesRoot: t.TempDir(), AgentTimeout: 10 * time.Second, ServerBaseURL: server.URL,
					Agents: map[string]AgentEntry{"cursor": {Path: fakeCursor}}},
			}
			task := Task{
				ID: "cursor-retry", WorkspaceID: "cursor-workspace", RuntimeID: "rt-cursor", IssueID: "cursor-issue",
				AgentID: "cursor-agent", AuthToken: "mat_cursor_fixture",
				PriorSessionID: "original-session", RequireCursorResume: tc.require,
				Agent: &AgentData{ID: "cursor-agent", Name: "cursor-agent", CustomEnv: map[string]string{"CURSOR_ATTEMPTS": attempts}},
			}
			if tc.lostWorkdir {
				task.PriorWorkDir = filepath.Join(root, "missing")
			} else {
				prior, err := execenv.Prepare(execenv.PrepareParams{
					WorkspacesRoot: d.cfg.WorkspacesRoot, WorkspaceID: task.WorkspaceID,
					TaskID: "cursor-parent", Provider: "cursor",
					Task: execenv.TaskContextForEnv{AgentID: task.AgentID, IssueID: task.IssueID},
				}, logger)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = prior.Cleanup(true) })
				prior.ReleaseLock()
				task.PriorWorkDir = prior.WorkDir
			}
			_, err := d.runTask(context.Background(), task, "cursor", 0, logger)
			recorded, readErr := os.ReadFile(attempts)
			if tc.lostWorkdir {
				if err == nil || !strings.Contains(err.Error(), "cannot reach the original session") || !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("lost workdir: run error=%v launches=%q read error=%v", err, recorded, readErr)
				}
				return
			}
			if err != nil || readErr != nil {
				t.Fatalf("run error=%v read error=%v", err, readErr)
			}
			launches := strings.Split(strings.TrimSpace(string(recorded)), "\n")
			if len(launches) != tc.wantLaunches || !strings.Contains(launches[0], "--resume original-session") {
				t.Fatalf("launches = %q, want %d starting with the pinned session", launches, tc.wantLaunches)
			}
			if len(launches) == 2 && strings.Contains(launches[1], "--resume") {
				t.Fatalf("ordinary fallback retained the rejected session: %q", launches[1])
			}
		})
	}
}
