package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testFullRevision = "e761e94c7080fd1ac9e9a1afa5423ecedf0fb87d"

func TestServerVersionMetadata(t *testing.T) {
	vcsRevision := strings.Repeat("a", 40)
	info := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: vcsRevision},
		{Key: "vcs.time", Value: "2026-11-26T19:00:00Z"},
	}}
	started := time.Date(2026, 11, 26, 16, 2, 11, 0, time.FixedZone("EST", -5*60*60))
	for _, tc := range []struct {
		name, full, display, stamp string
		info                       *debug.BuildInfo
		want                       versionResponse
	}{
		{
			name: "explicit full revision wins",
			full: testFullRevision, display: strings.Repeat("b", 40), info: info,
			stamp: "2026-11-26T15:31:24-05:00",
			want:  versionResponse{Version: "v0.4.28", Commit: testFullRevision, BuiltAt: "2026-11-26T20:31:24Z"},
		},
		{
			name:    "Docker COMMIT stamp",
			display: testFullRevision,
			want:    versionResponse{Version: "v0.4.28", Commit: testFullRevision},
		},
		{
			name: "embedded VCS revision with no build timestamp",
			full: "unknown", display: testFullRevision[:8], stamp: "unknown", info: info,
			want: versionResponse{Version: "v0.4.28", Commit: vcsRevision},
		},
		{
			name: "omit embedded and linker revisions for a dirty source build",
			full: testFullRevision,
			info: &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: vcsRevision},
				{Key: "vcs.modified", Value: "true"},
			}},
			want: versionResponse{Version: "v0.4.28"},
		},
		{
			name: "omit unusable metadata",
			full: strings.Repeat("z", 40), display: testFullRevision[:8], stamp: "invalid",
			want: versionResponse{Version: "v0.4.28"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.want.StartedAt = "2026-11-26T21:02:11Z"
			got := serverVersionFromBuild("v0.4.28", tc.full, tc.display, tc.stamp, started, tc.info)
			if got != tc.want {
				t.Errorf("version response = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestServerVersionHandlerOmitsUnavailableValues(t *testing.T) {
	version := serverVersionFromBuild("dev", "unknown", "", "", time.Time{}, nil)
	rec := httptest.NewRecorder()
	version.handler(rec, httptest.NewRequest(http.MethodGet, "/api/version", nil))

	if got := strings.TrimSpace(rec.Body.String()); got != `{"version":"dev"}` {
		t.Errorf("response = %s, want only the version", got)
	}
}

func TestRouterVersionRoute(t *testing.T) {
	// A member session without a workspace role can read serving identity.
	memberToken, err := generateTestJWT(uuid.NewString(), "version-reader@example.test", "Version Reader")
	if err != nil {
		t.Fatal(err)
	}
	agentID, _ := firstFixtureAgent(t)
	agentToken := mintAgentTaskToken(t, agentID, ensureAgentTask(t, agentID), testUserID)
	client := testServer.Client()

	for _, tc := range []struct {
		name, method, token string
		wantStatus          int
	}{
		{"anonymous", http.MethodGet, "", http.StatusUnauthorized},
		{"invalid token", http.MethodGet, "invalid", http.StatusUnauthorized},
		{"member", http.MethodGet, memberToken, http.StatusOK},
		{"agent task", http.MethodGet, agentToken, http.StatusOK},
		{"write", http.MethodPost, memberToken, http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, testServer.URL+"/api/version", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			if got := resp.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if got := resp.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			var identity versionResponse
			decoder := json.NewDecoder(resp.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&identity); err != nil {
				t.Fatalf("decode build identity: %v", err)
			}
			if identity.Version != version {
				t.Errorf("version = %q, want %q", identity.Version, version)
			}
			if _, err := time.Parse(time.RFC3339, identity.StartedAt); err != nil {
				t.Errorf("invalid started_at %q: %v", identity.StartedAt, err)
			}
		})
	}
}
