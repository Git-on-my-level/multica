-- Fork overlay: leftover workspace-event capture triggers take a
-- workspace_event_cursor row lock on every issue/task/comment write.
-- That lock is orthogonal to upstream task-owner fencing and deadlocks
-- FailTask+Rerun plus status-only updates during workspace teardown tests.
-- Keep the tables for explicit event APIs; stop implicit capture.
DROP TRIGGER IF EXISTS capture_issue_workspace_event_trigger ON issue;
DROP TRIGGER IF EXISTS capture_comment_workspace_event_trigger ON comment;
DROP TRIGGER IF EXISTS capture_task_workspace_event_trigger ON agent_task_queue;
DROP TRIGGER IF EXISTS capture_run_workspace_event_trigger ON autopilot_run;
DROP TRIGGER IF EXISTS capture_artifact_workspace_event_trigger ON attachment;
