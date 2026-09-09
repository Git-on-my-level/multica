-- IF NOT EXISTS is required, not cosmetic: CREATE INDEX CONCURRENTLY cannot run
-- inside a transaction, so the runner applies this file and then records the
-- version in schema_migrations as two separate statements. If the process dies
-- between them the index is VALID but unrecorded, and the registered
-- invalid-index cleanup hook correctly leaves it alone — a bare CREATE then
-- fails 42P07 on every retry and wedges the migrator permanently.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS workspace_event_cursor_workspace_uidx ON workspace_event_cursor (workspace_id);
