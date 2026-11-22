CREATE TABLE member_rescope_receipt (
    issue_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    refusal_task_id uuid NOT NULL,
    armed_at timestamptz NOT NULL,
    admitted_task_id uuid,
    admitted_comment_id uuid,
    admitted_at timestamptz,
    CHECK (
        (admitted_task_id IS NULL AND admitted_comment_id IS NULL AND admitted_at IS NULL)
        OR (admitted_task_id IS NOT NULL AND admitted_comment_id IS NOT NULL AND admitted_at IS NOT NULL)
    )
);

CREATE TABLE member_rescope_directive (
    comment_id uuid NOT NULL
);
