-- Appends exactly once for a caller-supplied source id and assigns the next
-- sequence while holding the workspace cursor row lock. Because the cursor
-- update and event insert share the caller's transaction, rollback removes
-- both and concurrent commits for one workspace cannot become visible out of
-- sequence.
CREATE OR REPLACE FUNCTION append_workspace_event(
    p_workspace_id UUID,
    p_source_id TEXT,
    p_event_type TEXT,
    p_aggregate_kind TEXT,
    p_aggregate_id UUID,
    p_actor_type TEXT DEFAULT NULL,
    p_actor_id UUID DEFAULT NULL,
    p_payload JSONB DEFAULT '{}'::jsonb,
    p_occurred_at TIMESTAMPTZ DEFAULT now()
) RETURNS workspace_event_outbox
LANGUAGE plpgsql
AS $$
DECLARE
    existing workspace_event_outbox;
    next_sequence BIGINT;
    appended workspace_event_outbox;
BEGIN
    IF p_workspace_id IS NULL OR p_source_id IS NULL OR btrim(p_source_id) = '' THEN
        RAISE EXCEPTION 'workspace_id and source_id are required';
    END IF;
    IF p_event_type IS NULL OR btrim(p_event_type) = '' OR
       p_aggregate_kind IS NULL OR btrim(p_aggregate_kind) = '' OR
       p_aggregate_id IS NULL THEN
        RAISE EXCEPTION 'event_type, aggregate_kind, and aggregate_id are required';
    END IF;
    IF pg_column_size(COALESCE(p_payload, '{}'::jsonb)) > 65536 THEN
        RAISE EXCEPTION 'workspace event payload exceeds 65536 bytes';
    END IF;

    SELECT * INTO existing
    FROM workspace_event_outbox
    WHERE workspace_id = p_workspace_id AND source_id = p_source_id;
    IF FOUND THEN
		IF existing.event_type IS DISTINCT FROM p_event_type OR
		   existing.aggregate_kind IS DISTINCT FROM p_aggregate_kind OR
		   existing.aggregate_id IS DISTINCT FROM p_aggregate_id THEN
			RAISE EXCEPTION 'workspace event source_id conflicts with an existing event';
		END IF;
        RETURN existing;
    END IF;

    INSERT INTO workspace_event_cursor (workspace_id, last_sequence)
    VALUES (p_workspace_id, 1)
    ON CONFLICT (workspace_id) DO UPDATE
    SET last_sequence = workspace_event_cursor.last_sequence + 1,
        updated_at = now()
    RETURNING last_sequence INTO next_sequence;

    -- The cursor row serializes all appends for this workspace. Recheck after
    -- acquiring it so concurrent retries of one source do not consume a slot.
    SELECT * INTO existing
    FROM workspace_event_outbox
    WHERE workspace_id = p_workspace_id AND source_id = p_source_id;
    IF FOUND THEN
        UPDATE workspace_event_cursor
        SET last_sequence = last_sequence - 1, updated_at = now()
        WHERE workspace_id = p_workspace_id;
		IF existing.event_type IS DISTINCT FROM p_event_type OR
		   existing.aggregate_kind IS DISTINCT FROM p_aggregate_kind OR
		   existing.aggregate_id IS DISTINCT FROM p_aggregate_id THEN
			RAISE EXCEPTION 'workspace event source_id conflicts with an existing event';
		END IF;
        RETURN existing;
    END IF;

    INSERT INTO workspace_event_outbox (
        workspace_id, sequence, source_id, event_type, aggregate_kind,
        aggregate_id, actor_type, actor_id, occurred_at, payload
    ) VALUES (
        p_workspace_id, next_sequence, p_source_id, p_event_type,
        p_aggregate_kind, p_aggregate_id, p_actor_type, p_actor_id,
        p_occurred_at, COALESCE(p_payload, '{}'::jsonb)
    )
    RETURNING * INTO appended;

    RETURN appended;
END;
$$;

CREATE OR REPLACE FUNCTION capture_issue_workspace_event() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    row_data issue;
    operation TEXT;
