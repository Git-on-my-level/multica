package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestLauncherHealthProfileRegistrationPersistsAndClearsGate(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	profileID := insertRuntimeProfileFixture(t, ctx, "Morph fixture", "opencode", "opencode-morph-fixture")
	dbfx := testutil.New(testPool, testWorkspaceID, testUserID)
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_runtime WHERE profile_id=$1`, profileID) })
	base := map[string]any{"workspace_id": testWorkspaceID, "daemon_id": "launcher-morph-fixture", "device_name": "fixture"}
	base["runtimes"] = []map[string]any{{"name": "Morph fixture", "type": "opencode", "profile_id": profileID, "version": "9.9.9", "status": "online"}}
	testutil.Call(t, testHandler.DaemonRegister, newOwnerDaemonRequest("POST", "/api/daemon/register", base)).Want(http.StatusOK)
	var originalID string
	dbfx.QueryRow(t, `SELECT id::text FROM agent_runtime WHERE daemon_id=$1 AND profile_id=$2`, "launcher-morph-fixture", profileID).Scan(&originalID)
	// A busy profile keeps existing work online while refusing new claims.
	base["runtimes"] = []map[string]any{{"name": "Morph fixture", "type": "opencode", "profile_id": profileID, "status": "online", "failure_code": protocol.LauncherUnavailableCode}}
	testutil.Call(t, testHandler.DaemonRegister, newOwnerDaemonRequest("POST", "/api/daemon/register", base)).Want(http.StatusOK)
	var busyStatus string
	var held bool
	dbfx.QueryRow(t, `SELECT status,metadata->'offline_reason'->>'launcher_unavailable' = 'true' FROM agent_runtime WHERE id=$1`, originalID).Scan(&busyStatus, &held)
	if busyStatus != "online" || !held {
		t.Fatalf("busy launcher lost active bookkeeping or new-claim gate: %s held=%v", busyStatus, held)
	}
	delete(base, "runtimes")
	base["failed_profiles"] = []map[string]any{{"profile_id": profileID, "failure_code": protocol.LauncherUnavailableCode, "reason": "setting value must not persist", "command_name": "private-wrapper-setting"}}
	req := newOwnerDaemonRequest("POST", "/api/daemon/register", base)
	testutil.Call(t, testHandler.DaemonRegister, req).Want(http.StatusOK)
	var id, status string
	var metadata []byte
	dbfx.QueryRow(t, `SELECT id::text,status,metadata FROM agent_runtime WHERE daemon_id=$1 AND profile_id=$2`, "launcher-morph-fixture", profileID).Scan(&id, &status, &metadata)
	if id != originalID {
		t.Fatal("failed startup registration replaced runtime bookkeeping")
	}
	var meta map[string]any
	if err := json.Unmarshal(metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if status != "offline" || meta["runtime_profile_failure_reason"] != protocol.LauncherUnavailableDetail || meta["command_name"] != nil || meta["offline_reason"] == nil {
		t.Fatalf("invalid health gate: status=%s metadata=%v", status, meta)
	}
	delete(base, "failed_profiles")
	base["runtimes"] = []map[string]any{{"name": "Morph fixture", "type": "opencode", "profile_id": profileID, "version": "9.9.9", "status": "online"}}
	testutil.Call(t, testHandler.DaemonRegister, newOwnerDaemonRequest("POST", "/api/daemon/register", base)).Want(http.StatusOK)
	var restoredID string
	dbfx.QueryRow(t, `SELECT id::text,status,metadata FROM agent_runtime WHERE daemon_id=$1 AND profile_id=$2`, "launcher-morph-fixture", profileID).Scan(&restoredID, &status, &metadata)
	if err := json.Unmarshal(metadata, &meta); err != nil {
		t.Fatal(err)
	}
	var restored map[string]any
	if err := json.Unmarshal(metadata, &restored); err != nil {
		t.Fatal(err)
	}
	if restoredID != id || status != "online" || restored["offline_reason"] != nil {
		t.Fatalf("healthy profile registration did not restore same runtime: %s %s %v", restoredID, status, restored)
	}
}
