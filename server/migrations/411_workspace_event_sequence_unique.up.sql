CREATE UNIQUE INDEX CONCURRENTLY workspace_event_outbox_sequence_uidx ON workspace_event_outbox (workspace_id, sequence);
