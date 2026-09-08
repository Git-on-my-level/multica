package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/migrations"
)

// renamedMigrationAlias is one verified fork→current ledger rewrite.
// Old and new .up.sql files are byte-identical at the recorded SHA256;
// the runner never infers aliases from numeric offsets or suffixes.
type renamedMigrationAlias struct {
	OldVersion string
	NewVersion string
	SHA256     string
}

// fleetRenumberAliases is the exact set of schema_migrations rows that
// merge af021d45b renamed after they had already been applied on the
// fork. 408 and 412 were rewritten with IF NOT EXISTS and are not
// aliases. Earlier renames already recorded their current names.
//
// Hashes are compiled in from the independently verified byte-identical
// pair at old commit 5e0a94eb8cddef7446ef750fd7050a982563c1c6 versus
// current files. A later SQL edit must not keep the alias.
var fleetRenumberAliases = []renamedMigrationAlias{
	{
		OldVersion: "247_workspace_event_cursor_unique",
		NewVersion: "409_workspace_event_cursor_unique",
		SHA256:     "a6ea4600216659dfcaa2d8d701b4e69f8c396fdfcd11d537d8de16cd2e3e4a9d",
	},
	{
		OldVersion: "248_workspace_event_source_unique",
		NewVersion: "410_workspace_event_source_unique",
		SHA256:     "f5fa8b2ba96ebea059c3dc007cd170bc23e99c22bf73c5f97eaa3e9d43427623",
	},
	{
		OldVersion: "249_workspace_event_sequence_unique",
		NewVersion: "411_workspace_event_sequence_unique",
		SHA256:     "584e47e30ae4b07b6dc1b0e26c098b0092bccc6cdbc1e83ce9247446c2ffded0",
	},
	{
		OldVersion: "251_issue_create_idempotency_unique",
		NewVersion: "413_issue_create_idempotency_unique",
		SHA256:     "8f5d5a62c72f45ad32832bfd6404cfb711b1dbdf12fe7794a4245bb8914cc295",
	},
	{
		OldVersion: "252_workspace_event_id_unique",
		NewVersion: "414_workspace_event_id_unique",
		SHA256:     "532c618369df8c090098c91f3a404d67347c9ec7a8a365512c710ebcf98d7c7f",
	},
	{
		OldVersion: "253_workspace_event_capture",
		NewVersion: "415_workspace_event_capture",
		SHA256:     "dbe9716f42581c61bc6d89de5bf39e024a21a84a4d68163263ac15f81bc5c06c",
	},
	{
		OldVersion: "254_workspace_event_retention_index",
		NewVersion: "416_workspace_event_retention_index",
		SHA256:     "82a5c7de4eab067d7c853bdcc3257ecd2d6cce2b56da63afb92833aec335b1b5",
	},
}

type pendingRenumberAlias struct {
	alias renamedMigrationAlias
	file  string
}

// normalizeRenumberedMigrationLedger rewrites the seven verified fork
// version names to the current names while preserving applied_at. It
// runs only for up, under the caller's already-held advisory lock and
// pinned conn, before the per-file EXISTS loop. Fresh databases with
// no old rows are a no-op. Unknown ledger rows are left untouched.
//
// The old name is replaced rather than recorded alongside the new name
// so a later down/up cannot skip reconstruction. Old+new for one alias,
// a SHA256 mismatch, or a later UPDATE failure rolls back every alias
// rewrite in this invocation.
func normalizeRenumberedMigrationLedger(ctx context.Context, conn *pgxpool.Conn, opts runOptions, tableIdent string) error {
	if opts.Direction != "up" {
		return nil
	}

	filesByVersion := make(map[string]string, len(opts.Files))
	for _, file := range opts.Files {
		filesByVersion[migrations.ExtractVersion(file)] = file
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin renamed-migration ledger normalization: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	pending := make([]pendingRenumberAlias, 0, len(fleetRenumberAliases))
	for _, alias := range fleetRenumberAliases {
		file, inInvocation := filesByVersion[alias.NewVersion]
		if !inInvocation {
			continue
		}

		oldExists, newExists, err := lockRenumberAliasRows(ctx, tx, tableIdent, alias.OldVersion, alias.NewVersion)
		if err != nil {
			return err
		}
		if oldExists && newExists {
			return fmt.Errorf("schema_migrations has both %q and %q; refusing to guess which history is authoritative", alias.OldVersion, alias.NewVersion)
		}
		if !oldExists {
			continue
		}
		pending = append(pending, pendingRenumberAlias{alias: alias, file: file})
	}

	for _, item := range pending {
		if err := verifyRenumberAliasHash(item); err != nil {
			return err
		}
	}

	if len(pending) == 0 {
		return nil
	}

	for _, item := range pending {
		tag, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET version = $1 WHERE version = $2`, tableIdent), item.alias.NewVersion, item.alias.OldVersion)
		if err != nil {
			return fmt.Errorf("rename schema_migrations %q -> %q: %w", item.alias.OldVersion, item.alias.NewVersion, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("rename schema_migrations %q -> %q: expected 1 row, got %d", item.alias.OldVersion, item.alias.NewVersion, tag.RowsAffected())
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit renamed-migration ledger normalization: %w", err)
	}
	for _, item := range pending {
		slog.Info("normalized renamed migration ledger entry",
			"old_version", item.alias.OldVersion,
			"new_version", item.alias.NewVersion)
	}
	return nil
}

func verifyRenumberAliasHash(item pendingRenumberAlias) error {
	body, err := os.ReadFile(item.file)
	if err != nil {
		return fmt.Errorf("read current migration %s before renaming %q -> %q: %w", item.file, item.alias.OldVersion, item.alias.NewVersion, err)
	}
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if got != item.alias.SHA256 {
		return fmt.Errorf("current %q sha256 %s does not match expected %s; refusing to rename ledger %q -> %q", item.alias.NewVersion, got, item.alias.SHA256, item.alias.OldVersion, item.alias.NewVersion)
	}
	return nil
}

func lockRenumberAliasRows(ctx context.Context, tx pgx.Tx, tableIdent, oldVersion, newVersion string) (oldExists, newExists bool, err error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT version FROM %s WHERE version IN ($1, $2) FOR UPDATE`, tableIdent), oldVersion, newVersion)
	if err != nil {
		return false, false, fmt.Errorf("lock renamed-migration ledger rows for %q -> %q: %w", oldVersion, newVersion, err)
	}
	defer rows.Close()

	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return false, false, fmt.Errorf("scan renamed-migration ledger row for %q -> %q: %w", oldVersion, newVersion, err)
		}
		switch version {
		case oldVersion:
			oldExists = true
		case newVersion:
			newExists = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, fmt.Errorf("read renamed-migration ledger rows for %q -> %q: %w", oldVersion, newVersion, err)
	}
	return oldExists, newExists, nil
}
