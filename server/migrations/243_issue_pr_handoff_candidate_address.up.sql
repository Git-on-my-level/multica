CREATE INDEX CONCURRENTLY issue_pr_handoff_candidate_address_idx
ON issue_pr_handoff_candidate (workspace_id, repo_owner, repo_name, pr_number)
WHERE state = 'awaiting_mirror';
