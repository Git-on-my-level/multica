package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	unrelatedCurrent247 = "247_comment_parent_index"
	unrelatedCurrent254 = "254_runtime_profile_add_reasonix"
	cursorUniqueIndex   = "workspace_event_cursor_workspace_uidx"
	appendEventFunc     = "append_workspace_event"
)

func TestRenumberAliasesExactVerifiedSet(t *testing.T) {
	if len(fleetRenumberAliases) != 7 {
		t.Fatalf("fleetRenumberAliases len = %d, want 7", len(fleetRenumberAliases))
	}
	seenOld := map[string]bool{}
	seenNew := map[string]bool{}
	for i, alias := range fleetRenumberAliases {
		if alias.OldVersion == "" || alias.NewVersion == "" || len(alias.SHA256) != 64 {
			t.Fatalf("alias %d is incomplete: %+v", i, alias)
		}
		if alias.OldVersion == alias.NewVersion {
			t.Fatalf("alias %d old and new are the same: %s", i, alias.OldVersion)
		}
		if seenOld[alias.OldVersion] || seenNew[alias.NewVersion] {
			t.Fatalf("duplicate alias at %d: %+v", i, alias)
		}
		seenOld[alias.OldVersion] = true
		seenNew[alias.NewVersion] = true
		if strings.Contains(alias.OldVersion, "408_") || strings.Contains(alias.NewVersion, "408_") ||
			strings.Contains(alias.OldVersion, "412_") || strings.Contains(alias.NewVersion, "412_") {
			t.Fatalf("408/412 are not exact aliases: %+v", alias)
		}
	}
}

func TestRenumberAliasCompiledHashesMatchCurrentUpFiles(t *testing.T) {
	for _, alias := range fleetRenumberAliases {
		path := realMigrationFiles(t, []string{alias.NewVersion}, "up")[0]
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		sum := sha256.Sum256(body)
		got := hex.EncodeToString(sum[:])
		if got != alias.SHA256 {
			t.Errorf("%s sha256 %s, compiled-in %s", alias.NewVersion, got, alias.SHA256)
		}
	}
}

func TestRenumberAliasesNormalizeKnownSequence(t *testing.T) {
	f := newRenumberFixture(t)
	stamps, cursorOID := prepareAppliedOldAliases(t, f)

	if err := runMigrations(f.ctx, f.pool, f.upOpts(t, aliasUpFiles(t))); err != nil {
		t.Fatalf("normalize known renamed sequence: %v", err)
	}

	assertAliasLedgerNormalized(t, f, stamps)
	assertUnrelatedLegacyPreserved(t, f)
	if got := indexOID(t, f, cursorUniqueIndex); got != cursorOID {
		t.Fatalf("cursor unique index oid = %d, want preserved %d", got, cursorOID)
	}
	assertFunctionExists(t, f, appendEventFunc, true)
	assertIndexReadyAndValid(t, f.pool, f.schema, cursorUniqueIndex, true)
}

func TestRenumberAliasesMissingLegacyRowsApplyNormalDDL(t *testing.T) {
	f := newRenumberFixture(t)

	if err := runMigrations(f.ctx, f.pool, f.upOpts(t, aliasUpFiles(t))); err != nil {
		t.Fatalf("apply current alias files with no legacy rows: %v", err)
	}

	for _, alias := range fleetRenumberAliases {
		assertMigrationVersionRecorded(t, f.ctx, f.pool, f.schema, alias.OldVersion, false)
		assertMigrationVersionRecorded(t, f.ctx, f.pool, f.schema, alias.NewVersion, true)
	}
	assertIndexReadyAndValid(t, f.pool, f.schema, cursorUniqueIndex, true)
	assertFunctionExists(t, f, appendEventFunc, true)
}

