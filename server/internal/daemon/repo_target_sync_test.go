package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/repocache"
)

func TestCheckoutTargetIgnoresUnrelatedCredentialFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("synthetic SSH uses POSIX shell")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "ssh-attempts")
	ssh := filepath.Join(dir, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\necho attempted >> \"$CHECKOUT_SSH_LOG\"\necho 'Permission denied (publickey).' >&2\nexit 255\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSH_COMMAND", "'"+ssh+"'")
	t.Setenv("CHECKOUT_SSH_LOG", log)
	target := createDaemonTestRepo(t)
	const unrelated = "ssh://git@unrelated.invalid/team/unavailable.git"
	d := newRepoReadyTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(WorkspaceReposResponse{WorkspaceID: "ws-1", Repos: []RepoData{{URL: unrelated}, {URL: target}}})
	})
	d.workspaces["ws-1"] = newWorkspaceState("ws-1", nil, "", nil, nil)
	if err := d.syncWorkspaceReposContext(context.Background(), "ws-1", []RepoData{{URL: unrelated}}); err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("control sync = %v", err)
	}
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	checkout := func(task string) *repocache.WorktreeResult {
		t.Helper()
		if _, err := d.ensureRepoReady(context.Background(), "ws-1", target); err != nil {
			t.Fatal(err)
		}
		result, err := d.repoCache.CreateWorktree(repocache.WorktreeParams{WorkspaceID: "ws-1", RepoURL: target, WorkDir: filepath.Join(dir, "work"), AgentName: "Test", TaskID: task})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(log); !os.IsNotExist(err) {
			t.Fatalf("checkout attempted unrelated SSH: %v", err)
		}
		return result
	}
	result := checkout("11111111-1111-1111-1111-111111111111")
	if !strings.HasPrefix(result.BranchName, "agent/test/") {
		t.Fatalf("not a dedicated branch: %+v", result)
	}
	if d.repoCache.Lookup("ws-1", target) == "" || d.repoCache.Lookup("ws-1", unrelated) != "" {
		t.Fatal("unexpected shared cache state")
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", result.Path}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("commit", "--allow-empty", "-m", "unpushed work")
	head := git("rev-parse", "HEAD")
	kept := checkout("22222222-2222-2222-2222-222222222222")
	if kept.Kept == "" || kept.BranchName != result.BranchName || git("rev-parse", "HEAD") != head {
		t.Fatalf("unpushed checkout not kept: %+v", kept)
	}
	dirty := filepath.Join(result.Path, "local-work.txt")
	if err := os.WriteFile(dirty, []byte("keep me\n"), 0600); err != nil {
		t.Fatal(err)
	}
	kept = checkout("33333333-3333-3333-3333-333333333333")
	data, err := os.ReadFile(dirty)
	if kept.Kept == "" || kept.BranchName != result.BranchName || git("rev-parse", "HEAD") != head || err != nil || string(data) != "keep me\n" {
		t.Fatalf("dirty checkout not kept: %+v, %v", kept, err)
	}
	_, err = d.ensureRepoReady(context.Background(), "ws-1", unrelated)
	if err == nil || !strings.Contains(err.Error(), "ssh://unrelated.invalid/team/unavailable.git") || !strings.Contains(err.Error(), "Permission denied") || strings.Contains(err.Error(), target) {
		t.Fatalf("target failure identity: %v", err)
	}
	if d.repoCache.Lookup("ws-1", unrelated) != "" {
		t.Fatal("inaccessible target was cached")
	}
}

type checkoutCloneFailureCache struct {
	recordingRepoCache
}

func (c *checkoutCloneFailureCache) Lookup(_, _ string) string {
	return ""
}

func (c *checkoutCloneFailureCache) Sync(_ string, repos []repocache.RepoInfo) error {
	return fmt.Errorf("git clone --bare: authentication failed for %q", repos[0].URL)
}

func TestRepoCheckoutReadinessErrorOmitsURLUserinfo(t *testing.T) {
	const workspaceID = "ws-1"
	const target = "https://example.invalid/team/repo.git"
	const registered = "https://registered-user:registered-pass@example.invalid/team/repo.git"
	const requested = "https://requested-user:requested-pass@example.invalid/team/repo.git"

	workDir := t.TempDir()
	cache := &checkoutCloneFailureCache{}
	d := newRepoCheckoutTestDaemon(t, workspaceID, registered, workDir, cache)
	body, err := json.Marshal(repoCheckoutRequest{
		URL: requested, WorkspaceID: workspaceID, WorkDir: workDir, TaskID: "task-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	d.repoCheckoutHandler().ServeHTTP(rec, authorizedRepoCheckoutRequest(strings.NewReader(string(body))))
	message := rec.Body.String()
	if rec.Code != http.StatusInternalServerError ||
		!strings.Contains(message, "authentication failed for "+fmt.Sprintf("%q", target)) ||
		!strings.Contains(message, "(target: "+target+")") {
		t.Fatalf("expected a target-specific sync failure, got HTTP %d: %s", rec.Code, message)
	}
	for _, credential := range []string{"requested-user", "requested-pass", "registered-user", "registered-pass"} {
		if strings.Contains(message, credential) {
			t.Errorf("checkout readiness response exposed synthetic userinfo: %s", message)
		}
	}
}
