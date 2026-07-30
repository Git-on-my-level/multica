---
name: multica-working-on-issues
description: "Use when acting on a Multica issue beyond what the brief covers: PR linking vs close intent, reading a linked PR's real state, metadata keys, status-change side effects, sub-issue todo vs backlog."
user-invocable: false
allowed-tools: Bash(multica *), Bash(git *), Bash(gh *)
---

# Working on Multica issues

Product contracts the runtime brief does not fully encode: PR linking vs close
intent, reading linked-PR state, metadata keys, status side effects, and
sub-issue enqueue behavior.

For building mention links, load `multica-mentioning` instead — not this skill.

Every contract below is traced to source in
`references/working-on-issues-source-map.md`.

## Read the coordination snapshot before mutating

Use the ordinary read-only commands when ownership, duplicate work, or PR
handoff is uncertain:

```bash
multica context --output json
multica issue inspect <issue> --output json
multica issue route --title "..." [--repo <github-clone-url>] [--project <id>] --output json
multica issue url <issue>
```

`context` returns non-secret live workspace/project/resource/repository and
routing-agent context. `inspect` is the server-owned point-in-time view of the
issue, assignee, active/latest runs, native PRs, PR handoff, and safe-action
signals. `route` is advisory only: its decisions (`safe_to_create`,
`use_existing_issue`, `needs_project_selection`, or
`blocked_by_active_owner`) never create, assign, or rerun anything. `issue url`
is the authoritative human-facing web URL; do not assemble a public Multica
domain by hand.

## PR linking and close intent are two distinct contracts

The GitHub webhook runs two separate scans over an incoming PR. They are not the
same gate and they read different fields.

**Linking** scans the PR **title, body, OR branch** for a routable issue key
(`PREFIX-NUMBER`, e.g. `MUL-2759`). Each match writes an issue ↔ PR link row.
This is the link that `multica issue pull-requests` reads back — but see the
reference-only rule below: a key that appears **only** as a bare mention in the
body is linked yet hidden from that list.

```text
MUL-2759: add built-in issue working skill        # title prefix → links, shown
agent/matt/mul-2759-working-on-issues             # branch ref   → links, shown
```

**Close intent** is stricter and is a separate scan over **title or body only —
never the branch**. It fires only for a key placed immediately after a closing
keyword (`Closes` / `Fixes` / `Resolves`, optional `:` then whitespace). That
adjacency is what sets the link row's close-intent flag, the gate that
auto-advances the issue to `done` when the PR merges.

```text
Closes MUL-2759                                    # links AND records close intent
Fixes MUL-2759
Resolves MUL-2759
Fix login MUL-2759                                 # links only — keyword not adjacent
```

Consequence: a bare title prefix or a branch reference links the PR but does not
close the issue on merge. A closing keyword immediately adjacent to the issue key
records close intent; on merge, that close intent can move the linked issue to
`done`.

**Reference-only links (hidden from the PR list).** A key that appears **only**
as a bare mention in the body — no closing keyword, and not in the title or
branch — still writes a link row, but the row is flagged `reference_only` and
**excluded from `multica issue pull-requests`** (and the issue's right-side PR
list in the UI). This keeps passing mentions like `Related MUL-2759` or
`Follow up in MUL-2759` from surfacing an unrelated PR as if it were working on
that issue. To make a PR show up for an issue, put the key in the title, the
branch, or after a closing keyword in the body — not as a loose body reference.

```text
Closes MUL-2759 in the body                        # links and shown
Related to MUL-2759 in the body (no title/branch)  # links but reference_only → hidden
```

### Default for code-changing issue work

When an issue run changes code in a checked-out GitHub repo, the default handoff
is to open or update a PR before posting the final Multica issue comment, unless
the user explicitly asked for a local-only change or no PR. This is a default, not
an unconditional command: if no code changed, say no PR is needed; if PR creation
is blocked by auth, failing tests, or missing remote state, report that blocker
instead of pretending the run is complete.

Use a routable issue key in the PR title, body, or branch so the webhook can link
the PR back to the issue. If the PR should close the issue on merge, put the key
immediately after a closing keyword in the title or body, for example:

```text
MUL-2759: fix login redirect        # links only
Closes MUL-2759                     # links and records close intent
```

