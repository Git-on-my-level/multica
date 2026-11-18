package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Fork account pools: a report carries its account and the other pool
// accounts' snapshots one level deep; the stored JSON keeps both.
func TestRuntimeRateLimitSnapshotKeepsAccounts(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	raw := `{"observed_at":"2026-11-19T11:59:00Z","account":"default","windows":[{"kind":"weekly","used_percent":91}],
	  "accounts":[{"account":"claude-2","observed_at":"2026-11-19T10:00:00Z","windows":[{"kind":"weekly","used_percent":12},{"kind":"future_kind"}]}]}`
	var s runtimeRateLimitSnapshot
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	if !s.normalize(now) {
		t.Fatal("valid pooled report rejected")
	}
	out, _ := json.Marshal(s)
	if !strings.Contains(string(out), `"account":"default"`) || !strings.Contains(string(out), `"account":"claude-2"`) || strings.Contains(string(out), "future_kind") {
		t.Fatalf("stored snapshot = %s", out)
	}

	for name, bad := range map[string]string{
		"nested accounts":     `{"observed_at":"2026-11-19T11:59:00Z","windows":[],"accounts":[{"account":"a","observed_at":"2026-11-19T11:00:00Z","windows":[],"accounts":[{"account":"b","observed_at":"2026-11-19T11:00:00Z","windows":[]}]}]}`,
		"sibling without id":  `{"observed_at":"2026-11-19T11:59:00Z","windows":[],"accounts":[{"observed_at":"2026-11-19T11:00:00Z","windows":[]}]}`,
		"sibling bad percent": `{"observed_at":"2026-11-19T11:59:00Z","windows":[],"accounts":[{"account":"a","observed_at":"2026-11-19T11:00:00Z","windows":[{"kind":"weekly","used_percent":-1}]}]}`,
	} {
		var s runtimeRateLimitSnapshot
		if err := json.Unmarshal([]byte(bad), &s); err != nil {
			t.Fatal(err)
		}
		if s.normalize(now) {
			t.Errorf("%s: accepted", name)
		}
	}
}
