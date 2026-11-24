package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// Fork overlay: subscription account pools.
//
// A provider with extra accounts configured (cli.AccountPoolsConfig) runs each
// task under the account with the most usable plan headroom, and when a run
// fails because its account's plan window is spent, the daemon marks that
// account exhausted until the window resets and re-runs the task once on each
// remaining account. With fresh capacity facts, selection favors accounts
// whose last window resets earliest and whose projected usage fits every
// window. The legacy policy uses the limiting window from local snapshots.
//
// State (latest snapshot per account, exhaustion, which account owns which
// session) is daemon-local and persisted next to the profile so a restart does
// not forget a spent account. Sessions are bound to the account that created
// them: Claude refuses to resume another account's session and Codex keeps
// account-specific reasoning in its rollouts, so a task that has to move to a
// different account starts a fresh session with the continuity notice.

const (
	// accountHeadroom is the margin below the switch threshold an account
	// needs to count as preferred.
	accountHeadroom = 0.05
	// accountQuotaFallbackCooldown is how long an account stays exhausted
	// after a quota failure when no snapshot says when its window resets.
	accountQuotaFallbackCooldown = time.Hour
	accountSessionBindingTTL     = 30 * 24 * time.Hour
	accountSessionBindingMax     = 5000
	accountPoolStateVersion      = 1
	accountPoolStateFile         = "account-pools-state.json"
)

type accountPool struct {
	mu              sync.Mutex
	threshold       float64
	policy          string
	factsFile       string
	factsMaxAge     time.Duration
	epsilon         float64
	expectedRunCost float64
	active          map[string]int
	completed       map[string][]time.Time
	providers       map[string][]cli.AccountSpec // default account first
	state           accountPoolState
	statePath       string
	now             func() time.Time
	log             *slog.Logger
	// codexSharedHome resolves the daemon's own CODEX_HOME, the default
	// account's credential home.
	codexSharedHome func() string
}

type accountPoolState struct {
	Version  int                      `json:"version"`
	Accounts map[string]*accountState `json:"accounts,omitempty"`
	Sessions map[string]accountBound  `json:"sessions,omitempty"`
}

type accountState struct {
	Snapshot       *agent.RateLimitSnapshot `json:"snapshot,omitempty"`
	ExhaustedUntil time.Time                `json:"exhausted_until,omitempty"`
}

type accountBound struct {
	Account string    `json:"account"`
	At      time.Time `json:"at"`
}

func accountKey(provider, id string) string { return provider + "/" + id }

// newAccountPool returns nil when no provider has extra accounts, which keeps
// every call site a no-op.
func newAccountPool(cfg *cli.AccountPoolsConfig, statePath string, log *slog.Logger) *accountPool {
	if cfg == nil || len(cfg.Accounts) == 0 {
		return nil
	}
	p := &accountPool{
		threshold:       cli.DefaultAccountSwitchThreshold,
		policy:          cfg.Policy,
		factsFile:       cfg.FactsFile,
		factsMaxAge:     cli.DefaultAccountFactsMaxAge,
		epsilon:         cli.DefaultAccountEpsilon,
		expectedRunCost: cli.DefaultAccountExpectedRunCost,
		providers:       map[string][]cli.AccountSpec{},
		statePath:       statePath,
		now:             time.Now,
		log:             log,
		codexSharedHome: defaultCodexSharedHome,
		state:           accountPoolState{Version: accountPoolStateVersion},
	}
	if cfg.SwitchThreshold > 0 {
		p.threshold = cfg.SwitchThreshold
	}
	if cfg.FactsMaxAgeSeconds > 0 {
		p.factsMaxAge = time.Duration(cfg.FactsMaxAgeSeconds) * time.Second
	}
	if cfg.Epsilon != nil {
		p.epsilon = *cfg.Epsilon
	}
	if cfg.ExpectedRunCost != nil {
		p.expectedRunCost = *cfg.ExpectedRunCost
	}
	if p.policy != cli.AccountPolicySoonestReset && p.factsFile != "" {
		p.active = map[string]int{}
		p.completed = map[string][]time.Time{}
	}
	for provider, specs := range cfg.Accounts {
		if len(specs) == 0 {
			continue
		}
		pool := append([]cli.AccountSpec{{ID: cli.DefaultAccountID, FactsID: cfg.DefaultFactsIDs[provider]}}, specs...)
		for i := 1; i < len(pool); i++ {
			if pool[i].FactsID == "" {
				pool[i].FactsID = pool[i].ID
			}
		}
		p.providers[provider] = pool
	}
	if len(p.providers) == 0 {
		return nil
	}
	p.load()
	return p
}

