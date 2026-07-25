-- name: GetIssueForPRHandoff :one
SELECT i.* FROM issue i
JOIN workspace w ON w.id = i.workspace_id
WHERE i.id = $1
FOR KEY SHARE OF w, i;

-- name: LockIssueForPRHandoffDelete :one
SELECT id FROM issue
WHERE id = $1 AND workspace_id = $2
FOR UPDATE;

-- name: UpsertIssuePRHandoffCandidate :one
INSERT INTO issue_pr_handoff_candidate (
    workspace_id, issue_id, task_id, url, repo_owner, repo_name, pr_number, state
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
)
ON CONFLICT (task_id, url) DO UPDATE SET
    state = CASE
        WHEN issue_pr_handoff_candidate.state = 'linked' THEN 'linked'
        ELSE EXCLUDED.state
    END,
    updated_at = now()
RETURNING *;

-- name: ListLatestIssuePRHandoffCandidates :many
-- A later completed follow-up with no candidate must not erase the last
-- reported handoff. A later task that does report candidates supersedes the
-- prior task as one source-bound candidate group.
WITH latest_candidate_task AS (
    SELECT t.id AS task_id
    FROM agent_task_queue t
    WHERE t.issue_id = $1
      AND t.status = 'completed'
      AND EXISTS (
          SELECT 1
          FROM issue_pr_handoff_candidate c
          WHERE c.task_id = t.id AND c.issue_id = $1
      )
    ORDER BY t.completed_at DESC NULLS LAST, t.created_at DESC, t.id DESC
    LIMIT 1
)
SELECT c.*
FROM issue_pr_handoff_candidate c
JOIN latest_candidate_task t ON t.task_id = c.task_id
WHERE c.issue_id = $1
ORDER BY c.created_at ASC, c.url ASC;

-- name: ListAwaitingPRHandoffCandidates :many
SELECT * FROM issue_pr_handoff_candidate
WHERE workspace_id = $1
  AND repo_owner = $2
  AND repo_name = $3
  AND pr_number = $4
  AND state = 'awaiting_mirror'
ORDER BY created_at ASC;

-- name: MarkIssuePRHandoffCandidateLinked :exec
UPDATE issue_pr_handoff_candidate
SET state = 'linked', updated_at = now()
WHERE id = $1;

-- name: DeleteIssuePRHandoffCandidates :exec
DELETE FROM issue_pr_handoff_candidate
WHERE issue_id = $1;

-- name: DeleteWorkspacePRHandoffCandidates :exec
DELETE FROM issue_pr_handoff_candidate
WHERE workspace_id = $1;