func TestRenumberAliasesWrongFileHashLeavesEveryAliasUntouched(t *testing.T) {
	f := newRenumberFixture(t)
	stamps := seedOldAliasesAndUnrelated(t, f)
	files := writeAliasUpFiles(t, t.TempDir(), fleetRenumberAliases[len(fleetRenumberAliases)-1].NewVersion)

	err := runMigrations(f.ctx, f.pool, f.upOpts(t, files))
	if err == nil {
		t.Fatal("want sha256 mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "sha256") || !strings.Contains(err.Error(), "refusing to rename") {
		t.Fatalf("error %q does not diagnose the sha256 mismatch", err)
	}

	assertOldAliasRowsUntouched(t, f, stamps)
	assertNewAliasRows(t, f, nil)
	assertUnrelatedLegacyPreserved(t, f)
	assertIndexExists(t, f.pool, f.schema, cursorUniqueIndex, false)
	assertFunctionExists(t, f, appendEventFunc, false)
}

func TestRenumberAliasesOldAndNewConflictLeavesEveryAliasUntouched(t *testing.T) {
	f := newRenumberFixture(t)
	stamps := seedOldAliasesAndUnrelated(t, f)
	conflict := fleetRenumberAliases[len(fleetRenumberAliases)-1]
	conflictAt := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	insertMigrationRow(t, f, conflict.NewVersion, conflictAt)

	err := runMigrations(f.ctx, f.pool, f.upOpts(t, aliasUpFiles(t)))
	if err == nil {
		t.Fatal("want old+new conflict error, got nil")
	}
	if !strings.Contains(err.Error(), conflict.OldVersion) ||
		!strings.Contains(err.Error(), conflict.NewVersion) ||
		!strings.Contains(err.Error(), "refusing to guess") {
		t.Fatalf("error %q does not diagnose both ledger versions", err)
	}

	assertOldAliasRowsUntouched(t, f, stamps)
	assertNewAliasRows(t, f, map[string]bool{conflict.NewVersion: true})
	assertUnrelatedLegacyPreserved(t, f)
	if got := migrationAppliedAt(t, f, conflict.NewVersion); !got.Equal(conflictAt) {
		t.Fatalf("conflicting new row applied_at = %s, want %s", got, conflictAt)
	}
	assertIndexExists(t, f.pool, f.schema, cursorUniqueIndex, false)
	assertFunctionExists(t, f, appendEventFunc, false)
}

func TestRenumberAliasesPreserveUnrelatedLegacyRows(t *testing.T) {
	f := newRenumberFixture(t)
	stamps, _ := prepareAppliedOldAliases(t, f)

	if err := runMigrations(f.ctx, f.pool, f.upOpts(t, aliasUpFiles(t))); err != nil {
		t.Fatalf("normalize with unrelated legacy rows: %v", err)
	}

	assertAliasLedgerNormalized(t, f, stamps)
	assertUnrelatedLegacyPreserved(t, f)
}

func TestRenumberAliasesRepeatInvocationIdempotent(t *testing.T) {
	f := newRenumberFixture(t)
	stamps, cursorOID := prepareAppliedOldAliases(t, f)
	opts := f.upOpts(t, aliasUpFiles(t))

	if err := runMigrations(f.ctx, f.pool, opts); err != nil {
		t.Fatalf("first normalize: %v", err)
	}
	if err := runMigrations(f.ctx, f.pool, opts); err != nil {
		t.Fatalf("repeat normalize: %v", err)
	}

	assertAliasLedgerNormalized(t, f, stamps)
	assertUnrelatedLegacyPreserved(t, f)
	if got := indexOID(t, f, cursorUniqueIndex); got != cursorOID {
		t.Fatalf("repeat invocation changed cursor unique index oid %d → %d", cursorOID, got)
	}
	assertFunctionExists(t, f, appendEventFunc, true)
}

func TestRenumberAliasesDownUpAfterNormalizationRebuilds(t *testing.T) {
	f := newRenumberFixture(t)
	stamps, cursorOID := prepareAppliedOldAliases(t, f)

	if err := runMigrations(f.ctx, f.pool, f.upOpts(t, aliasUpFiles(t))); err != nil {
		t.Fatalf("normalize before down/up: %v", err)
	}
	assertAliasLedgerNormalized(t, f, stamps)
	if got := indexOID(t, f, cursorUniqueIndex); got != cursorOID {
		t.Fatalf("normalize changed cursor unique index oid %d → %d", cursorOID, got)
	}

	if err := runMigrations(f.ctx, f.pool, f.downOpts(t, aliasDownFiles(t))); err != nil {
		t.Fatalf("down after normalization: %v", err)
	}
	assertIndexExists(t, f.pool, f.schema, cursorUniqueIndex, false)
	assertFunctionExists(t, f, appendEventFunc, false)
	for _, alias := range fleetRenumberAliases {
		assertMigrationVersionRecorded(t, f.ctx, f.pool, f.schema, alias.OldVersion, false)
		assertMigrationVersionRecorded(t, f.ctx, f.pool, f.schema, alias.NewVersion, false)
	}
	assertUnrelatedLegacyPreserved(t, f)

	if err := runMigrations(f.ctx, f.pool, f.upOpts(t, aliasUpFiles(t))); err != nil {
		t.Fatalf("up after down: %v", err)
	}
	assertIndexReadyAndValid(t, f.pool, f.schema, cursorUniqueIndex, true)
	assertFunctionExists(t, f, appendEventFunc, true)
	rebuiltOID := indexOID(t, f, cursorUniqueIndex)
	if rebuiltOID == cursorOID {
		t.Fatal("down/up reused the original index oid instead of rebuilding")
	}
	for _, alias := range fleetRenumberAliases {
		assertMigrationVersionRecorded(t, f.ctx, f.pool, f.schema, alias.OldVersion, false)
		assertMigrationVersionRecorded(t, f.ctx, f.pool, f.schema, alias.NewVersion, true)
	}
}

func TestRenumberAliasesDoesNotNormalizeOnDown(t *testing.T) {
	f := newRenumberFixture(t)
	stamps := seedOldAliasesAndUnrelated(t, f)

	if err := runMigrations(f.ctx, f.pool, f.downOpts(t, aliasDownFiles(t))); err != nil {
		t.Fatalf("down with fork ledger names: %v", err)
	}

	assertOldAliasRowsUntouched(t, f, stamps)
	assertNewAliasRows(t, f, nil)
	assertUnrelatedLegacyPreserved(t, f)
}

type renumberFixture struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	schema   string
	tableSQL string
	tableFQN string
	lockKey  int64
}

