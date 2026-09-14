CREATE INDEX CONCURRENTLY IF NOT EXISTS workspace_event_outbox_retention_idx ON workspace_event_outbox (workspace_id, recorded_at, sequence);
