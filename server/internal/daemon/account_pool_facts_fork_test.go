package daemon

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/pkg/agent"
)

var drainTestNow = time.Date(2026, 11, 24, 5, 30, 0, 0, time.UTC)

func newDrainPool(t *testing.T, provider, defaultID, extraID, policy string) *accountPool {
	t.Helper()
	p, root := newTestPool(t, provider, extraID)
	p = newAccountPool(&cli.AccountPoolsConfig{
		Policy:          policy,
		FactsFile:       filepath.Join(root, "facts.json"),
		DefaultFactsIDs: map[string]string{provider: defaultID},
		Accounts:        map[string][]cli.AccountSpec{provider: p.providers[provider][1:]},
	}, "", p.log)
	p.now = func() time.Time { return drainTestNow }
	return p
}

func writeAccountFacts(t *testing.T, p *accountPool, generatedAt time.Time, accounts string) {
	t.Helper()
	body := fmt.Sprintf(`{"schema_version":1,"generated_at":%q,"accounts":%s}`, generatedAt.Format(time.RFC3339), accounts)
	if err := os.WriteFile(p.factsFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const claudeDrainFacts = `[
	{"id":"claude-1","windows":[{"name":"weekly","used_ratio":0.90,"resets_at":"2026-11-25T05:30:00Z"}]},
	{"id":"claude-2","windows":[{"name":"5h","used_ratio":0.38,"resets_at":"2026-11-24T07:30:00Z"},
	                          {"name":"weekly","used_ratio":0.21,"resets_at":"2026-11-28T05:30:00Z"}]}
]`

func codexDrainFacts(defaultUsage, otherUsage float64) string {
	return fmt.Sprintf(`[
		{"id":"codex","windows":[{"name":"weekly","used_ratio":%g,"resets_at":"2026-11-26T05:30:00Z"}]},
		{"id":"codex-2","windows":[{"name":"weekly","used_ratio":%g,"resets_at":"2026-11-29T05:30:00Z"}]}
	]`, defaultUsage, otherUsage)
}

func TestAccountPoolDrainPacketFacts(t *testing.T) {
	for _, tc := range []struct {
		name, provider, defaultID, extraID, facts, want string
	}{
		{"claude short reset cannot win", "claude", "claude-2", "claude-1", claudeDrainFacts, "claude-1"},
		{"codex drains the earlier window", "codex", "codex", "codex-2", codexDrainFacts(0.90, 0.16), "default"},
		{"codex over budget switches", "codex", "codex", "codex-2", codexDrainFacts(0.975, 0.16), "codex-2"},
		{"headroom boundary is inclusive", "codex", "codex", "codex-2", codexDrainFacts(0.96, 0.16), "default"},
		{"all over budget picks least used", "codex", "codex", "codex-2", codexDrainFacts(0.99, 0.98), "codex-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newDrainPool(t, tc.provider, tc.defaultID, tc.extraID, "")
			writeAccountFacts(t, p, drainTestNow, tc.facts)
			if got := rankedIDs(p, tc.provider)[0]; got != tc.want {
				t.Fatalf("next account = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestAccountPoolDrainShortWindowAndTie(t *testing.T) {
	p := newDrainPool(t, "claude", "claude-2", "claude-1", "")
	for _, tc := range []struct {
		name       string
		short      float64
		otherLong  float64
		otherReset string
		want       string
	}{
		{"short window vetoes earlier reset", 0.975, 0.4, "2026-11-28", "claude-1"},
		{"tie uses pressure on latest window", 0.9, 0.4, "2026-11-25", "default"},
		{"equal pressure keeps configured order", 0.9, 0.2, "2026-11-25", "default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeAccountFacts(t, p, drainTestNow, fmt.Sprintf(`[
				{"id":"claude-2","windows":[{"name":"weekly","used_ratio":%g,"resets_at":"2026-11-24T07:30:00Z"},
				                          {"name":"5h","used_ratio":0.2,"resets_at":"2026-11-25T05:30:00Z"}]},
				{"id":"claude-1","windows":[{"name":"weekly","used_ratio":%g,"resets_at":"%sT05:30:00Z"}]}
			]`, tc.short, tc.otherLong, tc.otherReset))
			if got := rankedIDs(p, "claude")[0]; got != tc.want {
				t.Fatalf("next account = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestAccountPoolFactsFallbackUsesLegacySnapshots(t *testing.T) {
	for _, tc := range []struct {
		name, policy, reason, accounts string
		age                            time.Duration
	}{
		{name: "kill switch", policy: cli.AccountPolicySoonestReset, accounts: claudeDrainFacts},
		{name: "unconfigured", reason: "not_configured"},
		{name: "unreadable", reason: "unreadable_or_invalid"},
		{name: "invalid", reason: "unreadable_or_invalid", accounts: `[{"id":"claude-2","windows":[{"name":"5h"}]}]`},
		{name: "stale file", reason: "stale", accounts: claudeDrainFacts, age: time.Hour + time.Second},
		{name: "incomplete pool", reason: "account_facts_missing", accounts: `[]`},
		{name: "stale window", reason: "window_observation_out_of_date", accounts: `[
			{"id":"claude-2","windows":[{"name":"5h","used_ratio":0.6,"observed_at":"2026-11-24T04:00:00Z"}]}
		]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newDrainPool(t, "claude", "claude-2", "claude-1", tc.policy)
			var log bytes.Buffer
			p.log = slog.New(slog.NewTextHandler(&log, nil))
			if tc.reason == "not_configured" {
				p.factsFile = ""
			}
			if tc.accounts != "" {
				writeAccountFacts(t, p, drainTestNow.Add(-tc.age), tc.accounts)
			}
			short, long := drainTestNow.Add(2*time.Hour), drainTestNow.Add(24*time.Hour)
			setSnapshot(p, "claude", "default", snapshotWith(agent.RateLimitWindow{UsedPercent: pct(38), ResetsAt: &short}))
			setSnapshot(p, "claude", "claude-1", snapshotWith(agent.RateLimitWindow{UsedPercent: pct(90), ResetsAt: &long}))
			if got := rankedIDs(p, "claude")[0]; got != "default" {
				t.Fatalf("legacy next account = %s, want default", got)
			}
			if tc.reason == "" {
				if log.Len() != 0 {
					t.Fatalf("kill switch emitted a facts warning: %s", &log)
				}
			} else if !bytes.Contains(log.Bytes(), []byte("reason="+tc.reason)) {
				t.Fatalf("missing %s warning: %s", tc.reason, &log)
			}
		})
	}
}

func TestAccountPoolDrainDebitsAndRetiresOnObservation(t *testing.T) {
	p := newDrainPool(t, "codex", "codex", "codex-2", "")
	d := &Daemon{accountPool: p}
	writeAccountFacts(t, p, drainTestNow, codexDrainFacts(0.96, 0.16))
	p.now = func() time.Time { return drainTestNow.Add(time.Second) }
	log := slog.New(slog.DiscardHandler)
	first := d.selectTaskAccount("codex", &Task{}, false, log)
	if first.spec.ID != "default" {
		t.Fatalf("first task selected %s, want default", first.spec.ID)
	}
	expectNext := func(want string) {
		t.Helper()
		next := d.selectTaskAccount("codex", &Task{}, false, log)
		defer next.release()
		if next.spec.ID != want {
			t.Fatalf("next task selected %s, want %s", next.spec.ID, want)
		}
	}
	expectNext("codex-2")
	first.attempted = true
	first.release()
	expectNext("codex-2")
	writeAccountFacts(t, p, p.now(), codexDrainFacts(0.96, 0.16))
	expectNext("default")
	expectNext("default") // A lease released before execution leaves no debit.
}

func TestAccountPoolDrainFailoverTransfersTheReservation(t *testing.T) {
	p := newDrainPool(t, "codex", "codex", "codex-2", "")
	d := &Daemon{accountPool: p}
	writeAccountFacts(t, p, drainTestNow, codexDrainFacts(0.96, 0.16))
	log := slog.New(slog.DiscardHandler)
	lease := d.selectTaskAccount("codex", &Task{}, false, log)
	observeFirst := lease.observeRateLimits(nil)
	lease.attempted = true
	if !lease.failover(agent.Result{Status: "failed", Error: "You've hit your weekly limit"}, log) {
		t.Fatal("expected quota failover")
	}
	if p.active["codex/default"] != 0 || p.active["codex/codex-2"] != 1 {
		t.Fatalf("failover did not transfer the reservation: %v", p.active)
	}
	snapshot := snapshotWith(agent.RateLimitWindow{UsedPercent: pct(100)})
	observeFirst(*snapshot)
	if p.state.Accounts["codex/codex-2"] != nil {
		t.Fatal("a late snapshot from the first account was attributed to the replacement account")
	}
	lease.release()
	if p.active["codex/codex-2"] != 0 || len(p.completed["codex/codex-2"]) != 0 {
		t.Fatalf("failover which never started left a debit: %v, %v", p.active, p.completed)
	}
}

func TestAccountPoolStatusShowsFactsWithConfiguredLimits(t *testing.T) {
	p, root := newTestPool(t, "codex", "codex-2")
	t.Setenv(cli.TaskConfigRootEnv, root)
	p.factsFile = filepath.Join(root, "facts.json")
	now := time.Now().UTC().Truncate(time.Second)
	firstReset := now.Add(24 * time.Hour)
	writeAccountFacts(t, p, now.Add(-2*time.Hour), fmt.Sprintf(`[
		{"id":"codex","windows":[{"name":"weekly","used_ratio":0.9,"resets_at":%q}]},
		{"id":"codex-2","windows":[{"name":"weekly","used_ratio":0.16,"resets_at":%q}]}
	]`, firstReset.Format(time.RFC3339), firstReset.Add(24*time.Hour).Format(time.RFC3339)))
	cfg := &cli.AccountPoolsConfig{
		Accounts:        map[string][]cli.AccountSpec{"codex": p.providers["codex"][1:]},
		FactsFile:       p.factsFile,
		DefaultFactsIDs: map[string]string{"codex": "codex"},
	}
	rows := AccountPoolStatus(cfg, "")
	if len(rows) != 2 || !rows[0].Next || rows[0].UsedPercent != nil {
		t.Fatalf("stale facts should use local state: %+v", rows)
	}
	cfg.FactsMaxAgeSeconds = 3 * 3600
	cfg.ExpectedRunCost = pct(0.10)
	cfg.Epsilon = pct(0.04)
	rows = AccountPoolStatus(cfg, "")
	if len(rows) != 2 || !rows[1].Next || rows[0].UsedPercent == nil || *rows[0].UsedPercent != 90 ||
		rows[0].ResetsAt == nil || !rows[0].ResetsAt.Equal(firstReset) {
		t.Fatalf("configured limits should select codex-2 and display facts: %+v", rows)
	}
}
