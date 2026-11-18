ALTER TABLE agent ADD COLUMN codex_model_admission_rejection jsonb;

-- Runtime picker catalogs are UI projections and can contain fallback data.
-- Compare a provider rejection against the configuration that was dispatched.
CREATE VIEW codex_model_admission_config AS
SELECT a.id AS agent_id, r.id AS runtime_id, a.model,
       jsonb_build_array(a.model, r.metadata->>'version',
                         a.runtime_config->>'codex_model_admission_revision') AS configuration
FROM agent a
JOIN agent_runtime r ON r.id = a.runtime_id
WHERE r.provider = 'codex' AND a.model <> '';
