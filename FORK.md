# Fork runbook (`Git-on-my-level/multica`)

This is a fork-only operational contract. Preserve it when merging upstream.

## What this fork deliberately carries

- Native OMP runtime/provider support.
- Subscription account pools: one daemon runs Claude Code / Codex under
  several local subscription logins. It uses fresh capacity facts to drain
  the account that resets first, falls back to local run snapshots, and
  fails over on a spent plan window (CLI_AND_DAEMON.md).
- Fork install, update, and repository URLs (`install-fork`), without an
  upstream Homebrew tap.
- Desktop updater feed defaults to `Git-on-my-level/multica` (baked into
  `app-update.yml` at package time). Upstream releases must never be offered
  to fork installs.
- Manual issue-to-PR linking: authorization, canonical HTTPS URLs, explicit
  `close_intent`, member-owned overrides, already-merged evaluation, sibling
  PR gates, and safe unlinking.
- Host-scoped custom runtime profiles: a daemon that does not have a profile's
  command gets no offline runtime row for it, and the machine page shows the
  "waiting for daemon sync" placeholder only for ten minutes after a profile
  changes (R24, 2026-11-16). A broken per-machine path override is still
  recorded. A provisioned host that later loses the command is also silent; its
  old row goes offline through the heartbeat sweeper.
- Migration collision registrations, fork self-host/Tailnet defaults, and
  `.goreleaser.fork.yml` for CLI releases without upstream Homebrew publishing.

## Overlay isolation (keep syncs cheap)

Put fork-only behavior in `*_fork.go` / dedicated files. Do **not** patch
upstream hot files (`daemon.go`, `client.go`, `cache.go`, generated
`models.go`) unless there is no other option.

Current isolated overlays:

| Contract | Overlay |
| --- | --- |
| OMP ACP backend | `server/pkg/agent/omp.go` + `BuiltinRuntimes` ProtocolFamily `omp` |
| Linked-worktree repair | `server/internal/daemon/repocache/cache_fork.go` |
| Manual PR link/handoff | `server/internal/handler/github_handoff.go` + link/unlink in `github.go` |
| Fork install URLs | `MULTICA_GITHUB_*` in handler config / helm / compose |
| Event tables without capture | `9xxx` leftover tables; `9015` drops leftover capture triggers |
| Host-scoped runtime profiles | `server/internal/handler/daemon_profile_fork.go` + one guard in the `DaemonRegister` failed-profile loop; `packages/views/runtimes/components/pending-runtime-fork.ts` + one filter in `runtime-detail-page.tsx` |
| Host-local Devin ACP | `server/pkg/agent/devin.go` + BuiltinRuntimes `devin` (`devin acp`; no cloud Devin; no root `--permission-mode`) |
| ZCode (Z.ai) ACP family | `server/pkg/agent/zcode.go` + probe for `zcode-acp-server`; migration `9018` (port of upstream #6987, which numbers it 469) |
| Subscription account pools | `server/internal/daemon/account_pool_fork.go` (+ hooks in `runTask`), `server/internal/daemon/account_pool_facts_fork.go` (`drain_before_reset` and `soonest_reset` fallback), `server/internal/cli/account_pools_fork.go`, `multica runtime accounts` (`cmd_runtime_accounts_fork.go`), `account`/`accounts` on rate-limit snapshots and the runtime Limits section |

The durable workspace event stream is **retired** (ruling R24, 2026-11-16), not
paused: `multica event list|watch` returns nothing after the 2026-09-13 capture
cutoff, and consumers (agentctl v0.21.13+, skills) read issue and task state
directly. Do not build new consumers on it.

Leftover `workspace_event_*` tables are fine. Do **not** re-enable
`9012`/`415` capture triggers: they take a
`workspace_event_cursor` lock on every issue/task write and deadlock
upstream FailTask/Rerun plus workspace-delete fence tests.

After taking upstream during a sync:

1. Delete fork files that upstream superseded (example: daemon-side
   `chat_suggest.go` — MUL-5573 moved generation server-side).
2. Delete leftover tests that import files upstream deleted.
3. Run `make sqlc` and commit the generated diff. Do not hand-edit
   `server/pkg/db/generated/models.go`.
4. **New fork-only migrations use `9xxx`.** Do not park them in the
   next-free 3-digit slot — upstream will take that number. If an
   existing overlay still sits in 3-digit space, remap it into `9xxx`
   with idempotent SQL and a `forkStemAliases` ledger rewrite; do not
   mix that remap into the sync PR.

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
above. Land the sync as a **merge commit** (not squash — squash leaves GitHub
showing the fork behind upstream). Do not rebase, force-push, mutate tags, or
delete an overlay unless upstream fully supersedes it and focused tests prove
the fork behavior remains. Run the relevant Go/TypeScript tests plus
`make check`, then push the merge normally.

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

## Verify a Multica server activation

Release reads serving identity from the production API for `multica-01` after
the authorized activation. Use the same HTTPS endpoint that workers and members
normally reach. Set `MULTICA_SERVER_URL` to that API's HTTPS base URL and use the
current task token or a member token in `MULTICA_TOKEN`.

```bash
printf 'Authorization: Bearer %s\n' "$MULTICA_TOKEN" |
  curl --fail --silent --show-error --max-time 15 --header @- "${MULTICA_SERVER_URL%/}/api/version"
```

1. Compare the returned full `commit` to the expected deployed SHA. Check that
   `started_at` agrees with the activation window. `built_at`, when present, is
   the build timestamp in UTC.
2. Once both checks pass, post `SERVING: multica-01 <commit> <UTC> ro-prod` on
   the activation issue. Use the response commit and the UTC time of the read
   (`date -u +%Y-%m-%dT%H:%M:%SZ`). Release can collect this evidence with its
   existing API credential.
3. If the route is unavailable, the SHA is missing or mismatched, or the start
   time falls outside the activation window, report the observed result on the
   issue and withhold the serving receipt.

The route uses standard member and agent task authentication and requires no
workspace header or admin scope. Use the full revision in `COMMIT` when
building a backend image from a source snapshot. The release workflow supplies
it automatically.
