package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

type eventRecord struct {
	ID            string          `json:"id"`
	WorkspaceID   string          `json:"workspace_id"`
	Sequence      int64           `json:"sequence"`
	SourceID      string          `json:"source_id"`
	Type          string          `json:"type"`
	AggregateKind string          `json:"aggregate_kind"`
	AggregateID   string          `json:"aggregate_id"`
	ActorType     *string         `json:"actor_type,omitempty"`
	ActorID       *string         `json:"actor_id,omitempty"`
	OccurredAt    string          `json:"occurred_at"`
	Payload       json.RawMessage `json:"payload"`
}

type eventPage struct {
	Events     []eventRecord `json:"events"`
	NextCursor string        `json:"next_cursor"`
	HasMore    bool          `json:"has_more"`
	Types      []string      `json:"types,omitempty"`
}

var eventCmd = &cobra.Command{
	Use:   "event",
	Short: "Read the durable workspace event stream",
}

var eventListCmd = &cobra.Command{
	Use:   "list",
	Short: "List committed workspace events after a cursor",
	Args:  cobra.NoArgs,
	RunE:  runEventList,
}

var eventWatchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Poll and stream committed workspace events as JSON lines",
	Args:  cobra.NoArgs,
	RunE:  runEventWatch,
}

func init() {
	eventCmd.AddCommand(eventListCmd, eventWatchCmd)
	for _, cmd := range []*cobra.Command{eventListCmd, eventWatchCmd} {
		cmd.Flags().String("cursor", "0", "Return events with sequence greater than this cursor")
		cmd.Flags().Int("limit", 100, "Maximum events per request (1-500)")
		cmd.Flags().StringSlice("type", nil, "Exact event type to include (repeatable; bound into returned cursors)")
	}
	eventWatchCmd.Flags().Duration("interval", time.Second, "Polling interval when no events are available")
}

func eventCommandClient(cmd *cobra.Command) (*cli.APIClient, error) {
	workspaceID, err := requireWorkspaceID(cmd)
	if err != nil {
		return nil, err
	}
	token := resolveToken(cmd)
	if token == "" {
		return nil, fmt.Errorf("not authenticated: run 'multica login' first")
	}
	return cli.NewAPIClient(resolveServerURL(cmd), workspaceID, token), nil
}

func fetchEventPage(ctx context.Context, cmd *cobra.Command, cursor string) (eventPage, error) {
	client, err := eventCommandClient(cmd)
	if err != nil {
		return eventPage{}, err
	}
	limit, _ := cmd.Flags().GetInt("limit")
	values := url.Values{
		"cursor": {cursor},
		"limit":  {strconv.Itoa(limit)},
	}
	types, _ := cmd.Flags().GetStringSlice("type")
	for _, eventType := range types {
		values.Add("type", eventType)
	}
	var page eventPage
	if err := client.GetJSON(ctx, "/api/events?"+values.Encode(), &page); err != nil {
		return eventPage{}, fmt.Errorf("list workspace events: %w", err)
	}
	if page.Events == nil {
		page.Events = []eventRecord{}
	}
	return page, nil
}

func runEventList(cmd *cobra.Command, _ []string) error {
	cursor, _ := cmd.Flags().GetString("cursor")
	ctx, cancel := cli.APIContext(cmd.Context())
	defer cancel()
	page, err := fetchEventPage(ctx, cmd, cursor)
	if err != nil {
		return err
	}
	return cli.PrintJSON(cmd.OutOrStdout(), page)
}

func runEventWatch(cmd *cobra.Command, _ []string) error {
	cursor, _ := cmd.Flags().GetString("cursor")
	interval, _ := cmd.Flags().GetDuration("interval")
	if interval < 100*time.Millisecond {
		return fmt.Errorf("--interval must be at least 100ms")
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()
	encoder := json.NewEncoder(cmd.OutOrStdout())

	for {
		requestCtx, cancel := cli.APIContext(ctx)
		page, err := fetchEventPage(requestCtx, cmd, cursor)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, event := range page.Events {
			if err := encoder.Encode(event); err != nil {
				return err
			}
		}
		cursor = page.NextCursor
		if page.HasMore {
			continue
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}
