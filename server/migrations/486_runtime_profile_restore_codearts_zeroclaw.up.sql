-- Converge databases that already applied leftover 485_runtime_profile_add_devin.
-- That leftover CHECK dropped codearts (441) and zeroclaw (403). The rewritten
-- 485 put those families back, but the runner keys schema_migrations on the
-- full stem and never re-runs a recorded version. This new stem is what actually
-- rewrites the already-applied constraint.
--
-- Idempotent: drop + add the full current whitelist (post-sync families plus
-- omp + devin). NOT VALID preserves historical-row tolerance.
ALTER TABLE runtime_profile DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check;

ALTER TABLE runtime_profile ADD CONSTRAINT runtime_profile_protocol_family_check
    CHECK (protocol_family IN (
        'claude',
        'codebuddy',
        'codex',
        'copilot',
        'opencode',
        'codearts',
        'openclaw',
        'hermes',
        'pi',
        'cursor',
        'kimi',
        'reasonix',
        'dsh',
        'kiro',
        'antigravity',
        'qoder',
        'qoderclicn',
        'traecli',
        'deveco',
        'grok',
        'qwen',
        'qwenpaw',
        'mcode',
        'dim',
        'zeroclaw',
        'omp',
        'devin'
    )) NOT VALID;