In the final issue comment, include the PR URL when a PR exists. If the task did
not produce a PR because no code changed or the user asked not to create one, say
that explicitly.

## Reading a linked PR's real state

When a step depends on PR state, query Multica's link table — do not infer it
from branch names, GitHub search, memory, or `pr_url` metadata (which can be
stale).

```bash
multica issue pull-requests <issue-id> --output json
```

Returns `{"pull_requests": [...], "handoff": {...}}`. `handoff.state` is one of
`missing`, `candidate_detected`, `awaiting_mirror`, `linked`,
`invalid_external_pr`, or `multiple_candidates_needs_review`. A canonical
agent-reported URL can remain `awaiting_mirror` until the connected GitHub App
mirrors it; candidate detection alone never declares close intent or completes
the issue. Each `pull_requests` element exposes:

- `number`, `html_url`, `title`
- `state` — the PR lifecycle as a **single enum**, one of `merged`, `closed`,
  `draft`, `open`. There is no separate `draft` or `merged` boolean in the
  response; the server folds them into `state` (merged wins, then closed, then
  draft, else open).
- `merged_at` — non-null once merged; a second confirmation of `state: merged`.
- `provider` — `github`, `forgejo`, `gitea`, or `gitlab`.
- `mergeable_state` — mirrors GitHub (`clean` / `dirty` surfaced; other values
  round-trip as unknown; retained for compatibility).
- GitHub API snapshot fields: `snapshot_available`, `mergeable`,
  `merge_state_status`, `checks_rollup`, `checks_total`, `checks_passed`,
  `checks_failed`, `checks_running`, `failed_check_names`,
  `snapshot_fetched_at`, and `snapshot_stale`. `snapshot_available == true`
  means the feature is enabled and the snapshot matches the PR's current head.
  Only then does `checks_rollup == null` mean "no checks"; false means the
  snapshot feature is disabled, has not fetched yet, or only has an old head.
- `checks_conclusion` — coarse CI compatibility status: `passed`, `failed`,
  `pending`, or `null`. GitHub derives it from the current API snapshot;
  Forgejo/Gitea/GitLab derive it from webhook commit statuses. Backed by the
  provider-appropriate check counts.

So "is it merged?" is `state == "merged"` (or `merged_at != null`); "is it still
a draft?" is `state == "draft"`; coarse CI status is `checks_conclusion`.

If the command returns no linked PRs after a PR was opened, the link scanner did
not observe a routable issue key in the PR title/body/branch — or the only match
was a bare body mention, which links as `reference_only` and is hidden from this
list (see the reference-only rule above).

## Linking or unlinking a PR by hand

If the webhook scanner did not link a PR (e.g. the issue key is not in its
title/body/branch, or you want close intent without editing the PR body), you
can link it explicitly. Both reuse the same link row, `close_intent` flag, and
close aggregate as the webhook path — there is no separate metadata-only link.

```bash
multica issue pull-requests link   <issue> --url <github-pr-url> [--close-intent]
multica issue pull-requests unlink <issue> --url <github-pr-url>
```

The PR must already be mirrored in the issue's workspace (a connected GitHub App
installation delivered it via webhook); an unmirrored PR returns 404. With
`--close-intent`, linking an already-merged PR advances the issue to `done`
under the native gate (no open/draft sibling, issue not already
`done`/`cancelled`). Unlinking never reopens a `done`/`cancelled` issue.
`pull-requests <id>` (no subcommand) still lists; `link`/`unlink` are
subcommands.

## Metadata: high-signal keys only

Metadata is durable issue state. Reading metadata is safe. Writing a metadata key
is a state mutation and should be tied to an explicit task requirement to record
that state for later readers or runs.

High-signal keys (reuse these names so queries stay consistent):

- `pr_url`
- `pr_number`
- `pipeline_status`
- `deploy_url`
- `external_issue_url`
- `waiting_on`
- `blocked_reason`
- `decision`

Not metadata: logs, summaries, files touched, timestamps, attempt counts,
investigation notes. Those belong in the result comment.

```bash
multica issue metadata set <issue-id> --key pr_url --value <url>
multica issue metadata delete <issue-id> --key <stale-key>
```

`--value` is JSON-parsed by default (bool/number are sniffed); pass `--type
string|number|bool` to force a type.

