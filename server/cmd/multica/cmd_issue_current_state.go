package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

type issueCurrentState struct {
	State         string                 `json:"state"`
	Blocker       string                 `json:"blocker"`
	NextAction    string                 `json:"next_action"`
	Owner         string                 `json:"owner"`
	Expiry        *string                `json:"expiry"`
	UpdatedAt     string                 `json:"updated_at"`
	UpdatedBy     issueCurrentStateActor `json:"updated_by"`
	IssueRevision int64                  `json:"issue_revision"`
}

type issueCurrentStateActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type issueCurrentStateAPIResponse struct {
	CurrentState  *issueCurrentState `json:"current_state"`
	IssueRevision int64              `json:"issue_revision"`
}

var issueCurrentStateCmd = &cobra.Command{
	Use:   "current-state",
	Short: "Read or update the issue's live execution handoff",
}

var issueCurrentStateGetCmd = &cobra.Command{
	Use:   "get <issue-id>",
	Short: "Read the current-state record without reading comments",
	Args:  exactArgs(1),
	RunE:  runIssueCurrentStateGet,
}

var issueCurrentStateSetCmd = &cobra.Command{
	Use:   "set <issue-id>",
	Short: "Replace the complete current-state record",
	Long: `Replace the complete current-state handoff. Every field is required on
every write so an old blocker or expiry cannot silently carry over. Pass
--blocker "" or --expiry "" to clear those fields. Expiry accepts RFC3339.`,
	Args: exactArgs(1),
	RunE: runIssueCurrentStateSet,
}

func init() {
	issueCurrentStateCmd.AddCommand(issueCurrentStateGetCmd)
	issueCurrentStateCmd.AddCommand(issueCurrentStateSetCmd)
	issueCmd.AddCommand(issueCurrentStateCmd)
	issueCurrentStateGetCmd.Flags().String("output", "json", "Output format: table or json")
	issueCurrentStateSetCmd.Flags().String("state", "", "Current execution state (required)")
	issueCurrentStateSetCmd.Flags().String("blocker", "", "Current blocker, or empty to clear (required)")
	issueCurrentStateSetCmd.Flags().String("next-action", "", "Next concrete action (required)")
	issueCurrentStateSetCmd.Flags().String("owner", "", "Person or agent responsible for the next action (required)")
	issueCurrentStateSetCmd.Flags().String("expiry", "", "RFC3339 expiry, or empty to clear (required)")
	issueCurrentStateSetCmd.Flags().String("output", "json", "Output format: table or json")
	for _, name := range []string{"state", "blocker", "next-action", "owner", "expiry"} {
		_ = issueCurrentStateSetCmd.MarkFlagRequired(name)
	}
}

func runIssueCurrentStateGet(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	issueRef, err := resolveIssueRef(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve issue: %w", err)
	}
	var response issueCurrentStateAPIResponse
	path := "/api/issues/" + url.PathEscape(issueRef.ID) + "/current-state"
	if err := client.GetJSON(ctx, path, &response); err != nil {
		return fmt.Errorf("get issue current state: %w", err)
	}
	output, _ := cmd.Flags().GetString("output")
	if output == "json" {
		return cli.PrintJSON(os.Stdout, response.CurrentState)
	}
	if response.CurrentState == nil {
		fmt.Fprintln(os.Stdout, "No current state recorded.")
		return nil
	}
	printIssueCurrentStateTable(*response.CurrentState)
	return nil
}

func runIssueCurrentStateSet(cmd *cobra.Command, args []string) error {
	state, _ := cmd.Flags().GetString("state")
	blocker, _ := cmd.Flags().GetString("blocker")
	nextAction, _ := cmd.Flags().GetString("next-action")
	owner, _ := cmd.Flags().GetString("owner")
	expiryText, _ := cmd.Flags().GetString("expiry")
	state = strings.TrimSpace(state)
	if state == "" {
		return fmt.Errorf("--state must not be empty")
	}
	var expiry *string
	if expiryText != "" {
		parsed, err := time.Parse(time.RFC3339Nano, expiryText)
		if err != nil {
			return fmt.Errorf("--expiry must be an RFC3339 timestamp or empty")
		}
		canonical := parsed.UTC().Format(time.RFC3339Nano)
		expiry = &canonical
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	issueRef, err := resolveIssueRef(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve issue: %w", err)
	}
	body := map[string]any{
		"state": state, "blocker": blocker, "next_action": nextAction,
		"owner": owner, "expiry": expiry,
	}
	var response issueCurrentStateAPIResponse
	path := "/api/issues/" + url.PathEscape(issueRef.ID) + "/current-state"
	if err := client.PutJSON(ctx, path, body, &response); err != nil {
		return fmt.Errorf("set issue current state: %w", err)
	}
	if response.CurrentState == nil {
		return fmt.Errorf("server returned no issue current state")
	}
	output, _ := cmd.Flags().GetString("output")
	if output == "json" {
		return cli.PrintJSON(os.Stdout, response.CurrentState)
	}
	printIssueCurrentStateTable(*response.CurrentState)
	return nil
}

func printIssueCurrentStateTable(state issueCurrentState) {
	expiry := ""
	if state.Expiry != nil {
		expiry = *state.Expiry
	}
	cli.PrintTable(os.Stdout,
		[]string{"STATE", "BLOCKER", "NEXT ACTION", "OWNER", "EXPIRY", "UPDATED AT", "UPDATED BY", "ISSUE REVISION"},
		[][]string{{state.State, state.Blocker, state.NextAction, state.Owner, expiry, state.UpdatedAt, state.UpdatedBy.Type + " " + state.UpdatedBy.ID, fmt.Sprint(state.IssueRevision)}},
	)
}
