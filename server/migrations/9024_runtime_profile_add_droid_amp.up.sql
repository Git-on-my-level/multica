-- Fork overlay: 9023 re-created the family CHECK after upstream's 652 (droid)
-- and 656 (amp) ran, dropping both. Restore the full upstream and fork union.
ALTER TABLE runtime_profile DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check;
ALTER TABLE runtime_profile ADD CONSTRAINT runtime_profile_protocol_family_check
CHECK (protocol_family IN ('claude', 'codebuddy', 'codex', 'copilot', 'opencode', 'codearts', 'deveco', 'openclaw', 'hermes', 'pi', 'cursor', 'kimi', 'reasonix', 'dsh', 'kiro', 'antigravity', 'qoder', 'qoderclicn', 'traecli', 'grok', 'qwen', 'qwenpaw', 'droid', 'amp', 'mcode', 'muse', 'dim', 'devin', 'zeroclaw', 'omp', 'museapp', 'zcode')) NOT VALID;
