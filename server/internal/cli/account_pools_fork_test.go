package cli

import (
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