func newRenumberFixture(t *testing.T) *renumberFixture {
	t.Helper()
	admin := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), rand.Uint32())
	schema := "migrate_renumber_" + suffix
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

	pool := openTestPoolWithSearchPath(t, schema)
	f := &renumberFixture{
		ctx:      ctx,
		pool:     pool,
		schema:   schema,
		tableSQL: pgx.Identifier{schema, "schema_migrations"}.Sanitize(),
		tableFQN: schema + ".schema_migrations",
		lockKey:  int64(rand.Uint64()&0x7fffffffffffffff) | 1,
	}
	if err := runMigrations(ctx, pool, f.upOpts(t, realMigrationFiles(t, []string{
		"408_workspace_event_outbox",
		"412_issue_create_idempotency",
	}, "up"))); err != nil {
		t.Fatalf("apply prerequisite tables: %v", err)
	}
	createWorkspaceEventCaptureTables(t, ctx, pool)
	return f
}

func (f *renumberFixture) upOpts(t *testing.T, files []string) runOptions {
	t.Helper()
	return runOptions{
		Direction:             "up",
		Files:                 files,
		SchemaMigrationsTable: f.tableFQN,
		AdvisoryLockKey:       f.lockKey,
		Hooks:                 hooksForDirection("up"),
	}
}

func (f *renumberFixture) downOpts(t *testing.T, files []string) runOptions {
	t.Helper()
	return runOptions{
		Direction:             "down",
		Files:                 files,
		SchemaMigrationsTable: f.tableFQN,
		AdvisoryLockKey:       f.lockKey,
		Hooks:                 hooksForDirection("down"),
	}
}

