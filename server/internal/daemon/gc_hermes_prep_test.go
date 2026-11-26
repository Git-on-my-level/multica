package daemon

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

func TestGCHermesPrepOrphans(t *testing.T) {
	d := newGCTestDaemon(t, http.NotFoundHandler())
	d.cfg.GCOrphanTTL = 12 * time.Hour
	const workspaceID = "01111111-1111-7111-8111-111111111111"
	const workspaceSlug = "Test Workspace"

	makeOverlay := func(n int, age time.Duration, extra string, indexed bool) (string, int64) {
		t.Helper()
		source := t.TempDir()
		prepareRoot := d.cfg.WorkspacesRoot
		if !indexed {
			prepareRoot = t.TempDir()
		}
		taskID := fmt.Sprintf("01111111-1111-7111-8111-%012x", n)
		env, err := execenv.Prepare(execenv.PrepareParams{
			WorkspacesRoot:   prepareRoot,
			WorkspaceID:      workspaceID,
			WorkspaceSlug:    workspaceSlug,
			TaskID:           taskID,
			Provider:         "hermes",
			HermesSourceHome: source,
			Task:             execenv.TaskContextForEnv{AgentSkills: []execenv.SkillContextForEnv{{Name: "fixture", Content: "fixture"}}},
		}, d.logger)
		if err != nil {
			t.Fatal(err)
		}
		env.ReleaseLock()
		entries, err := os.ReadDir(env.RootDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Name() != "hermes-home" {
				if err := os.RemoveAll(filepath.Join(env.RootDir, entry.Name())); err != nil {
					t.Fatal(err)
				}
			}
		}
		if prepareRoot != d.cfg.WorkspacesRoot {
			dst := filepath.Join(d.cfg.WorkspacesRoot, filepath.Base(filepath.Dir(env.RootDir)), filepath.Base(env.RootDir))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(env.RootDir, dst); err != nil {
				t.Fatal(err)
			}
			env.RootDir = dst
		}
		if extra != "" {
			if err := os.WriteFile(filepath.Join(env.RootDir, extra), []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		old := time.Now().Add(-age)
		err = filepath.WalkDir(env.RootDir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.Type()&os.ModeSymlink != 0 {
				return err
			}
			return os.Chtimes(path, old, old)
		})
		if err != nil {
			t.Fatal(err)
		}
		return env.RootDir, dirSize(env.RootDir)
	}

	expired, expiredBytes := makeOverlay(1, 8*24*time.Hour, "", false)
	extra, _ := makeOverlay(2, 8*24*time.Hour, "other-data", false)
	fresh, freshBytes := makeOverlay(3, 2*time.Hour, "", false)
	active, _ := makeOverlay(4, 8*24*time.Hour, "", false)
	defer d.markActiveEnvRoot(active)()
	locked, _ := makeOverlay(5, 8*24*time.Hour, ".task_lock", false)
	lookalike, _ := makeOverlay(6, 8*24*time.Hour, "", false)
	lookalikeHome := filepath.Join(lookalike, "hermes-home")
	if err := os.Remove(filepath.Join(lookalikeHome, ".multica-task-local-state-v1")); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(lookalikeHome, old, old); err != nil {
		t.Fatal(err)
	}
	outside, _ := makeOverlay(7, 8*24*time.Hour, "", false)
	outsideTarget := filepath.Join(t.TempDir(), filepath.Base(outside))
	if err := os.Rename(outside, outsideTarget); err != nil {
		t.Fatal(err)
	}

	indexed, indexedBytes := makeOverlay(8, 8*24*time.Hour, "", true)

	stats := &gcStats{}
	d.gcWorkspace(context.Background(), filepath.Dir(expired), stats)
	if got := stats.hermesPrepOrphans; got.found != 4 || got.aged != 3 || got.unproven != 1 ||
		got.removed != 2 || got.dirsRemoved != 1 || got.bytes < expiredBytes+indexedBytes+freshBytes || got.bytesRemoved != expiredBytes+indexedBytes {
		t.Fatalf("unexpected Hermes census: %+v", got)
	}
	if _, err := os.Lstat(expired); !os.IsNotExist(err) {
		t.Fatalf("expired overlay still exists: %v", err)
	}
	for _, path := range []string{extra, fresh, active, locked, lookalike, outsideTarget} {
		if _, err := os.Stat(filepath.Join(path, "hermes-home")); err != nil {
			t.Errorf("expected %s to survive: %v", path, err)
		}
	}
	if entries, err := os.ReadDir(indexed); err != nil || len(entries) != 0 {
		t.Fatalf("expected an empty anchor for the indexed root: %v, %v", entries, err)
	}
	if _, err := execenv.IndexedEnvRootOwner(d.cfg.WorkspacesRoot, indexed); err != nil {
		t.Fatalf("expected the stable root record to survive: %v", err)
	}
	after := &gcStats{}
	d.gcWorkspace(context.Background(), filepath.Dir(expired), after)
	if got := after.hermesPrepOrphans; got.found != 2 || got.aged != 1 || got.removed != 0 ||
		got.bytes != stats.hermesPrepOrphans.bytes-expiredBytes-indexedBytes {
		t.Fatalf("unexpected second census: %+v", got)
	}
	if d.gcHermesPrepOrphan(context.Background(), outsideTarget, &gcStats{}) {
		t.Fatal("outside root was eligible")
	}
}
