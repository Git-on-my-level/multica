package main

import (
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

func TestRuntimeAccountsReplacePreservesFactsID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")
	t.Setenv(cli.TaskConfigRootEnv, "")
	t.Chdir(home)

	cfg := cli.CLIConfig{AccountPools: &cli.AccountPoolsConfig{
		Accounts: map[string][]cli.AccountSpec{
			"claude": {{ID: "personal", ConfigDir: filepath.Join(home, "old"), FactsID: "claude-1"}},
		},
	}}
	if err := cli.SaveCLIConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.Flags().String("id", "personal", "")
	cmd.Flags().String("config-dir", filepath.Join(home, "new"), "")
	cmd.Flags().String("facts-id", "", "")
	for _, want := range []string{"claude-1", ""} {
		if err := runRuntimeAccountsAdd(cmd, []string{"claude"}); err != nil {
			t.Fatal(err)
		}
		updated, err := cli.LoadCLIConfig()
		if err != nil {
			t.Fatal(err)
		}
		account := updated.AccountPools.Accounts["claude"][0]
		if account.ConfigDir != filepath.Join(home, "new") || account.FactsID != want {
			t.Fatalf("stored account = %+v, want new config dir and facts_id %q", account, want)
		}
		if err := cmd.Flags().Set("facts-id", ""); err != nil {
			t.Fatal(err)
		}
	}
}
