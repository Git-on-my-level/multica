-- Fork overlay: add ZCode (Z.ai, driven over ACP through the zcode-acp-server
-- bridge) as a first-party protocol family (port of upstream PR #6987, which
-- numbers this 469 — the fork namespace is 90xx to avoid colliding with a
-- future upstream sync).
-- Idempotent: drop + add the full current whitelist (9016/9017 set: post-sync
-- families plus omp + devin + codearts + zeroclaw) plus zcode. NOT VALID
-- preserves historical-row tolerance.
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
        'devin',
        'zcode'
    )) NOT VALID;
