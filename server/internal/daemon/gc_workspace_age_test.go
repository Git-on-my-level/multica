package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

func TestGCWorkspace_RedispatchDoesNotRenewWorkspaceAge(t *testing.T) {
	t.Parallel()

	const issueID = "77777777-7777-7777-7777-777777777770"
	for _, tc := range []struct {
		name          string
		age, maxAge   time.Duration
		parentOffline bool
		local         bool
		reason        string
	}{
		{name: "completed TTL with ceiling disabled", age: 13 * time.Hour, reason: "completed task retention expired"},
		{name: "below completed TTL", age: 11 * time.Hour, maxAge: 36 * time.Hour},
		{name: "ceiling during parent outage", age: 37 * time.Hour, maxAge: 36 * time.Hour, parentOffline: true, reason: gcWorkspaceMaxAgeReason},
		{name: "no ceiling during parent outage", age: 37 * time.Hour, parentOffline: true},
		{name: "local directory record", age: 37 * time.Hour, maxAge: 36 * time.Hour, local: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newGCTestDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.parentOffline {
					http.Error(w, "offline", http.StatusServiceUnavailable)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"issues": []IssueGCCheckResult{{ID: issueID, Found: true, Status: "in_progress", UpdatedAt: time.Now()}},
				})
			}))
			d.cfg.GCTTL = 6 * time.Hour
			d.cfg.GCCompletedTaskTTL = 12 * time.Hour
			d.cfg.GCOrphanTTL = 12 * time.Hour
			d.cfg.GCWorkspaceMaxAge = tc.maxAge

			var logs bytes.Buffer
			d.logger = slog.New(slog.NewJSONHandler(&logs, nil))

			taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "issue-task", nil)
			writeFile(t, filepath.Join(taskDir, "workdir", "checkout.bin"), 128)
			past := time.Now().Add(-tc.age)
			ownerPath := filepath.Join(taskDir, ".task_owner")
			if err := os.Chtimes(ownerPath, past, past); err != nil {
				t.Fatal(err)
			}
			for _, taskID := range []string{"task", "followup-one", "followup-two"} {
				if err := execenv.WriteGCMeta(taskDir, execenv.GCMeta{
					Kind: execenv.GCKindIssue, IssueID: issueID, WorkspaceID: "ws", TaskID: taskID, LocalDirectory: tc.local,
				}, d.logger); err != nil {
					t.Fatal(err)
				}
			}

			scan := func() *gcStats {
				t.Helper()
				stats := &gcStats{}
				d.gcWorkspace(context.Background(), filepath.Dir(taskDir), stats)
				return stats
			}

			// First use the daemon's guard, then simulate a different daemon
			// holding the filesystem lock without marking this daemon's map.
			release := d.markActiveEnvRoot(taskDir)
			scan()
			release()
			assertKept(t, taskDir, ".gc_meta.json")

			root, err := os.OpenRoot(d.cfg.WorkspacesRoot)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			claim, _, err := execenv.LockEnvRootForReuse(root, filepath.Join("ws", "issue-task"), taskDir)
			if err != nil || claim == nil {
				t.Fatalf("hold run lock: claim=%v, err=%v", claim, err)
			}
			defer claim.Release()
			scan()
			claim.Release()
			assertKept(t, taskDir, ".gc_meta.json")

			stats := scan()
			if tc.reason == "" {
				assertKept(t, taskDir, ".gc_meta.json")
				return
			}
			assertGone(t, filepath.Dir(taskDir), filepath.Base(taskDir))
			if stats.cleaned != 1 {
				t.Fatalf("cleaned = %d, want 1", stats.cleaned)
			}
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var record struct {
					Message string        `json:"msg"`
					Reason  string        `json:"reason"`
					Age     time.Duration `json:"age"`
				}
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record.Message == "gc: removed" {
					if record.Reason != tc.reason || record.Age < tc.age {
						t.Fatalf("removal log = %s, want reason %q and age at least %s", line, tc.reason, tc.age)
					}
					t.Logf("active workspace survived, then reclaimed at age %s (%s)", record.Age.Round(time.Second), record.Reason)
					return
				}
			}
			t.Fatalf("no removal log: %s", logs.String())
		})
	}
}