func defaultCodexSharedHome() string {
	if v := strings.TrimSpace(os.Getenv("CODEX_HOME")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func (p *accountPool) load() {
	if p.statePath == "" {
		return
	}
	raw, err := os.ReadFile(p.statePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			p.log.Warn("account pool: read state failed; starting empty", "path", p.statePath, "error", err)
		}
		return
	}
	var st accountPoolState
	if err := json.Unmarshal(raw, &st); err != nil || st.Version != accountPoolStateVersion {
		p.log.Warn("account pool: ignoring unreadable state", "path", p.statePath, "error", err)
		return
	}
	p.state = st
}

// saveLocked persists state. Failures are logged, not fatal: the pool still
// works from memory.
func (p *accountPool) saveLocked() {
	if p.statePath == "" {
		return
	}
	p.pruneSessionsLocked()
	raw, err := json.Marshal(p.state)
	if err != nil {
		return
	}
	tmp := p.statePath + ".tmp"
	if err := os.MkdirAll(filepath.Dir(p.statePath), 0o700); err == nil {
		if err = os.WriteFile(tmp, raw, 0o600); err == nil {
			err = os.Rename(tmp, p.statePath)
		}
		if err == nil {
			return
		}
		p.log.Warn("account pool: save state failed", "path", p.statePath, "error", err)
	}
}

func (p *accountPool) pruneSessionsLocked() {
	if len(p.state.Sessions) == 0 {
		return
	}
	cutoff := p.now().Add(-accountSessionBindingTTL)
	for k, b := range p.state.Sessions {
		if b.At.Before(cutoff) {
			delete(p.state.Sessions, k)
		}
	}
	if over := len(p.state.Sessions) - accountSessionBindingMax; over > 0 {
		keys := make([]string, 0, len(p.state.Sessions))
		for k := range p.state.Sessions {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return p.state.Sessions[keys[i]].At.Before(p.state.Sessions[keys[j]].At) })
		for _, k := range keys[:over] {
			delete(p.state.Sessions, k)
		}
	}
}

func (p *accountPool) accountStateLocked(provider, id string) *accountState {
	if p.state.Accounts == nil {
		p.state.Accounts = map[string]*accountState{}
	}
	key := accountKey(provider, id)
	st := p.state.Accounts[key]
	if st == nil {
		st = &accountState{}
		p.state.Accounts[key] = st
	}
	return st
}

// accountPressure summarizes a snapshot: the highest used share (0-1) among
// windows that have not reset yet, and when that limiting window resets.
// known is false when the snapshot carries no usable window.
func accountPressure(s *agent.RateLimitSnapshot, now time.Time) (ratio float64, resetsAt time.Time, known bool) {
	if s == nil {
		return 0, time.Time{}, false
	}
	for _, w := range s.Windows {
		if w.ResetsAt != nil && !w.ResetsAt.After(now) {
			continue // that window has reset since the observation
		}
		r := -1.0
		if w.UsedPercent != nil {
			r = *w.UsedPercent / 100
		}
		if w.Status == agent.RateLimitStatusRejected && r < 1 {
			r = 1
		}
		if r < 0 {
			continue
		}
		var reset time.Time
		if w.ResetsAt != nil {
			reset = *w.ResetsAt
		}
		if !known || r > ratio || (r == ratio && reset.After(resetsAt)) {
			ratio, resetsAt, known = r, reset, true
		}
	}
	return ratio, resetsAt, known
}

