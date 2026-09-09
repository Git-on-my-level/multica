package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestRunMigrationsAcceptsValidUnrecordedWorkspaceEventCursorIndex covers the
// third failure mode of a concurrent index build, the one neither
// `concurrentIndexCleanups` nor the MUL-6288 totality test can catch.
//
// `CREATE INDEX CONCURRENTLY` cannot run inside a transaction, so runMigrations
// applies the migration file and then records the version in
// schema_migrations as two independent statements on an unwrapped connection.
// If the runner dies between them — SIGTERM, connection reset, container
// eviction — the index is left VALID and the version is left unrecorded. The
// registered pre-migration hook is correct to leave a VALID index alone, so the
// only thing standing between that state and a permanently wedged migrator is
// whether the migration's own SQL tolerates the index already existing.
//
// Migration 409 shipped as a bare `CREATE UNIQUE INDEX CONCURRENTLY` and hit
// exactly this: every retry failed with 42P07 "relation ... already exists" and
// the instance could not advance past 408. The assertion here is that a rerun
// against a VALID, unrecorded index succeeds, records the version, and leaves
// the index in place.
func TestRunMigrationsAcceptsValidUnrecordedWorkspaceEventCursorIndex(t *testing.T) {
	const (
		version   = "409_workspace_event_cursor_unique"
		indexName = "workspace_event_cursor_workspace_uidx"
	)

	suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), rand.Uint32())
	schema := "migrate_mig409_" + suffix

	admin := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	schemaIdent := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schemaIdent); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA IF EXISTS "+schemaIdent+" CASCADE"); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})

	// search_path-scoped so the real migration file's unqualified
	// `workspace_event_cursor` and the production hook's unqualified
	// to_regclass lookup both resolve inside the throwaway schema.
	pool := openTestPoolWithSearchPath(t, schema)

	if _, err := pool.Exec(ctx, `CREATE TABLE workspace_event_cursor (
		workspace_id UUID NOT NULL,
		sequence BIGINT NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatalf("create workspace_event_cursor: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO workspace_event_cursor (workspace_id) VALUES (gen_random_uuid())"); err != nil {
		t.Fatalf("seed workspace_event_cursor: %v", err)
	}

	hook := preMigrationHooks[version]
	if hook == nil {
		t.Fatalf("production hook is not registered for %s", version)
	}
	opts := runOptions{
		Direction:             "up",
		Files:                 []string{filepath.Join("..", "..", "migrations", version+".up.sql")},
		SchemaMigrationsTable: schema + ".schema_migrations",
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		Hooks:                 map[string]preMigrationHook{version: hook},
	}

	if err := runMigrations(ctx, pool, opts); err != nil {
		t.Fatalf("first run of %s: %v", version, err)
	}
	assertIndexValidity(t, pool, schema, indexName, true)

	// Reproduce the production state on multica-01: the index build committed,
	// the INSERT into schema_migrations did not.
	migrationsTable := pgx.Identifier{schema, "schema_migrations"}.Sanitize()
	tag, err := pool.Exec(ctx, "DELETE FROM "+migrationsTable+" WHERE version = $1", version)
	if err != nil {
		t.Fatalf("drop recorded version: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("expected %s to have been recorded once, deleted %d rows", version, tag.RowsAffected())
	}
	assertIndexValidity(t, pool, schema, indexName, true)

	// The retry must not wedge on 42P07. The hook leaves the VALID index alone,
	// so IF NOT EXISTS in the migration is what makes this converge.
	if err := runMigrations(ctx, pool, opts); err != nil {
		t.Fatalf("retry over VALID unrecorded index: %v", err)
	}

	var recorded bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM "+migrationsTable+" WHERE version = $1)", version).Scan(&recorded); err != nil {
		t.Fatalf("read recorded version: %v", err)
	}
	if !recorded {
		t.Fatalf("%s still not recorded after a successful retry", version)
	}
	assertIndexValidity(t, pool, schema, indexName, true)

	// The surviving index must still be the unique one the feed relies on, not
	// some unrelated relation that happened to take the name.
	var definition string
	if err := pool.QueryRow(ctx, "SELECT pg_get_indexdef($1::regclass)",
		pgx.Identifier{schema, indexName}.Sanitize()).Scan(&definition); err != nil {
		t.Fatalf("read index definition: %v", err)
	}
	for _, want := range []string{"UNIQUE INDEX", "workspace_event_cursor", "workspace_id"} {
		if !strings.Contains(definition, want) {
			t.Fatalf("index definition after retry is missing %q: %s", want, definition)
		}
	}
}
