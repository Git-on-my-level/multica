package execenv

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRemoveExpiredRootRejectsChangedDirectoryEntry(t *testing.T) {
	t.Parallel()

	base, other := t.TempDir(), t.TempDir()
	rootPath := filepath.Join(base, "ws", "task")
	if err := os.MkdirAll(rootPath, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{rootPath, other} {
		if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	baseRoot, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer baseRoot.Close()
	claim, err := LockEnvRootForGC(baseRoot, filepath.Join("ws", "task"), rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Release()

	oldPath := rootPath + "-moved"
	if err := os.Rename(rootPath, oldPath); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("filesystem refused to rename an open directory: %v", err)
		}
		t.Fatal(err)
	}
	if err := os.Symlink(other, rootPath); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation is unavailable: %v", err)
		}
		t.Fatal(err)
	}

	if err := claim.RemoveExpiredRoot(); err == nil {
		t.Fatal("reclamation accepted a changed directory entry")
	}
	for _, dir := range []string{oldPath, other} {
		if got, err := os.ReadFile(filepath.Join(dir, "keep")); err != nil || string(got) != "fixture" {
			t.Errorf("fixture contents changed in %s: %q, %v", dir, got, err)
		}
	}
	if info, err := os.Lstat(rootPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("replacement directory entry changed: %v, %v", info, err)
	}
}