func prepareAppliedOldAliases(t *testing.T, f *renumberFixture) (map[string]time.Time, uint32) {
	t.Helper()
	if err := runMigrations(f.ctx, f.pool, f.upOpts(t, aliasUpFiles(t))); err != nil {
		t.Fatalf("apply current alias files: %v", err)
	}
	assertReplayRejected(t, f, "409_workspace_event_cursor_unique")
	assertReplayRejected(t, f, "415_workspace_event_capture")
	stamps := rewriteNewAliasesToOld(t, f)
	insertMigrationRow(t, f, unrelatedCurrent247, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	insertMigrationRow(t, f, unrelatedCurrent254, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	return stamps, indexOID(t, f, cursorUniqueIndex)
}

func rewriteNewAliasesToOld(t *testing.T, f *renumberFixture) map[string]time.Time {
	t.Helper()
	stamps := make(map[string]time.Time, len(fleetRenumberAliases))
	base := time.Date(2024, 3, 14, 15, 9, 26, 0, time.UTC)
	for i, alias := range fleetRenumberAliases {
		appliedAt := base.Add(time.Duration(i) * time.Minute)
		stamps[alias.OldVersion] = appliedAt
		tag, err := f.pool.Exec(f.ctx,
			"UPDATE "+f.tableSQL+" SET version = $1, applied_at = $2 WHERE version = $3",
			alias.OldVersion, appliedAt, alias.NewVersion,
		)
		if err != nil {
			t.Fatalf("rewrite %s -> %s: %v", alias.NewVersion, alias.OldVersion, err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("rewrite %s -> %s: expected 1 row, got %d", alias.NewVersion, alias.OldVersion, tag.RowsAffected())
		}
	}
	return stamps
}

func assertReplayRejected(t *testing.T, f *renumberFixture, version string) {
	t.Helper()
	path := realMigrationFiles(t, []string{version}, "up")[0]
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	conn, err := f.pool.Acquire(f.ctx)
	if err != nil {
		t.Fatalf("acquire replay connection: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(f.ctx, string(body)); err == nil {
		t.Fatalf("replay of %s unexpectedly succeeded", version)
	} else if !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		t.Fatalf("replay of %s: want already exists, got %v", version, err)
	}
}

func seedOldAliasesAndUnrelated(t *testing.T, f *renumberFixture) map[string]time.Time {
	t.Helper()
	stamps := make(map[string]time.Time, len(fleetRenumberAliases))
	base := time.Date(2024, 3, 14, 15, 9, 26, 0, time.UTC)
	for i, alias := range fleetRenumberAliases {
		appliedAt := base.Add(time.Duration(i) * time.Minute)
		stamps[alias.OldVersion] = appliedAt
		insertMigrationRow(t, f, alias.OldVersion, appliedAt)
	}
	insertMigrationRow(t, f, unrelatedCurrent247, base.Add(-time.Hour))
	insertMigrationRow(t, f, unrelatedCurrent254, base.Add(-2*time.Hour))
	return stamps
}

func insertMigrationRow(t *testing.T, f *renumberFixture, version string, appliedAt time.Time) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		"INSERT INTO "+f.tableSQL+" (version, applied_at) VALUES ($1, $2)",
		version, appliedAt,
	); err != nil {
		t.Fatalf("insert schema_migrations %q: %v", version, err)
	}
}

func assertAliasLedgerNormalized(t *testing.T, f *renumberFixture, stamps map[string]time.Time) {
	t.Helper()
	for _, alias := range fleetRenumberAliases {
		assertMigrationVersionRecorded(t, f.ctx, f.pool, f.schema, alias.OldVersion, false)
		assertMigrationVersionRecorded(t, f.ctx, f.pool, f.schema, alias.NewVersion, true)
		got := migrationAppliedAt(t, f, alias.NewVersion)
		want := stamps[alias.OldVersion]
		if !got.Equal(want) {
			t.Fatalf("%s applied_at = %s, want preserved %s", alias.NewVersion, got, want)
		}
	}
}

func assertOldAliasRowsUntouched(t *testing.T, f *renumberFixture, stamps map[string]time.Time) {
	t.Helper()
	for _, alias := range fleetRenumberAliases {
		assertMigrationVersionRecorded(t, f.ctx, f.pool, f.schema, alias.OldVersion, true)
		got := migrationAppliedAt(t, f, alias.OldVersion)
		want := stamps[alias.OldVersion]
		if !got.Equal(want) {
			t.Fatalf("%s applied_at = %s, want untouched %s", alias.OldVersion, got, want)
		}
	}
}

func assertNewAliasRows(t *testing.T, f *renumberFixture, present map[string]bool) {
	t.Helper()
	for _, alias := range fleetRenumberAliases {
		assertMigrationVersionRecorded(t, f.ctx, f.pool, f.schema, alias.NewVersion, present[alias.NewVersion])
	}
}

func assertUnrelatedLegacyPreserved(t *testing.T, f *renumberFixture) {
	t.Helper()
	assertMigrationVersionRecorded(t, f.ctx, f.pool, f.schema, unrelatedCurrent247, true)
	assertMigrationVersionRecorded(t, f.ctx, f.pool, f.schema, unrelatedCurrent254, true)
	assertMigrationVersionRecorded(t, f.ctx, f.pool, f.schema, "408_workspace_event_outbox", true)
	assertMigrationVersionRecorded(t, f.ctx, f.pool, f.schema, "412_issue_create_idempotency", true)
}

func migrationAppliedAt(t *testing.T, f *renumberFixture, version string) time.Time {
	t.Helper()
	var appliedAt time.Time
	if err := f.pool.QueryRow(f.ctx,
		"SELECT applied_at FROM "+f.tableSQL+" WHERE version = $1",
		version,
	).Scan(&appliedAt); err != nil {
		t.Fatalf("read applied_at for %s: %v", version, err)
	}
	return appliedAt.UTC()
}

func indexOID(t *testing.T, f *renumberFixture, index string) uint32 {
	t.Helper()
	var oid uint32
	if err := f.pool.QueryRow(f.ctx, `
		SELECT c.oid
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind = 'i'
	`, f.schema, index).Scan(&oid); err != nil {
		t.Fatalf("read oid for %s.%s: %v", f.schema, index, err)
	}
	return oid
}

func assertFunctionExists(t *testing.T, f *renumberFixture, name string, want bool) {
	t.Helper()
	var exists bool
	if err := f.pool.QueryRow(f.ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_proc p
			JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = $1 AND p.proname = $2
		)
	`, f.schema, name).Scan(&exists); err != nil {
		t.Fatalf("read function %s.%s: %v", f.schema, name, err)
	}
	if exists != want {
		t.Fatalf("function %s.%s exists = %v, want %v", f.schema, name, exists, want)
	}
}

func aliasUpFiles(t *testing.T) []string {
	t.Helper()
	versions := make([]string, len(fleetRenumberAliases))
	for i, alias := range fleetRenumberAliases {
		versions[i] = alias.NewVersion
	}
	return realMigrationFiles(t, versions, "up")
}

func aliasDownFiles(t *testing.T) []string {
	t.Helper()
	n := len(fleetRenumberAliases)
	versions := make([]string, n)
	for i, alias := range fleetRenumberAliases {
		versions[n-1-i] = alias.NewVersion
	}
	return realMigrationFiles(t, versions, "down")
}

func writeAliasUpFiles(t *testing.T, dir, mutateNewVersion string) []string {
	t.Helper()
	files := make([]string, 0, len(fleetRenumberAliases))
	for _, alias := range fleetRenumberAliases {
		src := realMigrationFiles(t, []string{alias.NewVersion}, "up")[0]
		body, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		if alias.NewVersion == mutateNewVersion {
			body = append(append([]byte{}, body...), ' ')
		}
		dst := filepath.Join(dir, filepath.Base(src))
		if err := os.WriteFile(dst, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", dst, err)
		}
		files = append(files, dst)
	}
	return files
}

func createWorkspaceEventCaptureTables(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	statements := []string{
		`CREATE TABLE issue (id uuid)`,
		`CREATE TABLE comment (id uuid)`,
		`CREATE TABLE agent_task_queue (id uuid, status text)`,
		`CREATE TABLE autopilot_run (id uuid, status text)`,
		`CREATE TABLE attachment (id uuid)`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create capture fixture table: %v", err)
		}
	}
}
