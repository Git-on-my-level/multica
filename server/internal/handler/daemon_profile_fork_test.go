package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestIsUnprovisionedProfileFailure(t *testing.T) {
	cases := map[string]bool{
		"command not found on PATH or provider discovery: claude-pool": true,
		"command not found on PATH: missing-codex":                     true,
		"  command not found on PATH":                                  true,
		"Configured path override is not executable: /opt/x; command not found on PATH or provider discovery: claude-pool": false,
		"unsupported runtime_type: foo": false,
		"":                              false,
	}
	for reason, want := range cases {
		if got := isUnprovisionedProfileFailure(reason); got != want {
			t.Errorf("isUnprovisionedProfileFailure(%q) = %v, want %v", reason, got, want)
		}
	}
}

// A host that simply lacks a profile's command must not get an offline runtime
// row for that profile (R24: one host-specific profile left an offline copy on
// every other host). A broken path override on the same request is still
// recorded so a real misconfiguration stays visible.
func TestDaemonRegister_SkipsUnprovisionedProfileFailure(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	missing := insertRuntimeProfileFixture(t, ctx, "Host-only Claude", "claude", "host-only-claude")
	broken := insertRuntimeProfileFixture(t, ctx, "Broken Override Codex", "codex", "broken-codex")
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_runtime WHERE profile_id = ANY($1::uuid[])`, []string{missing, broken})
	})
	// Profiles are bound to one machine (MUL-9425); an unscoped legacy profile
	// may only refresh instances that already exist, so bind these to the
	// registering daemon.
	if _, err := testPool.Exec(ctx, `UPDATE runtime_profile SET daemon_id = $1 WHERE id = ANY($2::uuid[])`,
		"test-daemon-unprovisioned-profile", []string{missing, broken}); err != nil {
		t.Fatalf("bind profiles to the daemon: %v", err)
	}

	req := newOwnerDaemonRequest("POST", "/api/daemon/register", map[string]any{
		"workspace_id": testWorkspaceID,
		"daemon_id":    "test-daemon-unprovisioned-profile",
		"device_name":  "test-device",
		"failed_profiles": []map[string]any{
			{"profile_id": missing, "command_name": "host-only-claude", "reason": "command not found on PATH or provider discovery: host-only-claude"},
			{"profile_id": broken, "command_name": "broken-codex", "reason": "Configured path override is not executable: /opt/broken-codex; command not found on PATH or provider discovery: broken-codex"},
		},
	})
	req.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilityLocalWorktreeV1)
	testutil.Call(t, testHandler.DaemonRegister, req).Want(http.StatusOK)

	count := func(profileID string) int {
		var n int
		if err := testPool.QueryRow(ctx, `
			SELECT count(*) FROM agent_runtime
			WHERE workspace_id = $1 AND daemon_id = $2 AND profile_id = $3
		`, testWorkspaceID, "test-daemon-unprovisioned-profile", profileID).Scan(&n); err != nil {
			t.Fatalf("count runtimes: %v", err)
		}
		return n
	}
	if n := count(missing); n != 0 {
		t.Fatalf("unprovisioned profile produced %d runtime rows, want 0", n)
	}
	if n := count(broken); n != 1 {
		t.Fatalf("broken-override profile produced %d runtime rows, want 1", n)
	}
}
