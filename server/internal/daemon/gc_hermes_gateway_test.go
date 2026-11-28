package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

func writeHermesGatewayState(t *testing.T, taskDir, kind, state, homeOverride string, pid any) {
	t.Helper()
	home := filepath.Join(taskDir, "hermes-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	recordHome := home
	if homeOverride != "" {
		recordHome = homeOverride
	}
	record := map[string]any{
		"kind": kind, "gateway_state": state, "hermes_home": recordHome,
		"exit_reason": "auto-started gateway idle for 600s",
	}
	if pid != nil {
		record["pid"] = pid
	}
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "gateway_state.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGCHermesGatewayOrphans(t *testing.T) {
	d := newGCTestDaemon(t, http.NotFoundHandler())
	d.cfg.GCOrphanTTL = 12 * time.Hour
	const workspaceID = "01111111-1111-7111-8111-111111111111"
	const workspaceSlug = "Scaling For"
	const oldGatewayPID = 2147483647
	now := time.Now()

	type fixture struct {
		name         string
		pid          any
		state        string
		homeOverride string
		kind         string
		freshWrite   bool
		extraWorkdir bool
		indexed      bool
		active       bool
		outside      bool
		lock         bool
		pidFile      bool
		noState      bool
		path         string
		bytes        int64
	}
	cases := []fixture{
		{name: "dead PID", pid: oldGatewayPID},
		{name: "absent PID, indexed", indexed: true},
		{name: "live PID despite stopped state", pid: os.Getpid()},
		{name: "non-stopped state", state: "stopping", pid: oldGatewayPID},
		{name: "gateway state for another home", homeOverride: t.TempDir()},
		{name: "wrong kind", kind: "other"},
		{name: "invalid PID", pid: -1},
		{name: "invalid PID type", pid: "2147483647"},
		{name: "missing gateway state", noState: true},
		{name: "newest write still within TTL", freshWrite: true},
		{name: "additional workspace data", extraWorkdir: true},
		{name: "active root", active: true},
		{name: "outside workspaces root", outside: true},
		{name: "stale PID and lock records", lock: true, pidFile: true},
	}
	for i := range cases {
		c := &cases[i]
		root := d.cfg.WorkspacesRoot
		if c.outside {
			root = t.TempDir()
		}
		params := execenv.RootDirParams{
			WorkspacesRoot: root,
			WorkspaceID:    workspaceID,
			WorkspaceSlug:  workspaceSlug,
			TaskID:         fmt.Sprintf("01111111-1111-7111-8111-%012x", i+1),
		}
		c.path = execenv.PredictRootDir(params)
		if c.indexed {
			var err error
			c.path, err = execenv.ResolveRootDir(params)
			if err != nil {
				t.Fatal(err)
			}
		}
		home := filepath.Join(c.path, "hermes-home")
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		state, kind := c.state, c.kind
		if state == "" {
			state = "stopped"
		}
		if kind == "" {
			kind = "hermes-gateway"
		}
		if !c.noState {
			writeHermesGatewayState(t, c.path, kind, state, c.homeOverride, c.pid)
		}
		for name, content := range map[string]string{"state.db": "fixture", ".clean_shutdown": ""} {
			if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if c.lock {
			if err := os.WriteFile(filepath.Join(home, "gateway.lock"), []byte(`{"pid":2147483647}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if c.pidFile {
			if err := os.WriteFile(filepath.Join(home, "gateway.pid"), []byte(`{"pid":2147483647}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if c.extraWorkdir {
			workdir := filepath.Join(c.path, "workdir")
			if err := os.Mkdir(workdir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workdir, "keep"), []byte("user data"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if c.active {
			defer d.markActiveEnvRoot(c.path)()
		}
		old := now.Add(-8 * 24 * time.Hour)
		if err := filepath.WalkDir(c.path, func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chtimes(path, old, old)
		}); err != nil {
			t.Fatal(err)
		}
		if c.freshWrite {
			file := filepath.Join(home, "state.db")
			recent := now.Add(-time.Hour)
			if err := os.Chtimes(file, recent, recent); err != nil {
				t.Fatal(err)
			}
		}
		c.bytes = dirSize(c.path)
	}

	ctx := context.Background()
	stats := &gcStats{}
	wsDir := filepath.Dir(cases[0].path)
	d.gcWorkspace(ctx, wsDir, stats)
	reclaimed := cases[0].bytes + cases[1].bytes + cases[13].bytes
	got := stats.hermesGatewayOrphans
	if got.found != 11 || got.aged != 10 || got.unproven != 7 ||
		got.removed != 3 || got.dirsRemoved != 2 || got.bytesRemoved != reclaimed || got.bytes <= reclaimed {
		t.Fatalf("unexpected gateway census: %+v", got)
	}
	if _, err := os.Lstat(cases[0].path); !os.IsNotExist(err) {
		t.Fatalf("expired gateway root still exists: %v", err)
	}
	indexed := cases[1].path
	if entries, err := os.ReadDir(indexed); err != nil || len(entries) != 0 {
		t.Fatalf("expected an empty anchor for the indexed root: %v, %v", entries, err)
	}
	if _, err := execenv.IndexedEnvRootOwner(d.cfg.WorkspacesRoot, indexed); err != nil {
		t.Fatalf("expected the stable root record to survive: %v", err)
	}
	for _, c := range cases[2:] {
		if c.pidFile {
			if _, err := os.Stat(c.path); !os.IsNotExist(err) {
				t.Errorf("%s: stale Hermes task root should be reclaimed: %v", c.name, err)
			}
			continue
		}
		if _, err := os.Stat(filepath.Join(c.path, "hermes-home")); err != nil {
			t.Errorf("%s: expected gateway home to survive: %v", c.name, err)
		}
		if c.outside && d.gcHermesGatewayOrphan(ctx, c.path, &gcStats{}) {
			t.Errorf("%s: gateway home was eligible", c.name)
		}
		if c.extraWorkdir {
			if _, err := os.Stat(filepath.Join(c.path, "workdir", "keep")); err != nil {
				t.Errorf("%s: workdir sentinel must survive: %v", c.name, err)
			}
		}
	}
	after := &gcStats{}
	d.gcWorkspace(ctx, wsDir, after)
	if got := after.hermesGatewayOrphans; got.found != 8 || got.aged != 7 || got.removed != 0 ||
		got.bytes != stats.hermesGatewayOrphans.bytes-reclaimed {
		t.Fatalf("unexpected second census: %+v", got)
	}
}

func TestGCAutopilotWaitsForHermesGateway(t *testing.T) {
	d := newGCTestDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/autopilot-runs/run/gc-check" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"completed"}`)
	}))
	meta := &execenv.GCMeta{Kind: execenv.GCKindAutopilotRun, AutopilotRunID: "run", WorkspaceID: "ws"}
	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "task-111111111111", meta)
	plain := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "task-222222222222", meta)
	ctx := context.Background()

	assertKept := func() {
		t.Helper()
		d.gcWorkspace(ctx, filepath.Dir(taskDir), &gcStats{})
		if _, err := os.Stat(filepath.Join(taskDir, ".task_owner")); err != nil {
			t.Fatalf("expected the terminal task root to survive gateway shutdown: %v", err)
		}
	}

	writeHermesGatewayState(t, taskDir, "hermes-gateway", "running", "", os.Getpid())
	assertKept()
	if _, err := os.Lstat(plain); !os.IsNotExist(err) {
		t.Fatalf("terminal root without a gateway must be reaped immediately: %v", err)
	}

	writeHermesGatewayState(t, taskDir, "hermes-gateway", "stopped", "", os.Getpid())
	assertKept()

	writeHermesGatewayState(t, taskDir, "hermes-gateway", "stopped", "", 2147483647)
	reservation := filepath.Join(taskDir, "hermes-home", "gateway.pid")
	if err := os.WriteFile(reservation, []byte(fmt.Sprintf("%d", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	assertKept()
	if err := os.Remove(reservation); err != nil {
		t.Fatal(err)
	}

	d.gcWorkspace(ctx, filepath.Dir(taskDir), &gcStats{})
	if _, err := os.Lstat(taskDir); !os.IsNotExist(err) {
		t.Fatalf("terminal task with stopped gateway must be reaped on the next sweep: %v", err)
	}
}
