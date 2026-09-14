package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForkStemAliasesMatchMigrationFiles(t *testing.T) {
	t.Parallel()

	if len(forkStemAliases) == 0 {
		t.Fatal("forkStemAliases is empty")
	}

	seenOld := map[string]struct{}{}
	seenNew := map[string]struct{}{}
	for _, pair := range forkStemAliases {
		oldName, newName := pair[0], pair[1]
		if !strings.HasPrefix(oldName, "46") && !strings.HasPrefix(oldName, "47") && !strings.HasPrefix(oldName, "48") {
			t.Errorf("old stem %q is not in the 469–486 remap set", oldName)
		}
		if !strings.HasPrefix(newName, "9") {
			t.Errorf("new stem %q is not in 9xxx", newName)
		}
		if _, ok := seenOld[oldName]; ok {
			t.Errorf("duplicate old stem %q", oldName)
		}
		if _, ok := seenNew[newName]; ok {
			t.Errorf("duplicate new stem %q", newName)
		}
		seenOld[oldName] = struct{}{}
		seenNew[newName] = struct{}{}

		up := filepath.Join("..", "..", "migrations", newName+".up.sql")
		if _, err := os.Stat(up); err != nil {
			t.Errorf("missing %s: %v", up, err)
		}
		oldUp := filepath.Join("..", "..", "migrations", oldName+".up.sql")
		if _, err := os.Stat(oldUp); err == nil {
			t.Errorf("old stem file still present: %s", oldUp)
		}
	}
}
