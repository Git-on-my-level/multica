CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS member_rescope_receipt_idx
    ON member_rescope_receipt (issue_id, agent_id, armed_at DESC, refusal_task_id DESC);
