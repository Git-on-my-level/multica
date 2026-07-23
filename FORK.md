# Fork runbook (`Git-on-my-level/multica`)

This is a fork-only operational contract. Preserve it when merging upstream.

## What this fork deliberately carries

- Native OMP runtime/provider support.
- Fork install, update, and repository URLs (`install-fork`), without an
  upstream Homebrew tap.
- Manual issue-to-PR linking: authorization, canonical HTTPS URLs, explicit
  `close_intent`, member-owned overrides, already-merged evaluation, sibling
  PR gates, and safe unlinking.
- Migration collision registrations, fork self-host/Tailnet defaults, and
  `.goreleaser.fork.yml` for CLI releases without upstream Homebrew publishing.

## Safe upstream sync

Fetch branch heads only. Fork and upstream reuse semver tag names for different
objects, so `git fetch --all --tags` can fail and must never be used to force or
overwrite fork tags. Inspect upstream tags without importing them if needed:

```bash
git remote add upstream https://github.com/multica-ai/multica.git  # once
git fetch origin main
git fetch upstream main
git ls-remote --tags upstream
git log --oneline upstream/main..origin/main
git rev-list --left-right --count origin/main...upstream/main
git switch main
git merge --no-ff upstream/main -m 'merge: sync upstream main into fork'
```

Resolve conflicts by retaining current upstream behavior and the fork contracts
above. Do not rebase, squash, force-push, mutate tags, or delete an overlay
unless upstream fully supersedes it and focused tests prove the fork behavior
remains. Run the relevant Go/TypeScript tests plus `make check`, then push the
merge normally.

## Releases

Fork tag automation continues to publish CLI archives through
`.goreleaser.fork.yml` and may publish fork GHCR backend/web images. It must
not publish any macOS, Linux, or Windows Desktop artifact.

Desktop releases are manual arm64-only releases from David's Mac, signed,
notarized, stapled, and verified before upload. The public deliverable is the
arm64 **DMG only**: do not publish or wait for a ZIP, updater metadata, CLI,
containers, Windows, Linux, or x64 artifacts unless explicitly requested.
Use [the macOS release guide](apps/desktop/MACOS_RELEASE.md). It defaults to
the `multica-notary` login-Keychain profile and `--publish never`, then verifies
the resulting DMG before manually creating the matching GitHub Release. Leave
`CSC_NAME` unset so electron-builder auto-discovers the signing identity;
`CSC_IDENTITY_AUTO_DISCOVERY=false` belongs only to non-distributed smoke tests.

Pushing a version tag triggers the generic Release workflow, including unrelated
CLI/container work. That workflow is not a DMG gate. After local DMG checks, use
`gh release create --target <reviewed-sha>` to create the missing lightweight
tag and DMG-only release through GitHub's Release API, then verify
the remote tag resolves to that exact SHA. Do not push the tag separately or
wait for unrelated jobs.

Never rewrite a failed tag or release. Issue a corrected version after fixing
the cause.

## Fork install and self-hosting

Set these environment values on fork-hosted backends so UI install/update links
target the fork:

```bash
MULTICA_GITHUB_REPO=Git-on-my-level/multica
MULTICA_GITHUB_BRANCH=main
```

Use `scripts/install-fork.sh` for the curl entrypoint; it skips upstream
Homebrew and persists the fork repository for `multica update`. Fork GHCR
namespaces are lowercase (`ghcr.io/git-on-my-level/...`).