BEGIN
    row_data := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
    operation := CASE TG_OP WHEN 'INSERT' THEN 'created' WHEN 'UPDATE' THEN 'updated' ELSE 'deleted' END;
    IF TG_OP = 'UPDATE' AND NEW IS NOT DISTINCT FROM OLD THEN
        RETURN NEW;
    END IF;
    PERFORM append_workspace_event(
        row_data.workspace_id,
        'issue:' || row_data.id::text || ':' || gen_random_uuid()::text,
        'issue:' || operation,
        'issue', row_data.id,
        row_data.creator_type, row_data.creator_id,
        jsonb_strip_nulls(jsonb_build_object(
            'number', row_data.number,
            'status', row_data.status,
            'priority', row_data.priority,
            'assignee_type', row_data.assignee_type,
            'assignee_id', row_data.assignee_id,
            'parent_issue_id', row_data.parent_issue_id,
            'project_id', row_data.project_id
        )),
        CASE WHEN TG_OP = 'INSERT' THEN row_data.created_at ELSE row_data.updated_at END
    );
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END;
$$;

DROP TRIGGER IF EXISTS capture_issue_workspace_event_trigger ON issue;
CREATE TRIGGER capture_issue_workspace_event_trigger
AFTER INSERT OR UPDATE OR DELETE ON issue
FOR EACH ROW EXECUTE FUNCTION capture_issue_workspace_event();

CREATE OR REPLACE FUNCTION capture_comment_workspace_event() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    row_data comment;
    operation TEXT;
BEGIN
    row_data := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
    operation := CASE TG_OP WHEN 'INSERT' THEN 'created' WHEN 'UPDATE' THEN 'updated' ELSE 'deleted' END;
    IF TG_OP = 'UPDATE' AND NEW IS NOT DISTINCT FROM OLD THEN
        RETURN NEW;
    END IF;
    PERFORM append_workspace_event(
        row_data.workspace_id,
        'comment:' || row_data.id::text || ':' || gen_random_uuid()::text,
        'comment:' || operation,
        'comment', row_data.id,
        row_data.author_type, row_data.author_id,
        jsonb_strip_nulls(jsonb_build_object(
            'issue_id', row_data.issue_id,
            'comment_type', row_data.type,
            'parent_id', row_data.parent_id,
            'source_task_id', row_data.source_task_id,
            'resolved_at', row_data.resolved_at
        )),
        CASE WHEN TG_OP = 'INSERT' THEN row_data.created_at ELSE row_data.updated_at END
    );
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END;
$$;

DROP TRIGGER IF EXISTS capture_comment_workspace_event_trigger ON comment;
CREATE TRIGGER capture_comment_workspace_event_trigger
AFTER INSERT OR UPDATE OR DELETE ON comment
FOR EACH ROW EXECUTE FUNCTION capture_comment_workspace_event();

CREATE OR REPLACE FUNCTION capture_task_workspace_event() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    row_data agent_task_queue;
    workspace UUID;
    event_status TEXT;
BEGIN
    row_data := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
    IF TG_OP = 'UPDATE' AND NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
    END IF;

    SELECT i.workspace_id INTO workspace FROM issue i WHERE i.id = row_data.issue_id;
    IF workspace IS NULL THEN
        SELECT cs.workspace_id INTO workspace FROM chat_session cs WHERE cs.id = row_data.chat_session_id;
    END IF;
    IF workspace IS NULL THEN
        SELECT a.workspace_id INTO workspace
        FROM autopilot_run ar JOIN autopilot a ON a.id = ar.autopilot_id
        WHERE ar.id = row_data.autopilot_run_id;
    END IF;
    IF workspace IS NULL AND row_data.context ? 'workspace_id' AND
       (row_data.context->>'workspace_id') ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$' THEN
        workspace := (row_data.context->>'workspace_id')::uuid;
    END IF;
    IF workspace IS NULL THEN
        RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
    END IF;

    event_status := CASE WHEN TG_OP = 'DELETE' THEN 'deleted' ELSE row_data.status END;
    PERFORM append_workspace_event(
        workspace,
        'task:' || row_data.id::text || ':' || gen_random_uuid()::text,
        'task:' || event_status,
        'task', row_data.id,
        'system', NULL,
        jsonb_strip_nulls(jsonb_build_object(
            'status', event_status,
            'agent_id', row_data.agent_id,
            'issue_id', row_data.issue_id,
            'chat_session_id', row_data.chat_session_id,
            'autopilot_run_id', row_data.autopilot_run_id,
            'attempt', row_data.attempt,
            'parent_task_id', row_data.parent_task_id,
            'retry_of_task_id', row_data.retry_of_task_id,
            'rerun_of_task_id', row_data.rerun_of_task_id
        )),
        COALESCE(row_data.completed_at, row_data.started_at, row_data.dispatched_at, row_data.created_at, now())
    );
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END;
$$;