// usable reports whether the account's credentials are present on this
// machine. A missing login is skipped rather than launched into a failure.
func (p *accountPool) usable(provider string, spec cli.AccountSpec) bool {
	if spec.ID == cli.DefaultAccountID {
		return true
	}
	switch provider {
	case "claude":
		if spec.Executable != "" {
			info, err := os.Stat(spec.Executable)
			if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
				return false
			}
		}
		if spec.ConfigDir != "" && spec.Executable == "" {
			if info, err := os.Stat(spec.ConfigDir); err != nil || !info.IsDir() {
				return false
			}
		}
		return true
	case "codex":
		_, err := os.Stat(filepath.Join(spec.Home, "auth.json"))
		return err == nil
	}
	return false
}

type accountCandidate struct {
	spec          cli.AccountSpec
	tier          int // 0 preferred, 1 unknown usage, 2 over threshold, 3 exhausted
	ratio         float64
	resetsAt      time.Time
	exhausted     time.Time
	order         int
	orderReset    time.Time
	orderPressure float64
}

// rankLocked orders the provider's usable accounts, best first, leaving out
// those in skip.
func (p *accountPool) rankLocked(provider string, skip map[string]bool, facts map[string][]accountFactWindow) []accountCandidate {
	now := p.now()
	var out []accountCandidate
	for i, spec := range p.providers[provider] {
		if skip[spec.ID] || !p.usable(provider, spec) {
			continue
		}
		c := accountCandidate{spec: spec, order: i}
		st := p.state.Accounts[accountKey(provider, spec.ID)]
		var snap *agent.RateLimitSnapshot
		if st != nil {
			snap = st.Snapshot
		}
		ratio, reset, known := accountPressure(snap, now)
		c.ratio, c.resetsAt, c.orderReset = ratio, reset, reset
		switch {
		case st != nil && st.ExhaustedUntil.After(now):
			c.tier, c.exhausted = 3, st.ExhaustedUntil
		case !known:
			c.tier = 1
		case ratio <= p.threshold-accountHeadroom:
			c.tier = 0
		default:
			c.tier = 2
		}
		out = append(out, c)
	}
	p.applyCapacityFacts(provider, out, facts, now)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		switch a.tier {
		case 0:
			if !a.orderReset.Equal(b.orderReset) {
				return b.orderReset.IsZero() || (!a.orderReset.IsZero() && a.orderReset.Before(b.orderReset))
			}
			if a.orderPressure != b.orderPressure {
				return a.orderPressure < b.orderPressure
			}
		case 2:
			if a.ratio != b.ratio {
				return a.ratio < b.ratio
			}
		case 3:
			if !a.exhausted.Equal(b.exhausted) {
				return a.exhausted.Before(b.exhausted)
			}
		}
		return a.order < b.order
	})
	return out
}

// taskAccount is the account one task runs under.
type taskAccount struct {
	pool      *accountPool
	provider  string
	spec      cli.AccountSpec
	tried     map[string]bool
	attempted bool
	// setConfigDir records that this lease put CLAUDE_CONFIG_DIR into the
	// task env, so switching back to the default account removes it.
	setConfigDir bool
}

// accountPoolPinnedEnv lists custom_env keys that pin an agent to a specific
// login or API key; such agents bypass the pool.
var accountPoolPinnedEnv = map[string][]string{
	"claude": {"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"},
	"codex":  {"OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL"},
}