## Custom properties: typed workflow state

Workspaces may define custom issue properties (Severity, Environment, QA
Status, ...). Properties are the typed, user-visible sibling of metadata:
values are validated against the definition (select options, date format,
http(s) URL), visible in the issue sidebar, and addressed by name.

- Read what exists before writing: `multica property list` shows the catalog;
  `multica issue property list <issue-id>` shows values set on the issue.
- Set values by property name and option name — the CLI translates to ids:

```bash
multica issue property set <issue-id> --name Environment --value staging
multica issue property set <issue-id> --name Platforms --value "iOS,Android"
multica issue property unset <issue-id> --name Environment
```

- A validation error lists the legal options — fix the value and retry.
- Definitions may include an optional catalog icon for visual identification;
  it does not change the property's type or value validation.
- Agents cannot create or edit property definitions (owner/admin humans only).
  If a needed property does not exist, propose it in a comment instead.
- Property vs metadata: if the value is workflow state a human should see and
  filter by, and a definition exists, prefer the property. Metadata stays the
  free-form scratchpad for run state (`pr_url`, `waiting_on`, ...).

## Status changes have server side effects

A status change is not cosmetic — the server enqueues or skips agent work based
on it. These are the contracts, not advice:

- **`backlog`** parks an agent-assigned issue: the assignee is set but no task
  fires. Moving `backlog → todo` (or any non-done/non-cancelled status) enqueues
  the assigned agent then.
- **`in_progress` / `in_review` on assignment runs** are agent-managed CLI
  mutations, not `StartTask` / `CompleteTask` side effects. The assignment
  runtime brief asks ordinary agents for `todo`/`backlog` → `in_progress` then
  `in_review` when they have delivered. For **child/sub-issues under a parent**,
  finishing as **`in_review` or `done`** both count as stage-terminal and can
  wake the parent when the stage barrier closes — see *Child / sub-issue
  completion* below. Squad leaders share the opening `in_progress` step on the
  first assignment turn, keep the **parent** there while members/children work,
  and only move the parent to `in_review` when a later re-trigger confirms the
  overall goal / PR handoff is met.
- **`in_review`** is a valid finish status for children (handoff/results ready)
  **and** the usual PR/handoff status for the issue that owns a PR (parent or
  solo implementer). On a child, entering `in_review` participates in the parent
  stage barrier.
- **`done`**, **`cancelled`**, and **`in_review`** are stage-terminal for parent
  barriers. Entering any of these from a non-terminal status can close a stage
  and wake the parent (system comment + assignee task) once every sibling in
  that stage is stage-terminal. If a PR carries close intent (`Closes MUL-XXXX`),
  merge can advance the linked issue to `done` — you do not also need to flip it
  manually when that path applies.
- **`blocked`** does **not** close a stage, but entering `blocked` **immediately
  wakes the parent** with an attention comment (no barrier wait). Use it for a
  true hard stop so the parent can intervene.
- **`cancelled`** is a terminal, user-driven decision to close the issue. Like
  `done` it enqueues no new agent work, but it does **not** stop tasks already in
  flight — a run in progress keeps going (MUL-4465). To stop a running task,
  cancel the task itself.
- **Failed issue-triggered tasks** may roll an issue from `in_progress` back to
  `todo` when no active task / retry remains — that is the main server-owned
  status write on the agent-run path.

### Child / sub-issue completion (prescriptive)

When **this issue is a child** (has a parent) and its job is a unit of work the
parent will synthesize or gate on:

1. After the final comment with evidence/artifacts, set status to **`in_review`**
   (handoff/results ready) or **`done`**. Either status is stage-terminal and
   can wake the parent when the stage barrier closes.
2. Prefer **`in_review`** when the parent still needs to synthesize or act on
   your results; prefer **`done`** when the child unit is fully closed with
   nothing left for the parent except optional bookkeeping.
3. Use **`blocked`** when you cannot make further progress without external
   input (missing auth, human decision, hard dependency). State the exact
   blocker in the final comment. Entering `blocked` immediately wakes the
   parent (attention path); it does **not** close the stage barrier.
4. Use **`cancelled`** only if this unit of work is abandoned (also
   stage-terminal).

