CREATE INDEX CONCURRENTLY issue_child_attention_due_idx ON issue_child_attention (available_at, parent_id);