// selectTaskAccount picks the account for a task, or returns nil when the
// provider has no pool or the task is pinned (custom runtime command, or an
// agent whose custom_env selects its own login). When the task would resume
// a session owned by a different account than the one picked, the resume is
// dropped so the run starts fresh with the continuity notice.
func (d *Daemon) selectTaskAccount(provider string, task *Task, customCommand bool, taskLog *slog.Logger) *taskAccount {
	p := d.accountPool
	if p == nil || customCommand || len(p.providers[provider]) == 0 {
		return nil
	}
	if task.Agent != nil {
		for _, key := range accountPoolPinnedEnv[provider] {
			if strings.TrimSpace(task.Agent.CustomEnv[key]) != "" {
				return nil
			}
		}
	}
	facts := p.loadFacts(provider)
	p.mu.Lock()
	ranked := p.rankLocked(provider, nil, facts)
	if len(ranked) == 0 {
		p.mu.Unlock()
		return nil
	}
	chosen := ranked[0]
	sessionAccount := ""
	if task.PriorSessionID != "" {
		sessionAccount = cli.DefaultAccountID
		if b, ok := p.state.Sessions[accountKey(provider, task.PriorSessionID)]; ok {
			sessionAccount = b.Account
		}
		// Stay on the session's account while it still has headroom, so the
		// conversation resumes instead of restarting.
		for _, c := range ranked {
			if c.spec.ID == sessionAccount && c.tier <= 1 {
				chosen = c
				break
			}
		}
	}
	p.reserveLocked(provider, chosen.spec.ID)
	p.mu.Unlock()

	if sessionAccount != "" && chosen.spec.ID != sessionAccount {
		taskLog.Info("account pool: prior session belongs to another account; starting fresh",
			"provider", provider, "session_account", sessionAccount, "account", chosen.spec.ID)
		task.PriorSessionID = ""
		task.PriorSessionResumeUnavailable = true
	}
	taskLog.Info("account pool: selected account", "provider", provider, "account", chosen.spec.ID,
		"used", fmt.Sprintf("%.2f", chosen.ratio), "tier", chosen.tier)
	return &taskAccount{pool: p, provider: provider, spec: chosen.spec, tried: map[string]bool{chosen.spec.ID: true}}
}

// executable returns the launcher for this account, or defaultPath.
func (t *taskAccount) executable(defaultPath string) string {
	if t == nil || t.spec.Executable == "" {
		return defaultPath
	}
	return t.spec.Executable
}

// applyEnv points the task's environment at this account's credentials.
func (t *taskAccount) applyEnv(env map[string]string, codexHome string) error {
	if t == nil {
		return nil
	}
	switch t.provider {
	case "claude":
		if t.spec.ConfigDir != "" {
			env["CLAUDE_CONFIG_DIR"] = t.spec.ConfigDir
			t.setConfigDir = true
		} else if t.setConfigDir {
			delete(env, "CLAUDE_CONFIG_DIR")
			t.setConfigDir = false
		}
	case "codex":
		if codexHome == "" {
			return errors.New("account pool: task has no CODEX_HOME")
		}
		source := t.spec.Home
		if t.spec.ID == cli.DefaultAccountID {
			source = t.pool.codexSharedHome()
		}
		if source == "" {
			return errors.New("account pool: cannot resolve the default CODEX_HOME")
		}
		return linkCodexAuth(codexHome, filepath.Join(source, "auth.json"))
	}
	return nil
}

// linkCodexAuth makes the per-task CODEX_HOME's auth.json a symlink to the
// account's own auth.json, the same sharing execenv sets up for the default
// login, so token refreshes land in the account's home.
func linkCodexAuth(codexHome, source string) error {
	dst := filepath.Join(codexHome, "auth.json")
	if current, err := os.Readlink(dst); err == nil && current == source {
		return nil
	}
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("account pool: codex auth %s: %w", source, err)
	}
	tmp := dst + ".account-pool.tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(source, tmp); err != nil {
		return fmt.Errorf("account pool: link codex auth: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("account pool: link codex auth: %w", err)
	}
	return nil
}

