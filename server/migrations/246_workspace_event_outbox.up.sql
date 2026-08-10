-- Durable, workspace-ordered domain event replay for automation clients.
-- Relationships are application-enforced; this table intentionally has no FKs.
CREATE TABLE workspace_event_cursor (
    workspace_id UUID NOT NULL,
    last_sequence BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE workspace_event_outbox (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    sequence BIGINT NOT NULL,
    source_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    aggregate_kind TEXT NOT NULL,
    aggregate_id UUID NOT NULL,
    actor_type TEXT,
    actor_id UUID,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	recorded_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    CONSTRAINT workspace_event_outbox_payload_size
        CHECK (pg_column_size(payload) <= 65536)
);
