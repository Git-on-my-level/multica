package migrations

import (
	"reflect"
	"testing"
)

func TestSortMigrationFilesUsesNumericPrefix(t *testing.T) {
	files := []string{
		"dir/9014_runtime_profile_add_omp.up.sql",
		"dir/900_runtime_profile_add_foo.up.sql",
		"dir/999_runtime_profile_rewrite.up.sql",
		"dir/901_runtime_profile_add_bar.up.sql",
		"dir/486_runtime_profile_restore.up.sql",
		"dir/9016_runtime_profile_add_devin.up.sql",
		"dir/143_runtime_profile_add_omp.up.sql",
		"dir/143_agent_task_queue_chat_pending_v2.up.sql",
	}

	sortMigrationFiles(files, false)

	want := []string{
		"dir/143_agent_task_queue_chat_pending_v2.up.sql",
		"dir/143_runtime_profile_add_omp.up.sql",
		"dir/486_runtime_profile_restore.up.sql",
		"dir/900_runtime_profile_add_foo.up.sql",
		"dir/901_runtime_profile_add_bar.up.sql",
		"dir/999_runtime_profile_rewrite.up.sql",
		"dir/9014_runtime_profile_add_omp.up.sql",
		"dir/9016_runtime_profile_add_devin.up.sql",
	}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("up order = %v, want %v", files, want)
	}

	sortMigrationFiles(files, true)
	wantDown := []string{
		"dir/9016_runtime_profile_add_devin.up.sql",
		"dir/9014_runtime_profile_add_omp.up.sql",
		"dir/999_runtime_profile_rewrite.up.sql",
		"dir/901_runtime_profile_add_bar.up.sql",
		"dir/900_runtime_profile_add_foo.up.sql",
		"dir/486_runtime_profile_restore.up.sql",
		"dir/143_runtime_profile_add_omp.up.sql",
		"dir/143_agent_task_queue_chat_pending_v2.up.sql",
	}
	if !reflect.DeepEqual(files, wantDown) {
		t.Fatalf("down order = %v, want %v", files, wantDown)
	}
}
