package cli

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
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

// AccountPoolsConfig is the per-profile account pool configuration.
type AccountPoolsConfig struct {
	// SwitchThreshold overrides DefaultAccountSwitchThreshold when in (0, 1].
	SwitchThreshold float64 `json:"switch_threshold,omitempty"`
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
	out := &AccountPoolsConfig{SwitchThreshold: cfg.SwitchThreshold}
	for provider, specs := range cfg.Accounts {
		if len(specs) == 0 {
			continue
		}
		seen := map[string]bool{}
		for _, spec := range specs {
			valid, err := ValidateAccountSpec(provider, spec)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", provider, err)
			}
			if seen[valid.ID] {
				return nil, fmt.Errorf("%s: duplicate account id %q", provider, valid.ID)
			}
			seen[valid.ID] = true
			if out.Accounts == nil {
				out.Accounts = map[string][]AccountSpec{}
			}
			out.Accounts[provider] = append(out.Accounts[provider], valid)
		}
	}
	if len(out.Accounts) == 0 && out.SwitchThreshold == 0 {
		return nil, nil
	}
	return out, nil
}