// observeRateLimits records each snapshot against this account and forwards
// it tagged with the account and the other accounts' latest snapshots.
func (t *taskAccount) observeRateLimits(next func(agent.RateLimitSnapshot)) func(agent.RateLimitSnapshot) {
	if t == nil {
		return next
	}
	id := t.spec.ID
	return func(s agent.RateLimitSnapshot) {
		p := t.pool
		p.mu.Lock()
		stored := s
		stored.Account, stored.Accounts = id, nil
		st := p.accountStateLocked(t.provider, id)
		st.Snapshot = &stored
		if ratio, reset, known := accountPressure(&stored, p.now()); known && ratio >= 1 && reset.After(st.ExhaustedUntil) {
			st.ExhaustedUntil = reset
		}
		var siblings []agent.RateLimitSnapshot
		for _, spec := range p.providers[t.provider] {
			if spec.ID == id {
				continue
			}
			if other := p.state.Accounts[accountKey(t.provider, spec.ID)]; other != nil && other.Snapshot != nil && len(siblings) < cli.MaxExtraAccounts {
				siblings = append(siblings, *other.Snapshot)
			}
		}
		p.mu.Unlock()
		if next != nil {
			out := stored
			out.Accounts = siblings
			next(out)
		}
	}
}

// persist saves the pool state after a run. Snapshots arrive on the
// backend's reader goroutine, which must not block on disk, so they are only
// kept in memory until here.
func (t *taskAccount) persist() {
	if t == nil {
		return
	}
	t.pool.mu.Lock()
	t.pool.saveLocked()
	t.pool.mu.Unlock()
}

// bindSession records which account owns a session the run established.
func (t *taskAccount) bindSession(sessionID string) {
	if t == nil || sessionID == "" {
		return
	}
	p := t.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.Sessions == nil {
		p.state.Sessions = map[string]accountBound{}
	}
	p.state.Sessions[accountKey(t.provider, sessionID)] = accountBound{Account: t.spec.ID, At: p.now().UTC()}
	p.saveLocked()
}

// failover handles a finished run. When the run failed on a spent plan
// window it marks this account exhausted and, if another account is not yet
// tried and not itself exhausted, switches the lease to it and returns true.
func (t *taskAccount) failover(result agent.Result, taskLog *slog.Logger) bool {
	if t == nil || result.Status == "completed" || result.Status == "cancelled" {
		return false
	}
	if taskfailure.Classify(result.Error) != taskfailure.ReasonAgentProviderQuotaLimit {
		return false
	}
	p := t.pool
	facts := p.loadFacts(t.provider)
	p.mu.Lock()
	now := p.now()
	priorID := t.spec.ID
	st := p.accountStateLocked(t.provider, priorID)
	until := now.Add(accountQuotaFallbackCooldown)
	if ratio, reset, known := accountPressure(st.Snapshot, now); known && ratio >= p.threshold && reset.After(now) {
		until = reset
	}
	if until.After(st.ExhaustedUntil) {
		st.ExhaustedUntil = until
	}
	var next *accountCandidate
	for _, c := range p.rankLocked(t.provider, t.tried, facts) {
		if c.tier < 3 {
			c := c
			next = &c
			break
		}
	}
	if next != nil {
		t.releaseLocked()
		t.spec = next.spec
		t.tried[next.spec.ID] = true
		p.reserveLocked(t.provider, next.spec.ID)
	}
	p.saveLocked()
	p.mu.Unlock()
	taskLog.Warn("account pool: account hit its plan limit", "provider", t.provider, "account", priorID, "exhausted_until", until.UTC().Format(time.RFC3339))
	if next == nil {
		return false
	}
	taskLog.Info("account pool: failing over", "provider", t.provider, "from", priorID, "to", next.spec.ID)
	return true
}

