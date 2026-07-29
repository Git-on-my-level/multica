package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSkippedRuntimeRegistrationRetriesAndSurfacesHealth(t *testing.T) {
	origDetect := detectAgentVersion
	origCheck := checkAgentMinVersion
	t.Cleanup(func() {
		detectAgentVersion = origDetect
		checkAgentMinVersion = origCheck
	})

	var cursorProbes atomic.Int32
	detectAgentVersion = func(_ context.Context, path string) (string, error) {
		if path == "/cursor" && cursorProbes.Add(1) == 1 {
			return "", errors.New("signal: killed")
		}
		return "9.9.9", nil
	}
	checkAgentMinVersion = func(_, _ string) error { return nil }

	var registrations [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/daemon/register":
			var request struct {
				WorkspaceID string              `json:"workspace_id"`
				Runtimes    []map[string]string `json:"runtimes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode registration: %v", err)
			}
			providers := make([]string, 0, len(request.Runtimes))
			response := RegisterResponse{}
			for _, runtime := range request.Runtimes {
				provider := runtime["type"]
				providers = append(providers, provider)
				response.Runtimes = append(response.Runtimes, Runtime{ID: "rt-" + request.WorkspaceID + "-" + provider, Provider: provider, Status: "online"})
			}
			registrations = append(registrations, providers)
			_ = json.NewEncoder(w).Encode(response)
		case "/api/daemon/workspaces/ws-1/runtime-profiles":
			_ = json.NewEncoder(w).Encode(RuntimeProfilesResponse{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	d := freshDaemon(server.URL)
	d.cfg.Agents = map[string]AgentEntry{
		"claude": {Path: "/claude"},
		"cursor": {Path: "/cursor"},
	}

	initial, _, err := d.registerRuntimesForWorkspace(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("initial registration: %v", err)
	}
	if len(initial.Runtimes) != 1 || initial.Runtimes[0].Provider != "claude" {
		t.Fatalf("initial runtimes = %+v, want only claude", initial.Runtimes)
	}
	d.workspaces["ws-1"] = newWorkspaceState("ws-1", []string{"rt-ws-1-claude"}, "", nil, nil)
	d.runtimeIndex["rt-ws-1-claude"] = initial.Runtimes[0]

	rec := httptest.NewRecorder()
	d.healthHandler(time.Now()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	var health HealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if len(health.RegistrationSkips) != 1 || health.RegistrationSkips[0].Name != "cursor" || health.RegistrationSkips[0].Attempts != 1 {
		t.Fatalf("registration_skips = %+v, want cursor attempt 1", health.RegistrationSkips)
	}

	// A second workspace can happen to see cursor's now-successful probe
	// during initial sync. The skip must remain until the retry registers it
	// into *every* already-tracked workspace, otherwise ws-1 would stay
	// permanently offline while ws-2 comes online.
	second, _, err := d.registerRuntimesForWorkspace(context.Background(), "ws-2")
	if err != nil {
		t.Fatalf("second workspace registration: %v", err)
	}
	if len(second.Runtimes) != 2 {
		t.Fatalf("second workspace runtimes = %+v, want claude and cursor", second.Runtimes)
	}
	d.workspaces["ws-2"] = newWorkspaceState("ws-2", []string{"rt-ws-2-claude", "rt-ws-2-cursor"}, "", nil, nil)
	for _, runtime := range second.Runtimes {
		d.runtimeIndex[runtime.ID] = runtime
	}
	if skips := d.runtimeRegistrationSkips(); len(skips) != 1 || skips[0].Name != "cursor" {
		t.Fatalf("second workspace registration cleared pending skip: %+v", skips)
	}

	d.retrySkippedRuntimeRegistrations(context.Background(), time.Now().Add(time.Hour))
	if skips := d.runtimeRegistrationSkips(); len(skips) != 0 {
		t.Fatalf("registration skips after retry = %+v, want none", skips)
	}
	if _, ok := d.runtimeIndex["rt-ws-1-cursor"]; !ok {
		t.Fatal("cursor runtime missing after successful retry")
	}
	if got := d.workspaces["ws-1"].runtimeIDs; len(got) != 2 || got[0] != "rt-ws-1-claude" || got[1] != "rt-ws-1-cursor" {
		t.Fatalf("first workspace runtime IDs after retry = %v, want cursor added", got)
	}
	if got := d.workspaces["ws-2"].runtimeIDs; len(got) != 2 || got[0] != "rt-ws-2-claude" || got[1] != "rt-ws-2-cursor" {
		t.Fatalf("second workspace runtime IDs after retry = %v, want no duplicate cursor", got)
	}
	if len(registrations) != 4 || len(registrations[0]) != 1 || registrations[0][0] != "claude" || len(registrations[1]) != 2 || registrations[1][0] != "claude" || registrations[1][1] != "cursor" || len(registrations[2]) != 1 || registrations[2][0] != "cursor" || len(registrations[3]) != 1 || registrations[3][0] != "cursor" {
		t.Fatalf("registration payloads = %v, want [[claude] [claude cursor] [cursor] [cursor]]", registrations)
	}
}
