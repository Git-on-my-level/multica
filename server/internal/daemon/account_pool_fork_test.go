package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/pkg/agent"
)

var poolTestNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func pct(v float64) *float64 { return &v }
func at(d time.Duration) *time.Time {
	t := poolTestNow.Add(d)
	return &t
}

func snapshotWith(windows ...agent.RateLimitWindow) *agent.RateLimitSnapshot {
	return &agent.RateLimitSnapshot{Windows: windows, ObservedAt: poolTestNow}
}

// newTestPool builds a pool over real credential paths in a temp dir.
func newTestPool(t *testing.T, provider string, ids ...string) (*accountPool, string) {
	t.Helper()
	root := t.TempDir()
	var specs []cli.AccountSpec
	for _, id := range ids {
		switch provider {
		case "codex":
			home := filepath.Join(root, id)
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"id":"`+id+`"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			specs = append(specs, cli.AccountSpec{ID: id, Home: home})
		case "claude":
			exe := filepath.Join(root, "claude-"+id)
			if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			specs = append(specs, cli.AccountSpec{ID: id, Executable: exe, ConfigDir: filepath.Join(root, ".claude-"+id)})
		}
	}
	defaultHome := filepath.Join(root, "default-codex")
	if err := os.MkdirAll(defaultHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defaultHome, "auth.json"), []byte(`{"id":"default"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newAccountPool(&cli.AccountPoolsConfig{Accounts: map[string][]cli.AccountSpec{provider: specs}}, filepath.Join(root, "state.json"), slog.New(slog.DiscardHandler))
	p.now = func() time.Time { return poolTestNow }
	p.codexSharedHome = func() string { return defaultHome }
	return p, root
}

func setSnapshot(p *accountPool, provider, id string, s *agent.RateLimitSnapshot) {
	p.accountStateLocked(provider, id).Snapshot = s
}

func rankedIDs(p *accountPool, provider string) []string {
	var ids []string
	for _, c := range p.rankLocked(provider, nil, p.loadFacts(provider)) {
		ids = append(ids, c.spec.ID)
	}
	return ids
}

func TestNewAccountPoolNilWithoutAccounts(t *testing.T) {
	if p := newAccountPool(nil, "", slog.Default()); p != nil {
		t.Fatal("nil config should give a nil pool")
	}
	if p := newAccountPool(&cli.AccountPoolsConfig{SwitchThreshold: 0.8}, "", slog.Default()); p != nil {
		t.Fatal("a config without accounts should give a nil pool")
	}
	var lease *taskAccount
	if got := lease.executable("/bin/claude"); got != "/bin/claude" {
		t.Fatalf("nil lease executable = %q", got)
	}
	if lease.failover(agent.Result{Status: "failed", Error: "You've hit your weekly limit"}, slog.Default()) {
		t.Fatal("nil lease must not fail over")
	}
}

func TestAccountPressure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		snap      *agent.RateLimitSnapshot
		wantRatio float64
		wantReset *time.Time
		wantKnown bool
	}{
		{"nil", nil, 0, nil, false},
		{"no usable window", snapshotWith(agent.RateLimitWindow{Kind: "weekly"}), 0, nil, false},
		{"highest window limits", snapshotWith(
			agent.RateLimitWindow{Kind: "session", UsedPercent: pct(20), ResetsAt: at(time.Hour)},
			agent.RateLimitWindow{Kind: "weekly", UsedPercent: pct(80), ResetsAt: at(48 * time.Hour)},
		), 0.8, at(48 * time.Hour), true},
		{"reset window is ignored", snapshotWith(
			agent.RateLimitWindow{Kind: "session", UsedPercent: pct(100), ResetsAt: at(-time.Minute)},
			agent.RateLimitWindow{Kind: "weekly", UsedPercent: pct(30), ResetsAt: at(24 * time.Hour)},
		), 0.3, at(24 * time.Hour), true},
		{"rejected counts as full", snapshotWith(
			agent.RateLimitWindow{Kind: "weekly", Status: agent.RateLimitStatusRejected, ResetsAt: at(5 * time.Hour)},
		), 1, at(5 * time.Hour), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ratio, reset, known := accountPressure(tc.snap, poolTestNow)
			if known != tc.wantKnown || ratio != tc.wantRatio {
				t.Fatalf("got ratio=%v known=%v, want %v %v", ratio, known, tc.wantRatio, tc.wantKnown)
			}
			if tc.wantReset != nil && !reset.Equal(*tc.wantReset) {
				t.Fatalf("reset = %v, want %v", reset, *tc.wantReset)
			}
		})
	}
}

func TestRankPrefersSoonestResetWithHeadroom(t *testing.T) {
	p, _ := newTestPool(t, "codex", "codex-2", "codex-3")
	// default: 91% used -> above 0.95-0.05 headroom line.
	setSnapshot(p, "codex", "default", snapshotWith(agent.RateLimitWindow{Kind: "weekly", UsedPercent: pct(91), ResetsAt: at(2 * time.Hour)}))
	// codex-2: 40%, resets in 3 days. codex-3: 70%, resets tomorrow.
	setSnapshot(p, "codex", "codex-2", snapshotWith(agent.RateLimitWindow{Kind: "weekly", UsedPercent: pct(40), ResetsAt: at(72 * time.Hour)}))
	setSnapshot(p, "codex", "codex-3", snapshotWith(agent.RateLimitWindow{Kind: "weekly", UsedPercent: pct(70), ResetsAt: at(24 * time.Hour)}))
	got := rankedIDs(p, "codex")
	want := []string{"codex-3", "codex-2", "default"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("rank = %v, want %v", got, want)
	}
}

func TestRankOrdersUnknownThenOverThresholdThenExhausted(t *testing.T) {
	p, _ := newTestPool(t, "codex", "codex-2", "codex-3")
	setSnapshot(p, "codex", "default", snapshotWith(agent.RateLimitWindow{Kind: "weekly", UsedPercent: pct(100), ResetsAt: at(time.Hour)}))
	p.accountStateLocked("codex", "default").ExhaustedUntil = poolTestNow.Add(time.Hour)
	setSnapshot(p, "codex", "codex-2", snapshotWith(agent.RateLimitWindow{Kind: "weekly", UsedPercent: pct(93), ResetsAt: at(time.Hour)}))
	got := rankedIDs(p, "codex")
	if len(got) != 3 || got[0] != "codex-3" || got[1] != "codex-2" || got[2] != "default" {
		t.Fatalf("rank = %v, want [codex-3 codex-2 default]", got)
	}
}

func TestRankSkipsMissingLogin(t *testing.T) {
	p, root := newTestPool(t, "codex", "codex-2")
	if err := os.Remove(filepath.Join(root, "codex-2", "auth.json")); err != nil {
		t.Fatal(err)
	}
	if got := rankedIDs(p, "codex"); len(got) != 1 || got[0] != "default" {
		t.Fatalf("rank = %v, want [default]", got)
	}
}

func TestSelectTaskAccountStaysOnSessionAccount(t *testing.T) {
	p, _ := newTestPool(t, "claude", "claude-2")
	d := &Daemon{accountPool: p}
	// claude-2 is preferred by reset time, but the prior session is the
	// default account's and it still has headroom.
	setSnapshot(p, "claude", "default", snapshotWith(agent.RateLimitWindow{Kind: "weekly", UsedPercent: pct(50), ResetsAt: at(72 * time.Hour)}))
	setSnapshot(p, "claude", "claude-2", snapshotWith(agent.RateLimitWindow{Kind: "weekly", UsedPercent: pct(10), ResetsAt: at(time.Hour)}))
	task := Task{PriorSessionID: "sess-1"}
	lease := d.selectTaskAccount("claude", &task, false, slog.New(slog.DiscardHandler))
	if lease == nil || lease.spec.ID != "default" {
		t.Fatalf("lease = %+v, want default", lease)
	}
	if task.PriorSessionID != "sess-1" || task.PriorSessionResumeUnavailable {
		t.Fatalf("resume must be kept: %+v", task)
	}
}

func TestSelectTaskAccountDropsResumeWhenSessionAccountIsSpent(t *testing.T) {
	p, _ := newTestPool(t, "claude", "claude-2")
	d := &Daemon{accountPool: p}
	setSnapshot(p, "claude", "claude-2", snapshotWith(agent.RateLimitWindow{Kind: "weekly", UsedPercent: pct(99), ResetsAt: at(time.Hour)}))
	p.state.Sessions = map[string]accountBound{accountKey("claude", "sess-2"): {Account: "claude-2", At: poolTestNow}}
	task := Task{PriorSessionID: "sess-2"}
	lease := d.selectTaskAccount("claude", &task, false, slog.New(slog.DiscardHandler))
	if lease == nil || lease.spec.ID != "default" {
		t.Fatalf("lease = %+v, want default", lease)
	}
	if task.PriorSessionID != "" || !task.PriorSessionResumeUnavailable {
		t.Fatalf("resume must be dropped with the continuity notice: %+v", task)
	}
}

func TestSelectTaskAccountBypass(t *testing.T) {
	p, _ := newTestPool(t, "claude", "claude-2")
	d := &Daemon{accountPool: p}
	log := slog.New(slog.DiscardHandler)
	if d.selectTaskAccount("claude", &Task{}, true, log) != nil {
		t.Fatal("custom runtime commands bypass the pool")
	}
	if d.selectTaskAccount("claude", &Task{Agent: &AgentData{CustomEnv: map[string]string{"ANTHROPIC_API_KEY": "k"}}}, false, log) != nil {
		t.Fatal("an agent pinned by custom_env bypasses the pool")
	}
	if d.selectTaskAccount("codex", &Task{}, false, log) != nil {
		t.Fatal("a provider without accounts has no lease")
	}
}

func TestFailoverMarksExhaustedAndMovesOn(t *testing.T) {
	p, _ := newTestPool(t, "claude", "claude-2")
	d := &Daemon{accountPool: p}
	log := slog.New(slog.DiscardHandler)
	lease := d.selectTaskAccount("claude", &Task{}, false, log)
	if lease.spec.ID != "default" {
		t.Fatalf("first pick = %s", lease.spec.ID)
	}
	// The run reported its weekly window as rejected before failing.
	lease.observeRateLimits(nil)(agent.RateLimitSnapshot{ObservedAt: poolTestNow, Windows: []agent.RateLimitWindow{
		{Kind: "weekly", UsedPercent: pct(100), Status: agent.RateLimitStatusRejected, ResetsAt: at(30 * time.Hour)},
	}})
	if lease.failover(agent.Result{Status: "failed", Error: "boom: process exited"}, log) {
		t.Fatal("a non-quota failure must not fail over")
	}
	if !lease.failover(agent.Result{Status: "failed", Error: "You've hit your weekly limit · resets Nov 20"}, log) {
		t.Fatal("quota failure should fail over to claude-2")
	}
	if lease.spec.ID != "claude-2" {
		t.Fatalf("lease moved to %s", lease.spec.ID)
	}
	if until := p.state.Accounts[accountKey("claude", "default")].ExhaustedUntil; !until.Equal(poolTestNow.Add(30 * time.Hour)) {
		t.Fatalf("default exhausted until %v, want the window reset", until)
	}
	// Both accounts tried: no further failover.
	if lease.failover(agent.Result{Status: "failed", Error: "You've hit your session limit"}, log) {
		t.Fatal("no account left to fail over to")
	}
	if until := p.state.Accounts[accountKey("claude", "claude-2")].ExhaustedUntil; !until.Equal(poolTestNow.Add(accountQuotaFallbackCooldown)) {
		t.Fatalf("claude-2 without a snapshot uses the fallback cooldown, got %v", until)
	}
}

func TestApplyEnvClaude(t *testing.T) {
	p, _ := newTestPool(t, "claude", "claude-2")
	lease := &taskAccount{pool: p, provider: "claude", spec: p.providers["claude"][1], tried: map[string]bool{}}
	env := map[string]string{}
	if err := lease.applyEnv(env, ""); err != nil {
		t.Fatal(err)
	}
	if env["CLAUDE_CONFIG_DIR"] != lease.spec.ConfigDir {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q", env["CLAUDE_CONFIG_DIR"])
	}
	if got := lease.executable("/usr/bin/claude"); got != lease.spec.Executable {
		t.Fatalf("executable = %q", got)
	}
	lease.spec = p.providers["claude"][0]
	if err := lease.applyEnv(env, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := env["CLAUDE_CONFIG_DIR"]; ok {
		t.Fatal("switching back to default must remove the config dir it set")
	}
}

func TestApplyEnvCodexRelinksAuth(t *testing.T) {
	p, root := newTestPool(t, "codex", "codex-2")
	taskHome := filepath.Join(root, "task-codex-home")
	if err := os.MkdirAll(taskHome, 0o700); err != nil {
		t.Fatal(err)
	}
	// execenv links the default login first.
	if err := os.Symlink(filepath.Join(p.codexSharedHome(), "auth.json"), filepath.Join(taskHome, "auth.json")); err != nil {
		t.Fatal(err)
	}
	lease := &taskAccount{pool: p, provider: "codex", spec: p.providers["codex"][1], tried: map[string]bool{}}
	if err := lease.applyEnv(map[string]string{}, taskHome); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(taskHome, "auth.json"))
	if err != nil || string(raw) != `{"id":"codex-2"}` {
		t.Fatalf("auth.json = %q, %v", raw, err)
	}
	lease.spec = p.providers["codex"][0]
	if err := lease.applyEnv(map[string]string{}, taskHome); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(taskHome, "auth.json")); string(raw) != `{"id":"default"}` {
		t.Fatalf("auth.json after switching back = %q", raw)
	}
}

func TestObserveTagsAccountAndSiblings(t *testing.T) {
	p, _ := newTestPool(t, "codex", "codex-2")
	setSnapshot(p, "codex", "codex-2", &agent.RateLimitSnapshot{Account: "codex-2", ObservedAt: poolTestNow,
		Windows: []agent.RateLimitWindow{{Kind: "weekly", UsedPercent: pct(12)}}})
	lease := &taskAccount{pool: p, provider: "codex", spec: p.providers["codex"][0], tried: map[string]bool{}}
	var got agent.RateLimitSnapshot
	lease.observeRateLimits(func(s agent.RateLimitSnapshot) { got = s })(agent.RateLimitSnapshot{ObservedAt: poolTestNow,
		Windows: []agent.RateLimitWindow{{Kind: "weekly", UsedPercent: pct(55)}}})
	if got.Account != "default" || len(got.Accounts) != 1 || got.Accounts[0].Account != "codex-2" {
		t.Fatalf("forwarded snapshot = %+v", got)
	}
	if p.state.Accounts[accountKey("codex", "default")].Snapshot.Accounts != nil {
		t.Fatal("stored snapshots must not nest siblings")
	}
}

func TestAccountPoolStatePersists(t *testing.T) {
	p, root := newTestPool(t, "codex", "codex-2")
	lease := &taskAccount{pool: p, provider: "codex", spec: p.providers["codex"][1], tried: map[string]bool{}}
	lease.bindSession("thread-1")
	p.accountStateLocked("codex", "default").ExhaustedUntil = poolTestNow.Add(time.Hour)
	lease.persist()

	again := newAccountPool(&cli.AccountPoolsConfig{Accounts: map[string][]cli.AccountSpec{"codex": {p.providers["codex"][1]}}}, filepath.Join(root, "state.json"), slog.New(slog.DiscardHandler))
	if b := again.state.Sessions[accountKey("codex", "thread-1")]; b.Account != "codex-2" {
		t.Fatalf("session binding = %+v", b)
	}
	if until := again.state.Accounts[accountKey("codex", "default")].ExhaustedUntil; !until.Equal(poolTestNow.Add(time.Hour)) {
		t.Fatalf("exhausted until = %v", until)
	}
}

func TestClaudeLookupEnvUsesAccountConfigDir(t *testing.T) {
	p, _ := newTestPool(t, "claude", "claude-2")
	lease := &taskAccount{pool: p, provider: "claude", spec: p.providers["claude"][1], tried: map[string]bool{}}
	base := map[string]string{"FOO": "1"}
	got := lease.claudeLookupEnv(base)
	if got["CLAUDE_CONFIG_DIR"] != lease.spec.ConfigDir || got["FOO"] != "1" {
		t.Fatalf("lookup env = %v", got)
	}
	if _, leaked := base["CLAUDE_CONFIG_DIR"]; leaked {
		t.Fatal("claudeLookupEnv must not mutate its input")
	}
	var none *taskAccount
	if got := none.claudeLookupEnv(base); len(got) != 1 {
		t.Fatalf("nil lease lookup env = %v", got)
	}
}

func TestSessionBindsWhileTaskRuns(t *testing.T) {
	p, _ := newTestPool(t, "codex", "codex-2")
	d := &Daemon{accountPool: p}
	lease := &taskAccount{pool: p, provider: "codex", spec: p.providers["codex"][1], tried: map[string]bool{}}
	unregister := d.registerTaskAccount("task-1", lease)
	d.bindTaskAccountSession("task-1", "thread-9")
	unregister()
	d.bindTaskAccountSession("task-1", "thread-10") // after the run: ignored
	if b := p.state.Sessions[accountKey("codex", "thread-9")]; b.Account != "codex-2" {
		t.Fatalf("binding = %+v", b)
	}
	if _, ok := p.state.Sessions[accountKey("codex", "thread-10")]; ok {
		t.Fatal("an unregistered task must not bind sessions")
	}
}

func TestObserveCapsSiblings(t *testing.T) {
	ids := []string{}
	for i := 0; i < cli.MaxExtraAccounts; i++ {
		ids = append(ids, "codex-"+string(rune('a'+i)))
	}
	p, _ := newTestPool(t, "codex", ids...)
	for _, spec := range p.providers["codex"] {
		setSnapshot(p, "codex", spec.ID, &agent.RateLimitSnapshot{Account: spec.ID, ObservedAt: poolTestNow})
	}
	lease := &taskAccount{pool: p, provider: "codex", spec: p.providers["codex"][0], tried: map[string]bool{}}
	var got agent.RateLimitSnapshot
	lease.observeRateLimits(func(s agent.RateLimitSnapshot) { got = s })(agent.RateLimitSnapshot{ObservedAt: poolTestNow})
	if len(got.Accounts) > cli.MaxExtraAccounts {
		t.Fatalf("%d siblings exceed the server limit %d", len(got.Accounts), cli.MaxExtraAccounts)
	}
}
