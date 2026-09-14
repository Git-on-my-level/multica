-- Deliberately no workspace/issue/task FKs: those hot ownership trees use
-- explicit teardown sweeps elsewhere. Completion takes KEY SHARE locks on the
-- workspace+issue and issue deletion takes UPDATE before sweeping, which closes
-- the insert-after-sweep race without adding cascade paths to this table.
--
-- IF NOT EXISTS: fork previously applied this as 214_issue_pr_handoff_candidate
-- before upstream claimed 214 for chat_session_project; renumber to 241.
CREATE TABLE IF NOT EXISTS issue_pr_handoff_candidate (
    id           UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    issue_id     UUID NOT NULL,
    task_id      UUID NOT NULL,
    url          TEXT NOT NULL,
    repo_owner   TEXT NOT NULL,
    repo_name    TEXT NOT NULL,
    pr_number    INTEGER NOT NULL,
    state        TEXT NOT NULL CHECK (state IN (
        'candidate_detected',
        'awaiting_mirror',
        'linked',
        'invalid_external_pr'
    )),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
