package execenv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func writeHermesGatewayGCState(t *testing.T, taskDir string, pid any) {
	t.Helper()
	home := filepath.Join(taskDir, hermesGatewayHome)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	absoluteHome, err := filepath.Abs(home)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{
		"kind": "hermes-gateway", "gateway_state": "stopped", "hermes_home": absoluteHome,
	}
	if pid != nil {
		state["pid"] = pid
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "gateway_state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHermesGatewayOrphanAllowsStaleReservationFiles(t *testing.T) {
	workspaces := t.TempDir()
	taskDir := filepath.Join(workspaces, "01111111-1111-7111-8111-111111111111", "task-111111111111")
	home := filepath.Join(taskDir, hermesGatewayHome)
	writeHermesGatewayGCState(t, taskDir, nil)
	for name, body := range map[string]string{
		"gateway.pid":  `{"pid":2147483647}`,
		"gateway.lock": `{"pid":2147483647}`,
		"state.db":     "stale gateway data",
	} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := filepath.WalkDir(taskDir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, old, old)
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	orphan := OpenHermesGatewayOrphan(workspaces, taskDir, now)
	if orphan == nil {
		t.Fatal("stale reservation records should match the Hermes-only task root")
	}
	defer orphan.Close()
	if !orphan.Proven {
		t.Fatal("dead PID records and an unlocked gateway.lock should prove a stopped gateway")
	}
	if _, removed, err := orphan.Remove(now, 12*time.Hour, false); err != nil || !removed {
		t.Fatalf("remove stale Hermes root = removed %v, err %v; want removed", removed, err)
	}
	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Fatalf("expired Hermes task root still exists: %v", err)
	}
}

func TestHermesGatewayStatusPreservesLivePIDAndReservation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		livePIDFile bool
		activeLock  bool
	}{
		{name: "live PID file", livePIDFile: true},
		{name: "held lock", activeLock: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			taskDir := t.TempDir()
			homePath := filepath.Join(taskDir, hermesGatewayHome)
			writeHermesGatewayGCState(t, taskDir, nil)
			lockPath := filepath.Join(homePath, "gateway.lock")
			if err := os.WriteFile(lockPath, []byte(`{"pid":2147483647}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.livePIDFile {
				if err := os.WriteFile(filepath.Join(homePath, "gateway.pid"), []byte(`{"pid":`+strconv.Itoa(os.Getpid())+`}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(taskDir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()

			var ownedLock *os.File
			if tc.activeLock {
				home, err := root.OpenRoot(hermesGatewayHome)
				if err != nil {
					t.Fatal(err)
				}
				var reserved bool
				ownedLock, reserved, err = acquireHermesGatewayLock(home)
				if closeErr := home.Close(); err == nil {
					err = closeErr
				}
				if err != nil || reserved || ownedLock == nil {
					t.Fatalf("acquire test gateway lock = %v, %v; want held", reserved, err)
				}
				defer releaseHermesGatewayLock(ownedLock)
			}
			if got := inspectHermesGatewayStatus(root, false); got != hermesGatewayUnknown {
				t.Fatalf("gateway status = %v, want unknown while a live PID or reservation exists", got)
			}
		})
	}
}
