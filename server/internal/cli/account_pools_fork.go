package cli

import (
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Fork overlay: subscription account pools. A daemon can run one provider
// under several subscription accounts on the same machine and pick the account
// with headroom for each task, failing over when an account's plan window is
// spent. Accounts are machine-local: each login is created on this machine
// (`claude-2-login`, `codex-2-login`, ...) and only its location is recorded
// here; credentials are never copied or sent to the server.

// AccountPoolProviders lists the providers whose CLIs report plan windows and
// whose account the daemon knows how to select.
var AccountPoolProviders = []string{"claude", "codex"}

// DefaultAccountID names the account the provider's CLI uses without any
// pool configuration (Claude's default config dir, the daemon's CODEX_HOME).
// It is always a pool member and need not be listed.
const DefaultAccountID = "default"

// DefaultAccountSwitchThreshold is the window usage share (0-1) above which an
// account stops being preferred. Matches agentctl's capacity pools.
const DefaultAccountSwitchThreshold = 0.95

const (
	AccountPolicyDrainBeforeReset = "drain_before_reset"
	AccountPolicySoonestReset     = "soonest_reset"
	DefaultAccountFactsMaxAge     = time.Hour
	DefaultAccountEpsilon         = 0.03
	DefaultAccountExpectedRunCost = 0.01
)

// MaxExtraAccounts caps a provider's extra accounts: a rate-limit report
// carries every other account's snapshot and the server accepts at most this
// many siblings.
const MaxExtraAccounts = 8

// AccountPoolsConfig is the per-profile account pool configuration.
type AccountPoolsConfig struct {
	// SwitchThreshold overrides DefaultAccountSwitchThreshold when in (0, 1].
	SwitchThreshold    float64           `json:"switch_threshold,omitempty"`
	Policy             string            `json:"policy,omitempty"`
	FactsFile          string            `json:"facts_file,omitempty"`
	FactsMaxAgeSeconds int               `json:"facts_max_age_seconds,omitempty"`
	DefaultFactsIDs    map[string]string `json:"default_facts_ids,omitempty"`
	Epsilon            *float64          `json:"epsilon,omitempty"`
	ExpectedRunCost    *float64          `json:"expected_run_cost,omitempty"`
	// Accounts maps a provider ("claude", "codex") to its extra accounts.
	Accounts map[string][]AccountSpec `json:"accounts,omitempty"`
}

// AccountSpec locates one extra subscription account.
//
// Claude: Executable is a launcher that runs Claude Code as that account (for
// example ~/.local/bin/claude-2, which sets CLAUDE_CONFIG_DIR and the account's
// OAuth token itself); ConfigDir sets CLAUDE_CONFIG_DIR. At least one is set.
// Give both for a launcher so the daemon also reads that account's session
// files (usage, resume checks) from the right directory.
//
// Codex: Home is the account's CODEX_HOME (for example ~/.codex-2). Only its
// auth.json is used: the per-task CODEX_HOME keeps the daemon's shared config,
// skills and session store, so switching accounts never forks that state.
type AccountSpec struct {
	ID         string `json:"id"`
	FactsID    string `json:"facts_id,omitempty"`
	Executable string `json:"executable,omitempty"`
	ConfigDir  string `json:"config_dir,omitempty"`
	Home       string `json:"home,omitempty"`
}

var accountIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidateAccountSpec checks one account for provider and returns it with
// trimmed, absolute paths.
func ValidateAccountSpec(provider string, spec AccountSpec) (AccountSpec, error) {
	spec.ID = strings.TrimSpace(spec.ID)
	if !accountIDPattern.MatchString(spec.ID) {
		return spec, fmt.Errorf("account id %q must be 1-64 lowercase letters, digits, '.', '_' or '-'", spec.ID)
	}
	if spec.ID == DefaultAccountID {
		return spec, fmt.Errorf("account id %q is reserved for the provider's default login", DefaultAccountID)
	}
	if spec.FactsID != "" && !accountIDPattern.MatchString(spec.FactsID) {
		return spec, fmt.Errorf("account %s: invalid facts_id %q", spec.ID, spec.FactsID)
	}
	clean := func(field, value string) (string, error) {
		value = strings.TrimSpace(value)
		if value == "" {
			return "", nil
		}
		if !filepath.IsAbs(value) {
			return "", fmt.Errorf("account %s: %s must be an absolute path, got %q", spec.ID, field, value)
		}
		return filepath.Clean(value), nil
	}
	var err error
	if spec.Executable, err = clean("executable", spec.Executable); err != nil {
		return spec, err
	}
	if spec.ConfigDir, err = clean("config_dir", spec.ConfigDir); err != nil {
		return spec, err
	}
	if spec.Home, err = clean("home", spec.Home); err != nil {
		return spec, err
	}
	switch provider {
	case "claude":
		if spec.Home != "" {
			return spec, fmt.Errorf("account %s: claude accounts take executable or config_dir, not home", spec.ID)
		}
		if spec.Executable == "" && spec.ConfigDir == "" {
			return spec, fmt.Errorf("account %s: set executable, config_dir or both", spec.ID)
		}
	case "codex":
		if spec.Executable != "" || spec.ConfigDir != "" {
			return spec, fmt.Errorf("account %s: codex accounts take home only", spec.ID)
		}
		if spec.Home == "" {
			return spec, fmt.Errorf("account %s: home is required", spec.ID)
		}
	default:
		return spec, fmt.Errorf("provider %q has no account pool support (supported: %s)", provider, strings.Join(AccountPoolProviders, ", "))
	}
	return spec, nil
}

// NormalizeAccountPools validates cfg and drops empty providers. A nil or
// empty config returns nil.
func NormalizeAccountPools(cfg *AccountPoolsConfig) (*AccountPoolsConfig, error) {
	if cfg == nil {
		return nil, nil
	}
	if cfg.SwitchThreshold < 0 || cfg.SwitchThreshold > 1 {
		return nil, fmt.Errorf("switch_threshold must be between 0 and 1, got %v", cfg.SwitchThreshold)
	}
	out := *cfg
	out.Accounts = nil
	out.DefaultFactsIDs = nil
	switch cfg.Policy {
	case "", AccountPolicyDrainBeforeReset, AccountPolicySoonestReset:
	default:
		return nil, fmt.Errorf("unknown account pool policy %q", cfg.Policy)
	}
	if out.FactsFile = strings.TrimSpace(cfg.FactsFile); out.FactsFile != "" {
		if !filepath.IsAbs(out.FactsFile) {
			return nil, fmt.Errorf("facts_file must be an absolute path")
		}
		out.FactsFile = filepath.Clean(out.FactsFile)
	}
	if cfg.FactsMaxAgeSeconds < 0 || int64(cfg.FactsMaxAgeSeconds) > math.MaxInt64/int64(time.Second) {
		return nil, fmt.Errorf("facts_max_age_seconds is out of range")
	}
	epsilon, cost := DefaultAccountEpsilon, DefaultAccountExpectedRunCost
	if cfg.Epsilon != nil {
		epsilon = *cfg.Epsilon
		out.Epsilon = &epsilon
	}
	if cfg.ExpectedRunCost != nil {
		cost = *cfg.ExpectedRunCost
		out.ExpectedRunCost = &cost
	}
	if !(epsilon >= 0 && epsilon < 1) {
		return nil, fmt.Errorf("epsilon must be in [0, 1)")
	}
	if !(cost >= 0 && cost <= 1-epsilon) {
		return nil, fmt.Errorf("expected_run_cost must be in [0, 1 - epsilon]")
	}
	for provider, id := range cfg.DefaultFactsIDs {
		if (provider != "claude" && provider != "codex") || !accountIDPattern.MatchString(id) {
			return nil, fmt.Errorf("invalid default_facts_ids entry %q: %q", provider, id)
		}
		if out.DefaultFactsIDs == nil {
			out.DefaultFactsIDs = map[string]string{}
		}
		out.DefaultFactsIDs[provider] = id
	}
	for provider, specs := range cfg.Accounts {
		if len(specs) == 0 {
			continue
		}
		if len(specs) > MaxExtraAccounts {
			return nil, fmt.Errorf("%s: at most %d extra accounts, got %d", provider, MaxExtraAccounts, len(specs))
		}
		seen := map[string]bool{}
		factsSeen := map[string]bool{out.DefaultFactsIDs[provider]: true}
		for _, spec := range specs {
			valid, err := ValidateAccountSpec(provider, spec)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", provider, err)
			}
			if seen[valid.ID] {
				return nil, fmt.Errorf("%s: duplicate account id %q", provider, valid.ID)
			}
			seen[valid.ID] = true
			factsID := valid.FactsID
			if factsID == "" {
				factsID = valid.ID
			}
			if factsSeen[factsID] {
				return nil, fmt.Errorf("%s: duplicate facts id %q", provider, factsID)
			}
			factsSeen[factsID] = true
			if out.Accounts == nil {
				out.Accounts = map[string][]AccountSpec{}
			}
			out.Accounts[provider] = append(out.Accounts[provider], valid)
		}
	}
	if len(out.Accounts) == 0 && out.SwitchThreshold == 0 && out.Policy == "" &&
		out.FactsFile == "" && out.FactsMaxAgeSeconds == 0 && len(out.DefaultFactsIDs) == 0 &&
		out.Epsilon == nil && out.ExpectedRunCost == nil {
		return nil, nil
	}
	return &out, nil
}
