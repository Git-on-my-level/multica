-- name: ReserveIssueCreateClientKey :execrows
INSERT INTO issue_create_idempotency (workspace_id, client_key_hash, semantic_digest)
VALUES (@workspace_id, @client_key_hash, @semantic_digest)
ON CONFLICT (workspace_id, client_key_hash) DO NOTHING;

-- name: GetIssueCreateClientKey :one
SELECT workspace_id, client_key_hash, semantic_digest, issue_id, created_at
FROM issue_create_idempotency
WHERE workspace_id = @workspace_id AND client_key_hash = @client_key_hash;

-- name: BindIssueCreateClientKey :execrows
UPDATE issue_create_idempotency
SET issue_id = @issue_id
WHERE workspace_id = @workspace_id
  AND client_key_hash = @client_key_hash
  AND semantic_digest = @semantic_digest
  AND issue_id IS NULL;

-- name: DeleteIssueCreateClientKeysByWorkspace :exec
DELETE FROM issue_create_idempotency WHERE workspace_id = @workspace_id;
