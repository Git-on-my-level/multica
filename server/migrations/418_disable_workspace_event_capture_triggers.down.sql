-- Recreate leftover capture triggers if this overlay is rolled back.
-- Bodies already exist from 415_workspace_event_capture.
DROP TRIGGER IF EXISTS capture_issue_workspace_event_trigger ON issue;
CREATE TRIGGER capture_issue_workspace_event_trigger
AFTER INSERT OR UPDATE OR DELETE ON issue
FOR EACH ROW EXECUTE FUNCTION capture_issue_workspace_event();

DROP TRIGGER IF EXISTS capture_comment_workspace_event_trigger ON comment;
CREATE TRIGGER capture_comment_workspace_event_trigger
AFTER INSERT OR UPDATE OR DELETE ON comment
FOR EACH ROW EXECUTE FUNCTION capture_comment_workspace_event();

DROP TRIGGER IF EXISTS capture_task_workspace_event_trigger ON agent_task_queue;
CREATE TRIGGER capture_task_workspace_event_trigger
AFTER INSERT OR UPDATE OF status OR DELETE ON agent_task_queue
FOR EACH ROW EXECUTE FUNCTION capture_task_workspace_event();

DROP TRIGGER IF EXISTS capture_run_workspace_event_trigger ON autopilot_run;
CREATE TRIGGER capture_run_workspace_event_trigger
AFTER INSERT OR UPDATE OF status OR DELETE ON autopilot_run
FOR EACH ROW EXECUTE FUNCTION capture_run_workspace_event();

DROP TRIGGER IF EXISTS capture_artifact_workspace_event_trigger ON attachment;
CREATE TRIGGER capture_artifact_workspace_event_trigger
AFTER INSERT OR UPDATE OR DELETE ON attachment
FOR EACH ROW EXECUTE FUNCTION capture_artifact_workspace_event();
