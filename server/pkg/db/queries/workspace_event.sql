-- name: ListWorkspaceEventsAfter :many
SELECT id, workspace_id, sequence, source_id, event_type, aggregate_kind,
       aggregate_id, actor_type, actor_id, occurred_at, payload
FROM workspace_event_outbox
WHERE workspace_id = @workspace_id
  AND sequence > @after_sequence
  AND (COALESCE(cardinality(@event_types::text[]), 0) = 0 OR event_type = ANY(@event_types::text[]))
ORDER BY sequence ASC
LIMIT @result_limit;

-- name: GetWorkspaceEventBounds :one
SELECT COALESCE(
           (SELECT MIN(sequence) FROM workspace_event_outbox WHERE workspace_id = sqlc.arg('workspace_id')::uuid),
           (SELECT CASE WHEN last_sequence > 0 THEN last_sequence + 1 ELSE 0 END
              FROM workspace_event_cursor WHERE workspace_id = sqlc.arg('workspace_id')::uuid),
           0
       )::bigint AS min_sequence,
       COALESCE(
           (SELECT last_sequence FROM workspace_event_cursor WHERE workspace_id = sqlc.arg('workspace_id')::uuid),
           0
       )::bigint AS max_sequence;

-- name: AppendWorkspaceEvent :one
SELECT * FROM append_workspace_event(
    @workspace_id,
    @source_id,
    @event_type,
    @aggregate_kind,
    @aggregate_id,
    @actor_type,
    @actor_id,
    @payload,
    @occurred_at
);

-- name: DeleteWorkspaceEventsByWorkspace :exec
DELETE FROM workspace_event_outbox WHERE workspace_id = @workspace_id;

-- name: DeleteWorkspaceEventCursorByWorkspace :exec
DELETE FROM workspace_event_cursor WHERE workspace_id = @workspace_id;

-- name: ListWorkspaceEventCursorWorkspaces :many
SELECT workspace_id
FROM workspace_event_cursor
ORDER BY workspace_id;

-- name: CountPrunableWorkspaceEvents :one
WITH first_retained AS (
    SELECT MIN(sequence) AS sequence
    FROM workspace_event_outbox
    WHERE workspace_id = sqlc.arg('workspace_id')::uuid
      AND recorded_at >= sqlc.arg('cutoff')::timestamptz
)
SELECT COUNT(*)::bigint
FROM workspace_event_outbox event
CROSS JOIN first_retained
WHERE event.workspace_id = sqlc.arg('workspace_id')::uuid
  AND event.recorded_at < sqlc.arg('cutoff')::timestamptz
  AND (first_retained.sequence IS NULL OR event.sequence < first_retained.sequence);

-- name: PruneWorkspaceEventsBatch :one
WITH locked_cursor AS MATERIALIZED (
    SELECT workspace_id
    FROM workspace_event_cursor
    WHERE workspace_id = sqlc.arg('workspace_id')::uuid
    FOR UPDATE
),
first_retained AS MATERIALIZED (
    SELECT MIN(event.sequence) AS sequence
    FROM workspace_event_outbox event
    CROSS JOIN locked_cursor
    WHERE event.workspace_id = sqlc.arg('workspace_id')::uuid
      AND event.recorded_at >= sqlc.arg('cutoff')::timestamptz
),
candidates AS MATERIALIZED (
    SELECT event.id, event.sequence
    FROM workspace_event_outbox event
    CROSS JOIN locked_cursor
    CROSS JOIN first_retained
    WHERE event.workspace_id = sqlc.arg('workspace_id')::uuid
      AND event.recorded_at < sqlc.arg('cutoff')::timestamptz
      AND (first_retained.sequence IS NULL OR event.sequence < first_retained.sequence)
    ORDER BY event.sequence
    LIMIT sqlc.arg('batch_size')
),
deleted AS (
    DELETE FROM workspace_event_outbox event
    USING candidates
    WHERE event.id = candidates.id
    RETURNING event.sequence
)
SELECT COUNT(*)::bigint AS deleted_count,
       COALESCE(MIN(sequence), 0)::bigint AS first_deleted_sequence,
       COALESCE(MAX(sequence), 0)::bigint AS last_deleted_sequence
FROM deleted;