When **creating** sub-issues under a parent you own, put that completion contract
in each child description before assign. Parent gates should treat child
`in_review` or `done` as finished units.

## Sub-issues: `todo` starts work now, `backlog` parks it

On an agent-assigned issue, create status decides whether the assignee fires
immediately. A non-backlog status (e.g. `todo`) enqueues the agent at create
time; `backlog` sets the assignee without triggering.

Parallel children — all start now:

```bash
multica issue create --title "..." --parent <issue-id> --assignee <agent> --status todo
```

Strictly serial children — park later steps, promote one at a time:

```bash
multica issue create --title "Step 2: ..." --parent <issue-id> --assignee <agent> --status backlog
multica issue status <child-id> todo   # promote when the previous step is truly done
```

Creating every serial step as `todo` enqueues the whole chain at once.

### Stages: order sub-issues into barrier groups

`--stage <N>` (N ≥ 1) groups sub-issues under the same parent into ordered
stages. The parent assignee is woken **once, when a whole stage finishes** —
i.e. every sub-issue in the lowest unfinished stage has reached a terminal
status (`done`/`cancelled`/`in_review` — not `blocked`). A completion that does
not close a stage is silent on the handoff path (no stage comment). A sibling
set with **no** stages is one implicit stage, so the parent is woken once when
the *last* sub-issue finishes — not on every child. Separately, a child entering
**`blocked`** wakes the parent immediately for attention without closing the
stage (see *Child / sub-issue completion* above).

Advancement is agent-driven: the server only detects the closed barrier and
wakes the parent assignee, who then decides whether to promote the next stage's
`backlog` sub-issues to `todo`.

```bash
# Stage 1 runs now; later stages parked until promoted
multica issue create --title "Research A" --parent <id> --assignee <agent> --stage 1 --status todo
multica issue create --title "Research B" --parent <id> --assignee <agent> --stage 1 --status todo
multica issue create --title "Build"      --parent <id> --assignee <agent> --stage 2 --status backlog
multica issue create --title "Ship"       --parent <id> --assignee <agent> --stage 3 --status backlog
```

When both Stage 1 sub-issues finish you (the parent assignee) are woken with a
"Stage 1 complete" comment. Inspect the layout, then promote the next stage:

```bash
multica issue children <parent-id>             # sub-issues grouped by stage
multica issue status <stage-2-child-id> todo   # promote when its deps are met
```

Read each sub-issue's description before promoting and only promote items whose
stated dependencies are met; if a description conflicts with the parent's
breakdown, leave it `backlog` and comment to confirm first.

## Incorrect → correct

PR title (link the issue):

```text
Fix login redirect                  # incorrect — no issue key, won't link
MUL-2759: fix login redirect        # correct — links the PR
```

Serial / phased sub-issues (don't start the whole chain at once):

```bash
# incorrect — all fire immediately, no ordering
multica issue create --title "Step 2" --parent <issue-id> --assignee <agent> --status todo
multica issue create --title "Step 3" --parent <issue-id> --assignee <agent> --status todo

# correct — stage them; Stage 1 runs, later stages park and are promoted as
# each stage's barrier closes
multica issue create --title "Step 1" --parent <issue-id> --assignee <agent> --stage 1 --status todo
multica issue create --title "Step 2" --parent <issue-id> --assignee <agent> --stage 2 --status backlog
multica issue create --title "Step 3" --parent <issue-id> --assignee <agent> --stage 3 --status backlog
```

Child finish status (stage barrier / parent wake):

```text
# correct — handoff/results ready; stage-terminal (wakes parent when barrier closes)
multica issue status <child-id> in_review

# correct — child unit fully closed; also stage-terminal
multica issue status <child-id> done

# correct for a true hard stop — immediate parent attention wake (does not close stage)
multica issue status <child-id> blocked
```

## References

`references/working-on-issues-source-map.md` — accurate `file:line` for every
contract above: the `pull-requests` CLI and route, the PR response field list,
`derivePRState`, the two-path link (`extractIdentifiers`) vs close-intent
(`extractClosingIdentifiers`) proof, the backlog enqueue lines, child-done
notify, the stage column / `stageBarrierClosed` barrier and the `--stage` /
`issue children` CLI, and the metadata CLI. Re-derive before depending on an
exact line.
