package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	defaultBatchSize = 1000
	maxBatchSize     = 10000
	defaultMaxBatch  = 100
	maxBatchCount    = 100000
)

type pruneConfig struct {
	Retention   time.Duration
	BatchSize   int
	MaxBatches  int
	WorkspaceID string
	Apply       bool
}

func parsePruneConfig(args []string, stderr io.Writer) (pruneConfig, error) {
	fs := flag.NewFlagSet("prune_workspace_events", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg pruneConfig
	fs.DurationVar(&cfg.Retention, "retention", 0, "required event retention duration, for example 720h")
	fs.IntVar(&cfg.BatchSize, "batch-size", defaultBatchSize, "maximum rows deleted per batch")
	fs.IntVar(&cfg.MaxBatches, "max-batches", defaultMaxBatch, "maximum deletion batches per invocation")
	fs.StringVar(&cfg.WorkspaceID, "workspace-id", "", "optional workspace UUID; omitted processes every event workspace")
	fs.BoolVar(&cfg.Apply, "apply", false, "perform deletion; omitted is a read-only preview")
	if err := fs.Parse(args); err != nil {
		return pruneConfig{}, err
	}
	if fs.NArg() != 0 {
		return pruneConfig{}, errors.New("unexpected positional arguments")
	}
	if cfg.Retention <= 0 {
		return pruneConfig{}, errors.New("--retention must be explicitly set to a positive duration")
	}
	if cfg.BatchSize < 1 || cfg.BatchSize > maxBatchSize {
		return pruneConfig{}, fmt.Errorf("--batch-size must be between 1 and %d", maxBatchSize)
	}
	if cfg.MaxBatches < 1 || cfg.MaxBatches > maxBatchCount {
		return pruneConfig{}, fmt.Errorf("--max-batches must be between 1 and %d", maxBatchCount)
	}
	if cfg.WorkspaceID != "" {
		if _, err := util.ParseUUID(cfg.WorkspaceID); err != nil {
			return pruneConfig{}, errors.New("--workspace-id must be a UUID")
		}
	}
	return cfg, nil
}

type workspacePruneSummary struct {
	WorkspaceID          string `json:"workspace_id"`
	Candidates           int64  `json:"candidates,omitempty"`
	Deleted              int64  `json:"deleted,omitempty"`
	FirstDeletedSequence int64  `json:"first_deleted_sequence,omitempty"`
	LastDeletedSequence  int64  `json:"last_deleted_sequence,omitempty"`
}

type pruneSummary struct {
	Applied    bool                    `json:"applied"`
	Cutoff     string                  `json:"cutoff"`
	Retention  string                  `json:"retention"`
	BatchSize  int                     `json:"batch_size"`
	MaxBatches int                     `json:"max_batches"`
	Batches    int                     `json:"batches"`
	Deleted    int64                   `json:"deleted"`
	HasMore    bool                    `json:"has_more"`
	Workspaces []workspacePruneSummary `json:"workspaces"`
}

func workspaceIDs(ctx context.Context, q *db.Queries, only string) ([]pgtype.UUID, error) {
	if only != "" {
		id, err := util.ParseUUID(only)
		if err != nil {
			return nil, err
		}
		return []pgtype.UUID{id}, nil
	}
	return q.ListWorkspaceEventCursorWorkspaces(ctx)
}

func runPrune(ctx context.Context, q *db.Queries, cfg pruneConfig, now time.Time) (pruneSummary, error) {
	cutoffTime := now.UTC().Add(-cfg.Retention)
	cutoff := pgtype.Timestamptz{Time: cutoffTime, Valid: true}
	ids, err := workspaceIDs(ctx, q, cfg.WorkspaceID)
	if err != nil {
		return pruneSummary{}, fmt.Errorf("list event workspaces: %w", err)
	}
	summary := pruneSummary{
		Applied: cfg.Apply, Cutoff: cutoffTime.Format(time.RFC3339Nano), Retention: cfg.Retention.String(),
		BatchSize: cfg.BatchSize, MaxBatches: cfg.MaxBatches,
		Workspaces: make([]workspacePruneSummary, len(ids)),
	}
	for i, id := range ids {
		summary.Workspaces[i].WorkspaceID = util.UUIDToString(id)
	}
	if !cfg.Apply {
		for i, id := range ids {
			count, err := q.CountPrunableWorkspaceEvents(ctx, db.CountPrunableWorkspaceEventsParams{WorkspaceID: id, Cutoff: cutoff})
			if err != nil {
				return pruneSummary{}, fmt.Errorf("preview workspace %s: %w", util.UUIDToString(id), err)
			}
			summary.Workspaces[i].Candidates = count
		}
		return summary, nil
	}

	active := make([]int, len(ids))
	for i := range ids {
		active[i] = i
	}
	for len(active) > 0 && summary.Batches < cfg.MaxBatches {
		next := make([]int, 0, len(active))
		for offset, index := range active {
			if summary.Batches >= cfg.MaxBatches {
				next = append(next, active[offset:]...)
				break
			}
			row, err := q.PruneWorkspaceEventsBatch(ctx, db.PruneWorkspaceEventsBatchParams{
				WorkspaceID: ids[index], Cutoff: cutoff, BatchSize: int32(cfg.BatchSize),
			})
			if err != nil {
				return pruneSummary{}, fmt.Errorf("prune workspace %s: %w", util.UUIDToString(ids[index]), err)
			}
			summary.Batches++
			workspace := &summary.Workspaces[index]
			workspace.Deleted += row.DeletedCount
			if row.DeletedCount > 0 {
				if workspace.FirstDeletedSequence == 0 {
					workspace.FirstDeletedSequence = row.FirstDeletedSequence
				}
				workspace.LastDeletedSequence = row.LastDeletedSequence
				summary.Deleted += row.DeletedCount
			}
			if row.DeletedCount == int64(cfg.BatchSize) {
				next = append(next, index)
			}
		}
		active = next
	}
	summary.HasMore = len(active) > 0
	return summary, nil
}

func main() {
	cfg, err := parsePruneConfig(os.Args[1:], os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: connect:", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error: ping:", err)
		os.Exit(1)
	}
	summary, err := runPrune(ctx, db.New(pool), cfg, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(summary); err != nil {
		fmt.Fprintln(os.Stderr, "error: encode summary:", err)
		os.Exit(1)
	}
}
