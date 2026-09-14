CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS workspace_event_outbox_source_uidx ON workspace_event_outbox (workspace_id, source_id);
