-- A bounded outbox: at most one outstanding attention signal per child.
-- No backfill: installing a server must not start work on historical issues.
CREATE TABLE issue_child_attention (
    child_id UUID NOT NULL,
    generation UUID NOT NULL DEFAULT gen_random_uuid(),
    parent_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    status TEXT NOT NULL,
    available_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
