# Migration runner operations

## Recover fork migrations renumbered by merge af021d45b

Merge `af021d45b` renumbered seven workspace-event migrations after they
had already been applied on the fork. Databases that recorded the fork
names still have these `schema_migrations` rows:

| Recorded (fork) | Current file |
| --- | --- |
| `247_workspace_event_cursor_unique` | `409_workspace_event_cursor_unique` |
| `248_workspace_event_source_unique` | `410_workspace_event_source_unique` |
| `249_workspace_event_sequence_unique` | `411_workspace_event_sequence_unique` |
| `251_issue_create_idempotency_unique` | `413_issue_create_idempotency_unique` |
| `252_workspace_event_id_unique` | `414_workspace_event_id_unique` |
| `253_workspace_event_capture` | `415_workspace_event_capture` |
| `254_workspace_event_retention_index` | `416_workspace_event_retention_index` |

Each pair is the same `.up.sql` bytes. The existing indexes and
`415_workspace_event_capture` functions are already present; they are
not leftovers from a failed concurrent build.

`migrate up` (including the normal fleet updater) rewrites those seven
ledger rows to the current names, preserving `applied_at`, then skips
the identical DDL. Fresh databases have no fork rows and apply the
current files as usual. Do not hand-edit production `schema_migrations`
and do not treat a valid `workspace_event_cursor_workspace_uidx` as a
crash-recovery index problem. 408 and 412 are not part of this alias
set.

## Recover the comment content search index

Migration 371 keeps exactly one comment-content search index per environment:
`idx_comment_content_bigm` when `pg_bigm` is usable, otherwise the portable
`idx_comment_content_trgm` fallback. A conditionally skipped migration is still
recorded in `schema_migrations`, so rerunning `migrate up` does not recreate the
fallback if the selected bigram index is later dropped or becomes invalid.

First check whether either index is live, ready, and valid:

```sql
SELECT indexrelid::regclass AS index_name, indisvalid, indisready, indislive
FROM pg_index
WHERE indexrelid IN (
    to_regclass('idx_comment_content_bigm'),
    to_regclass('idx_comment_content_trgm')
);
```

If neither index is usable, restore the portable fallback before serving search
traffic. Run each statement separately and outside a transaction so the
concurrent index build is valid:

```sql
CREATE EXTENSION IF NOT EXISTS pg_trgm;
DROP INDEX CONCURRENTLY IF EXISTS idx_comment_content_trgm;
CREATE INDEX CONCURRENTLY idx_comment_content_trgm
    ON comment USING gin (LOWER(content) gin_trgm_ops);
```

Verify that `idx_comment_content_trgm` reports all three flags as `true` before
resuming traffic. If `idx_comment_content_bigm` is repaired later, keep the
fallback until the bigram index also reports all three flags as `true` **and**
has the exact migration 036 shape: a non-unique, non-partial GIN index on
`LOWER(content)` using the `pg_bigm`-owned `gin_bigm_ops` operator class. Only
then can the fallback be dropped with `DROP INDEX CONCURRENTLY` during a
maintenance window.
