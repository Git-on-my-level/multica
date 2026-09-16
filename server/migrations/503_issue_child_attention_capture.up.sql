-- Capture in the status write's transaction, including batch and non-HTTP
-- writers. A server crash after that commit cannot lose the handoff.
CREATE FUNCTION capture_issue_child_attention() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    effective_status TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        DELETE FROM issue_child_attention WHERE child_id = OLD.id OR parent_id = OLD.id;
        RETURN OLD;
    END IF;
    IF NEW.status IS NOT DISTINCT FROM OLD.status
       AND NEW.parent_issue_id IS NOT DISTINCT FROM OLD.parent_issue_id THEN
        RETURN NEW;
    END IF;
    DELETE FROM issue_child_attention WHERE child_id = NEW.id;
    effective_status := issue_effective_status(NEW.workspace_id, NEW.status);
    IF NEW.parent_issue_id IS NOT NULL
       AND NEW.status IS DISTINCT FROM OLD.status
       AND effective_status IN ('in_review', 'blocked')
       AND effective_status IS DISTINCT FROM issue_effective_status(OLD.workspace_id, OLD.status)
       AND EXISTS (
           SELECT 1 FROM issue parent
           WHERE parent.id = NEW.parent_issue_id AND parent.workspace_id = NEW.workspace_id
             AND issue_effective_status(parent.workspace_id, parent.status) NOT IN ('backlog', 'done', 'cancelled', 'triage')
             AND parent.assignee_type IS DISTINCT FROM 'member'
       ) THEN
        INSERT INTO issue_child_attention (child_id, parent_id, workspace_id, status, available_at)
        VALUES (NEW.id, NEW.parent_issue_id, NEW.workspace_id, effective_status, now() + interval '1 second');
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER issue_child_attention_capture
AFTER UPDATE OF status, parent_issue_id OR DELETE ON issue
FOR EACH ROW EXECUTE FUNCTION capture_issue_child_attention();
