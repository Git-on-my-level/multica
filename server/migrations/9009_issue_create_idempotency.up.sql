-- Workspace-scoped authority binding for crash-safe issue promotion retries.
-- Only hashes and the resulting issue id are stored; prompts/briefs are never retained here.
CREATE TABLE IF NOT EXISTS issue_create_idempotency (
    workspace_id UUID NOT NULL,
    client_key_hash TEXT NOT NULL,
    semantic_digest TEXT NOT NULL,
    issue_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT issue_create_client_key_hash_format CHECK (client_key_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT issue_create_semantic_digest_format CHECK (semantic_digest ~ '^[0-9a-f]{64}$')
);
