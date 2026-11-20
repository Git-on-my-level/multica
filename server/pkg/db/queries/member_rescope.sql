-- name: ArmMemberRescopeReceipt :exec
INSERT INTO member_rescope_receipt (issue_id, agent_id, refusal_task_id, armed_at)
VALUES ($1, $2, $3, clock_timestamp());

-- name: GetMemberRescopeReceipt :one
SELECT * FROM member_rescope_receipt
WHERE issue_id = $1 AND agent_id = $2
ORDER BY armed_at DESC, refusal_task_id DESC
LIMIT 1;

-- name: AdmitMemberRescopeTask :one
-- The per-agent claim lock serializes consumers. Save the admitted task ID
-- so recovering an undelivered claim can dispatch that same task again.
WITH consumed AS (
    UPDATE member_rescope_receipt AS r
    SET admitted_task_id = sqlc.arg(task_id),
        admitted_comment_id = sqlc.arg(comment_id),
        admitted_at = clock_timestamp()
    WHERE r.issue_id = sqlc.arg(issue_id) AND r.agent_id = sqlc.arg(agent_id)
      AND r.armed_at = sqlc.arg(armed_at) AND r.refusal_task_id = sqlc.arg(refusal_task_id)
      AND r.admitted_task_id IS NULL
    RETURNING admitted_task_id
)
UPDATE agent_task_queue AS t
SET force_fresh_session = true,
    coalesced_comment_ids = '{}',
    handoff_note = NULL
FROM consumed
WHERE t.id = consumed.admitted_task_id AND t.status = 'dispatched'
RETURNING t.*;

-- name: GetMemberRescopeTrigger :one
SELECT c.created_at,
       (d.comment_id IS NOT NULL AND c.author_type = 'member' AND c.revision = 1 AND c.deleted_at IS NULL AND c.author_id = sqlc.arg(originator_user_id)) AS eligible
FROM comment c
LEFT JOIN member_rescope_directive d ON d.comment_id = c.id
WHERE c.id = sqlc.arg(comment_id) AND c.issue_id = sqlc.arg(issue_id);
