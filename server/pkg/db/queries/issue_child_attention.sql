-- name: ListDueChildAttentionParents :many
SELECT parent_id FROM issue_child_attention
WHERE available_at <= now()
GROUP BY parent_id ORDER BY min(available_at), parent_id LIMIT $1;

-- name: TryLockChildAttentionParent :one
SELECT pg_try_advisory_xact_lock(hashtextextended('child_attention:' || sqlc.arg(parent_id)::uuid::text, 0))::boolean AS acquired;

-- name: LockChildAttentionParent :one
SELECT * FROM issue WHERE id = $1 FOR UPDATE;

-- name: LockChildAttentionWorkspace :one
SELECT w.id FROM workspace w JOIN issue i ON i.workspace_id = w.id
WHERE i.id = $1 FOR KEY SHARE OF w;

-- name: LockChildAttentionAgent :one
SELECT a.id FROM issue i
LEFT JOIN squad s ON i.assignee_type = 'squad' AND s.id = i.assignee_id AND s.workspace_id = i.workspace_id
JOIN agent a ON a.id = CASE WHEN i.assignee_type = 'agent' THEN i.assignee_id ELSE s.leader_id END
    AND a.workspace_id = i.workspace_id
WHERE i.id = $1 FOR KEY SHARE OF a;

-- name: LockChildAttention :many
SELECT * FROM issue_child_attention
WHERE parent_id = $1 AND available_at <= now()
ORDER BY child_id LIMIT 100 FOR UPDATE;

-- name: DeleteChildAttention :exec
DELETE FROM issue_child_attention WHERE child_id = ANY($1::uuid[]);

-- name: DeferChildAttention :exec
UPDATE issue_child_attention SET available_at = now() + interval '1 minute'
WHERE child_id = ANY(sqlc.arg(child_ids)::uuid[]) AND generation = ANY(sqlc.arg(generations)::uuid[]);