DROP TRIGGER IF EXISTS capture_task_workspace_event_trigger ON agent_task_queue;
CREATE TRIGGER capture_task_workspace_event_trigger
AFTER INSERT OR UPDATE OF status OR DELETE ON agent_task_queue
FOR EACH ROW EXECUTE FUNCTION capture_task_workspace_event();

CREATE OR REPLACE FUNCTION capture_run_workspace_event() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    row_data autopilot_run;
    workspace UUID;
    event_status TEXT;
BEGIN
    row_data := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
    IF TG_OP = 'UPDATE' AND NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
    END IF;
    SELECT a.workspace_id INTO workspace FROM autopilot a WHERE a.id = row_data.autopilot_id;
    IF workspace IS NULL THEN
        RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
    END IF;
    event_status := CASE WHEN TG_OP = 'DELETE' THEN 'deleted' ELSE row_data.status END;
    PERFORM append_workspace_event(
        workspace,
        'run:' || row_data.id::text || ':' || gen_random_uuid()::text,
        'run:' || event_status,
        'run', row_data.id,
        'system', NULL,
        jsonb_strip_nulls(jsonb_build_object(
            'status', event_status,
            'autopilot_id', row_data.autopilot_id,
            'trigger_id', row_data.trigger_id,
            'source', row_data.source,
            'issue_id', row_data.issue_id,
            'task_id', row_data.task_id,
            'completed_at', row_data.completed_at
        )),
        COALESCE(row_data.completed_at, row_data.triggered_at, row_data.created_at, now())
    );
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END;
$$;

DROP TRIGGER IF EXISTS capture_run_workspace_event_trigger ON autopilot_run;
CREATE TRIGGER capture_run_workspace_event_trigger
AFTER INSERT OR UPDATE OF status OR DELETE ON autopilot_run
FOR EACH ROW EXECUTE FUNCTION capture_run_workspace_event();

CREATE OR REPLACE FUNCTION capture_artifact_workspace_event() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    row_data attachment;
    operation TEXT;
BEGIN
    row_data := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
    operation := CASE TG_OP WHEN 'INSERT' THEN 'created' WHEN 'UPDATE' THEN 'updated' ELSE 'deleted' END;
    IF TG_OP = 'UPDATE' AND NEW IS NOT DISTINCT FROM OLD THEN
        RETURN NEW;
    END IF;
    PERFORM append_workspace_event(
        row_data.workspace_id,
        'artifact:' || row_data.id::text || ':' || gen_random_uuid()::text,
        'artifact:' || operation,
        'artifact', row_data.id,
        row_data.uploader_type, row_data.uploader_id,
        jsonb_strip_nulls(jsonb_build_object(
            'issue_id', row_data.issue_id,
            'comment_id', row_data.comment_id,
            'chat_session_id', row_data.chat_session_id,
            'chat_message_id', row_data.chat_message_id,
            'task_id', row_data.task_id,
            'filename', row_data.filename,
            'content_type', row_data.content_type,
            'size_bytes', row_data.size_bytes
        )),
        row_data.created_at
    );
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END;
$$;

DROP TRIGGER IF EXISTS capture_artifact_workspace_event_trigger ON attachment;
CREATE TRIGGER capture_artifact_workspace_event_trigger
AFTER INSERT OR UPDATE OR DELETE ON attachment
FOR EACH ROW EXECUTE FUNCTION capture_artifact_workspace_event();