// claudeLookupEnv returns childEnv with this account's CLAUDE_CONFIG_DIR, for
// daemon-side lookups of the account's Claude session files that run before
// the task env is assembled (worktree transcript migration).
func (t *taskAccount) claudeLookupEnv(childEnv map[string]string) map[string]string {
	if t == nil || t.provider != "claude" || t.spec.ConfigDir == "" {
		return childEnv
	}
	out := make(map[string]string, len(childEnv)+1)
	for k, v := range childEnv {
		out[k] = v
	}
	out["CLAUDE_CONFIG_DIR"] = t.spec.ConfigDir
	return out
}

// registerTaskAccount makes the lease reachable from executeAndDrain, which
// binds a session to its account as soon as the run reports it, so a daemon
// crash mid-run cannot leave a pinned session unattributed. The returned func
// unregisters.
func (d *Daemon) registerTaskAccount(taskID string, t *taskAccount) func() {
	if t == nil {
		return func() {}
	}
	d.taskAccounts.Store(taskID, t)
	return func() { d.taskAccounts.Delete(taskID) }
}

func (d *Daemon) bindTaskAccountSession(taskID, sessionID string) {
	if v, ok := d.taskAccounts.Load(taskID); ok {
		v.(*taskAccount).bindSession(sessionID)
	}
}

// accountPoolStatePath is where a profile's pool state lives.
func accountPoolStatePath(profile string) string {
	dir, err := cli.ProfileDir(profile)
	if err != nil {
		return ""
	}
	return filepath.Join(dir, accountPoolStateFile)
}

// AccountPoolStatusRow is one account as `multica runtime accounts list`
// shows it.
type AccountPoolStatusRow struct {
	Provider    string     `json:"provider"`
	Account     string     `json:"account"`
	Location    string     `json:"location"`
	UsedPercent *float64   `json:"used_percent,omitempty"`
	ResetsAt    *time.Time `json:"resets_at,omitempty"`
	State       string     `json:"state"`
	// Next marks the account the daemon would pick for a new task.
	Next bool `json:"next"`
}

// AccountPoolStatus reports the configured pools with the state the daemon
// last persisted for profile, in pool order.
func AccountPoolStatus(cfg *cli.AccountPoolsConfig, profile string) []AccountPoolStatusRow {
	p := newAccountPool(cfg, accountPoolStatePath(profile), slog.New(slog.DiscardHandler))
	if p == nil {
		return nil
	}
	providers := make([]string, 0, len(p.providers))
	for provider := range p.providers {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	var rows []AccountPoolStatusRow
	for _, provider := range providers {
		ranked := p.rankLocked(provider, nil, p.loadFacts(provider))
		byID := map[string]accountCandidate{}
		for _, c := range ranked {
			byID[c.spec.ID] = c
		}
		for _, spec := range p.providers[provider] {
			row := AccountPoolStatusRow{Provider: provider, Account: spec.ID, Location: accountLocation(provider, spec, p)}
			c, ok := byID[spec.ID]
			switch {
			case !ok:
				row.State = "login missing"
			case c.tier == 3:
				row.State = "exhausted until " + c.exhausted.Local().Format("Mon 15:04")
			case c.tier == 2:
				row.State = "over threshold"
			case c.tier == 1:
				row.State = "no usage seen yet"
			default:
				row.State = "ready"
			}
			if ok && c.tier != 1 {
				pct := c.ratio * 100
				row.UsedPercent = &pct
				if !c.resetsAt.IsZero() {
					reset := c.resetsAt
					row.ResetsAt = &reset
				}
			}
			row.Next = ok && len(ranked) > 0 && ranked[0].spec.ID == spec.ID
			rows = append(rows, row)
		}
	}
	return rows
}

func accountLocation(provider string, spec cli.AccountSpec, p *accountPool) string {
	switch {
	case spec.ID == cli.DefaultAccountID && provider == "codex":
		return p.codexSharedHome()
	case spec.ID == cli.DefaultAccountID:
		return "default login"
	case spec.Home != "":
		return spec.Home
	case spec.Executable != "":
		return spec.Executable
	default:
		return spec.ConfigDir
	}
}
