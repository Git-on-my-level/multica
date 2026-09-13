CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS issue_create_idempotency_workspace_key_uidx ON issue_create_idempotency (workspace_id, client_key_hash);
