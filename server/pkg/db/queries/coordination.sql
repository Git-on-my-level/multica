-- name: FindActiveDuplicateIssuesByTitle :many
SELECT * FROM issue
WHERE workspace_id = $1
  AND status NOT IN ('done', 'cancelled')
  AND lower(btrim(regexp_replace(title, '[[:space:]]+', ' ', 'g'))) = $2
ORDER BY created_at ASC;

-- name: ListOpenPullRequestsByRepository :many
SELECT * FROM github_pull_request
WHERE workspace_id = $1
  AND repo_owner = $2
  AND repo_name = $3
  AND state IN ('open', 'draft')
ORDER BY pr_updated_at DESC
LIMIT 20;
