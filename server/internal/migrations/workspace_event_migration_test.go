package migrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceEventCaptureInstallsAfterPrerequisiteIndexes(t *testing.T) {
	dir := realMigrationsDir(t)
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(data)
	}
	tableMigration := read("9005_workspace_event_outbox.up.sql")
	if strings.Contains(tableMigration, "CREATE TRIGGER") || strings.Contains(tableMigration, "append_workspace_event") {
		t.Fatal("migration 9005 installs capture before prerequisite unique indexes")
	}
	for _, name := range []string{
		"9006_workspace_event_cursor_unique.up.sql",
		"9007_workspace_event_source_unique.up.sql",
		"9008_workspace_event_sequence_unique.up.sql",
		"9011_workspace_event_id_unique.up.sql",
	} {
		body := strings.TrimSpace(read(name))
		if !strings.HasPrefix(body, "CREATE UNIQUE INDEX CONCURRENTLY") || strings.Count(body, ";") != 1 {
			t.Errorf("%s must remain one concurrent unique-index statement: %q", name, body)
		}
	}
	captureMigration := read("9012_workspace_event_capture.up.sql")
	for _, required := range []string{"CREATE OR REPLACE FUNCTION append_workspace_event", "CREATE TRIGGER capture_issue_workspace_event_trigger", "CREATE TRIGGER capture_artifact_workspace_event_trigger"} {
		if !strings.Contains(captureMigration, required) {
			t.Errorf("migration 9012 missing %q", required)
		}
	}
	retentionIndex := strings.TrimSpace(read("9013_workspace_event_retention_index.up.sql"))
	if !strings.HasPrefix(retentionIndex, "CREATE INDEX CONCURRENTLY") || strings.Count(retentionIndex, ";") != 1 {
		t.Fatalf("retention index must remain one concurrent-index statement: %q", retentionIndex)
	}
}
