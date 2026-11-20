CREATE TABLE member_rescope_receipt (
    issue_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    refusal_task_id uuid NOT NULL,
    armed_at timestamptz NOT NULL,
    admitted_task_id uuid,
    admitted_at timestamptz
);

CREATE TABLE member_rescope_directive (
    comment_id uuid NOT NULL
);
