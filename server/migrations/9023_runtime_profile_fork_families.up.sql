-- Restore the complete upstream and fork family union after overlay migrations.
ALTER TABLE runtime_profile DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check;
ALTER TABLE runtime_profile ADD CONSTRAINT runtime_profile_protocol_family_check
CHECK (protocol_family IN ('claude', 'codebuddy', 'codex', 'copilot', 'opencode', 'codearts', 'deveco', 'openclaw', 'hermes', 'pi', 'cursor', 'kimi', 'reasonix', 'dsh', 'kiro', 'antigravity', 'qoder', 'qoderclicn', 'traecli', 'grok', 'qwen', 'qwenpaw', 'mcode', 'muse', 'dim', 'devin', 'zeroclaw', 'omp', 'museapp', 'zcode')) NOT VALID;
