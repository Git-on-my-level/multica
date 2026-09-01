package repocache

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Fork overlay: keep linked-worktree repair out of upstream cache.go so
// syncs do not re-conflict on that file.
func ensureLinkedWorktreeUsable(worktreePath string) error {
	if out, err := runGitCombinedOutput("-C", worktreePath, "config", "--worktree", "core.bare", "false"); err != nil {
		if werr := writeLinkedWorktreeBareFalse(worktreePath); werr != nil {
			return fmt.Errorf("set core.bare=false: git config: %s (%w); write: %v", strings.TrimSpace(string(out)), err, werr)
		}
	}
	out, err := runGitOutput("-C", worktreePath, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("linked worktree still unusable at %s: %s: %v", worktreePath, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func writeLinkedWorktreeBareFalse(worktreePath string) error {
	gitFile := filepath.Join(worktreePath, ".git")
	raw, err := os.ReadFile(gitFile)
	if err != nil {
		return err
	}
	line := strings.TrimSpace(string(raw))
	const prefix = "gitdir:"
	if !strings.HasPrefix(strings.ToLower(line), prefix) {
		return fmt.Errorf("unexpected .git file contents in %s", worktreePath)
	}
	gitdir := strings.TrimSpace(line[len(prefix):])
	if gitdir == "" {
		return fmt.Errorf("empty gitdir in %s", worktreePath)
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(worktreePath, gitdir)
	}
	return os.WriteFile(filepath.Join(gitdir, "config.worktree"), []byte("[core]\n\tbare = false\n"), 0o644)
}
