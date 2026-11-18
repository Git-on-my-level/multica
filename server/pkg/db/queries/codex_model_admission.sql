-- name: LockCodexModelAdmissionAgentForTask :exec
-- Use the same agent row lock as claimTask, before the terminal task write.
SELECT a.id
FROM agent a
JOIN agent_task_queue t ON t.agent_id = a.id
WHERE t.id = @task_id
  AND t.context ? 'codex_model_admission'
FOR UPDATE OF a;

-- name: RecordCodexModelAdmissionRejection :execrows
-- Match the provider's rejected model and the original dispatch snapshot.
-- A late failure from a superseded configuration must not block the new one.
UPDATE agent a
SET codex_model_admission_rejection = jsonb_build_array(c.runtime_id, c.configuration)
FROM codex_model_admission_config c
JOIN agent_task_queue t ON t.agent_id = c.agent_id AND t.runtime_id = c.runtime_id
WHERE a.id = c.agent_id
  AND t.id = @task_id
  AND t.started_at IS NOT NULL
  AND c.model = @rejected_model::text
  AND c.configuration = t.context->'codex_model_admission'
  AND a.codex_model_admission_rejection IS DISTINCT FROM jsonb_build_array(c.runtime_id, c.configuration);

-- name: IsCodexModelAdmissionRejected :one
SELECT EXISTS (
    SELECT 1
    FROM codex_model_admission_config c
    JOIN agent a ON a.id = c.agent_id
    WHERE c.agent_id = $1 AND c.runtime_id = $2
      AND a.codex_model_admission_rejection = jsonb_build_array(c.runtime_id, c.configuration)
);

-- name: GetCodexAgentForClaimDelivery :one
-- Read the launch agent and its admission configuration in the same snapshot.
-- A legacy or reclaimed claim with a different configuration must be claimed
-- again before its payload can use the new one.
SELECT sqlc.embed(a),
       c.configuration IS NOT DISTINCT FROM (sqlc.narg(claim_context)::jsonb->'codex_model_admission') AS admission_matches
FROM agent a
LEFT JOIN codex_model_admission_config c ON c.agent_id = a.id AND c.runtime_id = @runtime_id
WHERE a.id = @agent_id;
