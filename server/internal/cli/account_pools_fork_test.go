package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateAccountSpec(t *testing.T) {
	for _, tc := range []struct {
		name, provider string
		spec           AccountSpec
		wantErr        string
	}{
		{"claude launcher", "claude", AccountSpec{ID: "claude-2", Executable: "/u/bin/claude-2"}, ""},
		{"claude launcher and dir", "claude", AccountSpec{ID: "claude-2", Executable: "/u/bin/claude-2", ConfigDir: "/u/.claude-2"}, ""},
		{"claude needs a location", "claude", AccountSpec{ID: "claude-2"}, "set executable, config_dir or both"},
		{"claude rejects home", "claude", AccountSpec{ID: "claude-2", ConfigDir: "/u/.claude-2", Home: "/u/.codex"}, "not home"},
		{"codex home", "codex", AccountSpec{ID: "codex-2", Home: "/u/.codex-2/"}, ""},
		{"codex needs home", "codex", AccountSpec{ID: "codex-2"}, "home is required"},
		{"codex rejects executable", "codex", AccountSpec{ID: "codex-2", Home: "/u/.codex-2", Executable: "/u/bin/codex-2"}, "home only"},
		{"relative path", "codex", AccountSpec{ID: "codex-2", Home: "~/.codex-2"}, "absolute path"},
		{"reserved id", "codex", AccountSpec{ID: "default", Home: "/u/.codex"}, "reserved"},
		{"bad id", "codex", AccountSpec{ID: "Codex 2", Home: "/u/.codex"}, "lowercase"},
		{"unknown provider", "gemini", AccountSpec{ID: "g-2", Home: "/u/.g"}, "no account pool support"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateAccountSpec(tc.provider, tc.spec)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got.Home != "" && strings.HasSuffix(got.Home, "/") {
					t.Fatalf("home not cleaned: %q", got.Home)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestAccountPoolConfigVersionSkew(t *testing.T) {
	// Existing profiles omit the policy and all facts fields.
	const legacy = `{"account_pools":{"switch_threshold":0.92,"accounts":{
		"claude":[{"id":"personal","config_dir":"/u/.claude-1"}]}}}`
	var current CLIConfig
	if err := json.Unmarshal([]byte(legacy), &current); err != nil {
		t.Fatal(err)
	}
	pools, err := NormalizeAccountPools(current.AccountPools)
	if err != nil {
		t.Fatal(err)
	}
	if pools.Policy != "" || pools.FactsFile != "" || pools.SwitchThreshold != 0.92 {
		t.Fatalf("legacy config changed: %+v", pools)
	}
	pools.Policy = AccountPolicyDrainBeforeReset
	pools.FactsFile = "/u/capacity/facts.json"
	pools.DefaultFactsIDs = map[string]string{"claude": "claude-2"}
	pools.Accounts["claude"][0].FactsID = "claude-1"
	pools, err = NormalizeAccountPools(pools)
	if err != nil {
		t.Fatal(err)
	}
	current.AccountPools = pools
	body, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip CLIConfig
	if err := json.Unmarshal(body, &roundTrip); err != nil {
		t.Fatal(err)
	}
	got := roundTrip.AccountPools
	if got.Policy != pools.Policy || got.FactsFile != pools.FactsFile || got.DefaultFactsIDs["claude"] != "claude-2" ||
		got.Accounts["claude"][0].FactsID != "claude-1" {
		t.Fatalf("current config lost facts fields: %+v", got)
	}
	// Older CLI readers ignore new keys and retain the prior selection inputs.
	var old struct {
		AccountPools struct {
			SwitchThreshold float64 `json:"switch_threshold"`
			Accounts        map[string][]struct {
				ID        string `json:"id"`
				ConfigDir string `json:"config_dir"`
			} `json:"accounts"`
		} `json:"account_pools"`
	}
	if err := json.Unmarshal(body, &old); err != nil {
		t.Fatal(err)
	}
	if old.AccountPools.SwitchThreshold != 0.92 || old.AccountPools.Accounts["claude"][0].ConfigDir != "/u/.claude-1" {
		t.Fatalf("old reader lost account config: %+v", old.AccountPools)
	}
}

func TestNormalizeAccountPoolPolicyOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*AccountPoolsConfig)
	}{
		{"unknown policy", func(c *AccountPoolsConfig) { c.Policy = "typo" }},
		{"relative path", func(c *AccountPoolsConfig) { c.FactsFile = "relative.json" }},
		{"negative age", func(c *AccountPoolsConfig) { c.FactsMaxAgeSeconds = -1 }},
		{"invalid epsilon", func(c *AccountPoolsConfig) { v := 1.0; c.Epsilon = &v }},
		{"cost exceeds headroom", func(c *AccountPoolsConfig) { v := 1.0; c.ExpectedRunCost = &v }},
		{"invalid facts id", func(c *AccountPoolsConfig) { c.Accounts["claude"][0].FactsID = "bad/id" }},
		{"duplicated facts id", func(c *AccountPoolsConfig) { c.DefaultFactsIDs = map[string]string{"claude": "claude-1"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &AccountPoolsConfig{
				Accounts: map[string][]AccountSpec{"claude": {{ID: "claude-1", ConfigDir: "/u/.claude-1"}}},
			}
			tc.edit(cfg)
			if _, err := NormalizeAccountPools(cfg); err == nil {
				t.Fatal("invalid account pool config was accepted")
			}
		})
	}
}

func TestNormalizeAccountPools(t *testing.T) {
	if got, err := NormalizeAccountPools(&AccountPoolsConfig{Accounts: map[string][]AccountSpec{"claude": nil}}); err != nil || got != nil {
		t.Fatalf("empty pools should normalize to nil, got %+v, %v", got, err)
	}
	if _, err := NormalizeAccountPools(&AccountPoolsConfig{SwitchThreshold: 1.5}); err == nil {
		t.Fatal("threshold above 1 must be rejected")
	}
	dup := &AccountPoolsConfig{Accounts: map[string][]AccountSpec{"codex": {{ID: "codex-2", Home: "/a"}, {ID: "codex-2", Home: "/b"}}}}
	if _, err := NormalizeAccountPools(dup); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate ids must be rejected, got %v", err)
	}
}

func TestNormalizeAccountPoolsCapsAccounts(t *testing.T) {
	var specs []AccountSpec
	for i := 0; i <= MaxExtraAccounts; i++ {
		specs = append(specs, AccountSpec{ID: "codex-" + string(rune('a'+i)), Home: "/h/" + string(rune('a'+i))})
	}
	if _, err := NormalizeAccountPools(&AccountPoolsConfig{Accounts: map[string][]AccountSpec{"codex": specs}}); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("over-cap pool must be rejected, got %v", err)
	}
}
