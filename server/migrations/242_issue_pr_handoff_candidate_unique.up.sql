CREATE UNIQUE INDEX CONCURRENTLY issue_pr_handoff_candidate_task_url_uidx
ON issue_pr_handoff_candidate (task_id, url);
