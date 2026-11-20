package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/daemon"
)

// Fork overlay: `multica runtime accounts` manages this machine's subscription
// account pools (cli.AccountPoolsConfig). The daemon reads them at start.

var runtimeAccountsCmd = &cobra.Command{
	Use:   "accounts",
	Short: "Manage this machine's subscription account pools (local only)",
	Long: `Run Claude Code or Codex under several subscription accounts on this machine.

The daemon runs each task under the account with the most plan headroom and,
when an account's session or weekly window runs out mid-task, re-runs the task
on the next account. The provider's default login is always in the pool as
"default"; add the extra logins you created on this machine.

  multica runtime accounts add claude --id claude-2 --executable ~/.local/bin/claude-2 --config-dir ~/.claude-2
  multica runtime accounts add codex --id codex-2 --home ~/.codex-2

Restart the daemon after a change.`,
}

var runtimeAccountsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured accounts and their last known plan usage",
	Args:  exactArgs(0),
	RunE:  runRuntimeAccountsList,
}

var runtimeAccountsAddCmd = &cobra.Command{
	Use:   "add <claude|codex>",
	Short: "Add or replace an account in a provider's pool",
	Args:  exactArgs(1),
	RunE:  runRuntimeAccountsAdd,
}

var runtimeAccountsRemoveCmd = &cobra.Command{
	Use:   "remove <claude|codex> <account-id>",
	Short: "Remove an account from a provider's pool",
	Args:  exactArgs(2),
	RunE:  runRuntimeAccountsRemove,
}

var runtimeAccountsThresholdCmd = &cobra.Command{
	Use:   "threshold <0-1>",
	Short: "Set the usage share above which an account stops being preferred (default 0.95)",
	Args:  exactArgs(1),
	RunE:  runRuntimeAccountsThreshold,
}

func init() {
	runtimeCmd.AddCommand(runtimeAccountsCmd)
	runtimeAccountsCmd.AddCommand(runtimeAccountsListCmd, runtimeAccountsAddCmd, runtimeAccountsRemoveCmd, runtimeAccountsThresholdCmd)
	runtimeAccountsListCmd.Flags().String("output", "table", "Output format: table or json")
	runtimeAccountsAddCmd.Flags().String("id", "", "Account id, for example claude-2 (required)")
	runtimeAccountsAddCmd.Flags().String("executable", "", "Claude: launcher that runs Claude Code as this account")
	runtimeAccountsAddCmd.Flags().String("config-dir", "", "Claude: the account's CLAUDE_CONFIG_DIR")
	runtimeAccountsAddCmd.Flags().String("home", "", "Codex: the account's CODEX_HOME")
}

func expandAccountPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func loadAccountPools(cmd *cobra.Command) (cli.CLIConfig, string, *cli.AccountPoolsConfig, error) {
	profile := resolveProfile(cmd)
	cfg, err := cli.LoadCLIConfigForProfile(profile)
	if err != nil {
		return cfg, profile, nil, fmt.Errorf("load CLI config: %w", err)
	}
	pools := cfg.AccountPools
	if pools == nil {
		pools = &cli.AccountPoolsConfig{}
	}
	return cfg, profile, pools, nil
}

func saveAccountPools(cfg cli.CLIConfig, profile string, pools *cli.AccountPoolsConfig) error {
	normalized, err := cli.NormalizeAccountPools(pools)
	if err != nil {
		return err
	}
	cfg.AccountPools = normalized
	if err := cli.SaveCLIConfigForProfile(cfg, profile); err != nil {
		return fmt.Errorf("save CLI config: %w", err)
	}
	fmt.Println("Restart the daemon for the change to take effect.")
	return nil
}

func runRuntimeAccountsAdd(cmd *cobra.Command, args []string) error {
	if err := requireHumanLocalCommand("runtime accounts add"); err != nil {
		return err
	}
	provider := args[0]
	id, _ := cmd.Flags().GetString("id")
	executable, _ := cmd.Flags().GetString("executable")
	configDir, _ := cmd.Flags().GetString("config-dir")
	home, _ := cmd.Flags().GetString("home")
	spec, err := cli.ValidateAccountSpec(provider, cli.AccountSpec{
		ID:         id,
		Executable: expandAccountPath(executable),
		ConfigDir:  expandAccountPath(configDir),
		Home:       expandAccountPath(home),
	})
	if err != nil {
		return err
	}
	cfg, profile, pools, err := loadAccountPools(cmd)
	if err != nil {
		return err
	}
	if pools.Accounts == nil {
		pools.Accounts = map[string][]cli.AccountSpec{}
	}
	replaced := false
	for i, existing := range pools.Accounts[provider] {
		if existing.ID == spec.ID {
			pools.Accounts[provider][i] = spec
			replaced = true
		}
	}
	if !replaced {
		pools.Accounts[provider] = append(pools.Accounts[provider], spec)
	}
	fmt.Printf("Added %s account %s.\n", provider, spec.ID)
	return saveAccountPools(cfg, profile, pools)
}

func runRuntimeAccountsRemove(cmd *cobra.Command, args []string) error {
	if err := requireHumanLocalCommand("runtime accounts remove"); err != nil {
		return err
	}
	provider, id := args[0], args[1]
	cfg, profile, pools, err := loadAccountPools(cmd)
	if err != nil {
		return err
	}
	kept := pools.Accounts[provider][:0]
	found := false
	for _, spec := range pools.Accounts[provider] {
		if spec.ID == id {
			found = true
			continue
		}
		kept = append(kept, spec)
	}
	if !found {
		return fmt.Errorf("%s has no account %q", provider, id)
	}
	pools.Accounts[provider] = kept
	fmt.Printf("Removed %s account %s.\n", provider, id)
	return saveAccountPools(cfg, profile, pools)
}

func runRuntimeAccountsThreshold(cmd *cobra.Command, args []string) error {
	if err := requireHumanLocalCommand("runtime accounts threshold"); err != nil {
		return err
	}
	var v float64
	if _, err := fmt.Sscanf(args[0], "%g", &v); err != nil || v <= 0 || v > 1 {
		return fmt.Errorf("threshold must be a number in (0, 1], got %q", args[0])
	}
	cfg, profile, pools, err := loadAccountPools(cmd)
	if err != nil {
		return err
	}
	pools.SwitchThreshold = v
	fmt.Printf("Switch threshold set to %g.\n", v)
	return saveAccountPools(cfg, profile, pools)
}

func runRuntimeAccountsList(cmd *cobra.Command, _ []string) error {
	_, profile, pools, err := loadAccountPools(cmd)
	if err != nil {
		return err
	}
	rows := daemon.AccountPoolStatus(pools, profile)
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Println("No account pools configured. Add one with `multica runtime accounts add`.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tACCOUNT\tLOCATION\tUSED\tRESETS\tSTATE")
	for _, r := range rows {
		used := "-"
		if r.UsedPercent != nil {
			used = fmt.Sprintf("%.0f%%", *r.UsedPercent)
		}
		resets := "-"
		if r.ResetsAt != nil {
			resets = r.ResetsAt.Local().Format("Mon 15:04")
		}
		state := r.State
		if r.Next {
			state += " (next)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Provider, r.Account, r.Location, used, resets, state)
	}
	return w.Flush()
}
