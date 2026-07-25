Warning: truncated output (original token count: 48950)
Total output lines: 4512

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/attribution"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/featureflags"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/runtimeapps"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/redact"
	"github.com/multica-ai/multica/server/pkg/skillbundle"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

type TaskService struct {
	Queries   *db.Queries
	TxStarter TxStarter
	Hub       *realtime.Hub
	Bus       *events.Bus
	Analytics analytics.Client
	Metrics   *obsmetrics.BusinessMetrics
	Wakeup    TaskWakeupNotifier
	// FeatureFlags is the server-side toggle router. Nil is valid and returns
	// each call site's default.
	FeatureFlags *featureflag.Service
	// EmptyClaim caches "this runtime has no queued task" so the daemon
	// poll path can skip a Postgres scan on the steady-state empty case.
	// Optional — a nil cache disables the fast path and every claim
	// goes through the DB. Wired in router.go from the shared Redis
	// client.
	EmptyClaim *EmptyClaimCache
	// Composio computes the per-task MCP overlay (Stage 3 of the Composio
	// epic, MUL-3721) — the integration's "current user's connected apps
	// → MCP session URL" hook called from each Enqueue* path. Optional: a
	// nil ComposioOverlayBuilder turns the overlay step into a no-op so
	// every Multica deployment that hasn't enabled Composio behaves
	// exactly as before. Wired in router.go after composiointeg.NewService
	// succeeds; the concrete type is *composio.Service.
	Composio ComposioOverlayBuilder

	analyticsContextMu    sync.Mutex
	analyticsContextCache map[string]analytics.TaskContext
	analyticsContextOrder []string
}

// ComposioOverlayBuilder is the seam TaskService uses to build the per-task
// MCP overlay at enqueue time. Implemented by
// internal/integrations/composio.Service.BuildTaskOverlay; tests provide an
// inline fake so they don't have to spin a fake Composio SDK.
//
// Contract: a zero MCPOverlayResult means "no overlay for this run" — covers
// all gates the implementation enforces (no owner / empty allowlist / empty
// intersection with active connections / empty session URL). Any non-empty
// MCPOverlay is the exact value to store in agent_task_queue.runtime_mcp_overlay;
// ConnectedApps is non-secret metadata to store alongside it for daemon brief
// injection. A non-nil error is surfaced to the caller but treated as
// best-effort — failed overlay computation must not fail the enqueue.
//
// agent is passed by value so the builder can inspect OwnerID and
// ComposioToolkitAllowlist without re-querying the DB; every enqueue path
// already loaded the agent for runtime/archive checks, so passing it is
// free and avoids a second GetAgent round-trip in the hot path.
type ComposioOverlayBuilder interface {
	BuildTaskOverlay(ctx context.Context, originatorUserID pgtype.UUID, agent db.Agent) (runtimeapps.MCPOverlayResult, error)
}

type TaskWakeupNotifier interface {
	NotifyTaskAvailable(runtimeID, taskID string)
}

// triggerSummaryMaxLen caps the snapshot length so the row stays cheap to
// transmit (it ends up in every task list response). 200 is enough for a
// recognisable preview of a one-paragraph comment.
const triggerSummaryMaxLen = 200

// truncateForSummary returns s shortened to maxRunes, with a trailing
// `…` when truncated. Operates on runes (not bytes) so multibyte characters
// — Chinese / emoji — count as one each. Strips surrounding whitespace
// first so a leading newline doesn't waste budget.
func truncateForSummary(s string, maxRunes int) string {
	// strings.Builder + Grow avoids the O(N²) realloc cycle of `+=` in
	// a loop. Grow uses byte length, which is an upper bound for the
	// rune-equivalent output (replacing \n/\r/\t with space is byte-equal
	// for ASCII whitespace), so we never reallocate.
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\n', '\r', '\t':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	rs := []rune(strings.TrimSpace(b.String()))
	if len(rs) <= maxRunes {
		return string(rs)
	}
	return string(rs[:maxRunes]) + "…"
}

// maxSynthesizedFallbackCommentRunes bounds the completion-fallback comment that
// CompleteTask synthesizes from a task's final output when the agent left no
// comment of its own during the run. A real final assistant message is at most
// a few thousand words; anything larger is a runaway raw-stream dump — every
// streamed text delta concatenated together plus a literal `tool call` line per
// tool_use event — which some runtimes/providers emit as the task's Output on
// long, tool-heavy runs. Such a dump (observed at 190–264 KB) must never be
// posted, even partially, to the issue thread (GH #5455).
const maxSynthesizedFallbackCommentRunes = 8000

const oversizedFallbackCommentNotice = "This task completed, but its output was too large to post safely. The raw output was not posted. Review the task in this issue's Execution log."

// truncateFallbackCommentBody bounds a synthesized completion-fallback comment
// body. Unlike truncateForSummary (which flattens newlines for a one-line row
// snapshot), it preserves genuine final messages below the cap verbatim. Output
// above the cap is untrusted: the reported failure mode puts process narration
// and tool traces at the head, so retaining any excerpt can expose execution
// details and still discard the final answer. Replace the entire body with a
// fixed notice instead. Callers pass the already-redacted body.
func truncateFallbackCommentBody(body string, maxRunes int) string {
	if utf8.RuneCountInString(body) <= maxRunes {
		return body
	}
	return oversizedFallbackCommentNotice
}

const (
	taskAnalyticsContextCacheMax = 4096
	// claimResponseRecoveryWindow must exceed daemon client.Timeout for
	// /tasks/claim (30s) plus /tasks/{id}/start (30s) plus scheduling slack.
	// Longer pre-start work is protected by prepareLeaseDuration instead of
	// stretching this global crash-recovery window.
	claimResponseRecoveryWindow = 90 * time.Second
	prepareLeaseDuration        = 45 * time.Second
)

// buildCommentTriggerSummary fetches the comment content and truncates
// it for storage on the task row. Returns an invalid pgtype.Text when
// the comment is missing (deleted / wrong workspace / etc) so the column
// stays NULL — front-end falls back to a structural label in that case.
//
// workspaceID scopes the fetch to the task's own workspace: the summary is
// later returned in claim / task-history responses, so a foreign comment UUID
// reaching an enqueue/merge path must NOT leak another workspace's text even in
// truncated form (MUL-4252).
func (s *TaskService) buildCommentTriggerSummary(ctx context.Context, workspaceID, commentID pgtype.UUID) pgtype.Text {
	if !commentID.Valid {
		return pgtype.Text{}
	}
	comment, err := s.Queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{
		ID:          commentID,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		return pgtype.Text{}
	}
	summary := truncateForSummary(comment.Content, triggerSummaryMaxLen)
	if summary == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: summary, Valid: true}
}

// ResolveOriginatorFromTriggerComment is the exported wrapper used by the
// comment-merge path (MUL-4195) to compute the top-of-chain human originator
// for a newly-arrived comment, so a merge can be gated on the originator being
// unchanged. workspaceID scopes the comment lookup to the task's workspace
// (MUL-4252). See resolveOriginatorFromTriggerComment for the chain rules.
func (s *TaskService) ResolveOriginatorFromTriggerComment(ctx context.Context, workspaceID, commentID pgtype.UUID) pgtype.UUID {
	return s.resolveOriginatorFromTriggerComment(ctx, workspaceID, commentID)
}

// AttributionForMergedComment resolves the FULL attribution snapshot for a comment
// being coalesced into an already-queued task (MUL-4302). A merge re-attributes the
// run to the newly-arrived comment's human, so the whole snapshot — source, evidence,
// delegation lineage, and both person columns — must move together as one
// attribution.Result; re-stamping only the person columns would leave a run showing
// B accountable while still pointing at A's stale source / evidence / level. isMention
// picks the agent-authored label (delegation for a mention / thread-parent, otherwise
// comment_source), matching the fresh-enqueue routing.
//
// The merge re-opens the same fail-closed decision the original enqueue faced: a merge
// swaps the effective trigger, responsible human, and evidence to the NEW comment, so
// "the enqueue already checked" does not carry over. It runs the comment through
// applyAttributionFallback — the identical fail-closed gate the fresh-enqueue path uses
// — and returns ErrAttributionFailClosed when the new comment cannot be attributed
// precisely and the workspace forbids the owner_fallback degrade. The caller must then
// REFUSE the merge and keep the original (precisely-attributed) task snapshot rather
// than re-stamp a queued run to a degraded owner_fallback (Elon must-fix).
func (s *TaskService) AttributionForMergedComment(ctx context.Context, workspaceID, commentID pgtype.UUID, isMention bool, agent db.Agent) (attribution.Result, error) {
	agentAuthoredSource := attribution.SourceCommentSource
	if isMention {
		agentAuthoredSource = attribution.SourceDelegation
	}
	attr := s.attributionFromTriggerComment(ctx, workspaceID, commentID, agentAuthoredSource)
	return s.applyAttributionFallback(ctx, attr, agent)
}

// BuildCommentTriggerSummary is the exported wrapper used by the comment-merge
// path (MUL-4195) to refresh a coalesced task's trigger_summary to the newest
// trigger comment's snapshot. workspaceID scopes the lookup (MUL-4252).
func (s *TaskService) BuildCommentTriggerSummary(ctx context.Context, workspaceID, commentID pgtype.UUID) pgtype.Text {
	return s.buildCommentTriggerSummary(ctx, workspaceID, commentID)
}

// BuildRuntimeMCPOverlayForMerge recomputes the Composio MCP overlay +
// connected-app metadata for (originatorUserID, agent), used when a merge
// re-stamps a coalesced task's originator (MUL-4195 review must-fix #1). The
// overlay is a pure function of (originator, agent); re-stamping it alongside
// originator_user_id keeps the coalescing run's connected-app capabilities and
// audit attribution consistent with the latest trigger comment's originator
// instead of the task's original one. Fails soft to empty (same as the enqueue
// path) so a transient Composio hiccup never blocks the merge.
func (s *TaskService) BuildRuntimeMCPOverlayForMerge(ctx context.Context, originatorUserID pgtype.UUID, agent db.Agent) (overlay, connectedApps []byte) {
	data := s.buildRuntimeMCPOverlay(ctx, originatorUserID, agent)
	return data.Overlay, data.ConnectedApps
}

func NewTaskService(q *db.Queries, tx TxStarter, hub *realtime.Hub, bus *events.Bus, wakeups ...TaskWakeupNotifier) *TaskService {
	var wakeup TaskWakeupNotifier
	if len(wakeups) > 0 {
		wakeup = wakeups[0]
	}
	return &TaskService{Queries: q, TxStarter: tx, Hub: hub, Bus: bus, Wakeup: wakeup}
}

var trivialDoneMarkers = []string{
	"done",
	"готово",
	"готова",
	"сделано",
	"完成",
	"完了",
}

func isTrivialDoneOutput(output string) bool {
	normalized := strings.TrimSpace(strings.ToLower(output))
	normalized = strings.Trim(normalized, ".!！。… ")
	for _, marker := range trivialDoneMarkers {
		if normalized == marker {
			return true
		}
	}
	return false
}

func (s *TaskService) captureTaskQueued(ctx context.Context, task db.AgentTaskQueue) {
	if s.Metrics != nil {
		source, runtimeMode, _ := s.taskMetricsContext(ctx, task)
		s.Metrics.RecordTaskEnqueued(source, runtimeMode)
	}
}

type runtimeMCPOverlayData struct {
	Overlay       json.RawMessage
	ConnectedApps json.RawMessage
}

// buildRuntimeMCPOverlay computes the optional per-task Composio MCP overlay.
// Enqueue paths call this BEFORE inserting the queued row so the daemon cannot
// claim a task during the network round-trip to Composio and miss the overlay.
func (s *TaskService) buildRuntimeMCPOverlay(ctx context.Context, originatorUserID pgtype.UUID, agent db.Agent) runtimeMCPOverlayData {
	if s == nil || s.Composio == nil {
		return runtimeMCPOverlayData{}
	}
	if !featureflags.ComposioMCPAppsEnabled(ctx, s.FeatureFlags) {
		return runtimeMCPOverlayData{}
	}
	result, err := s.Composio.BuildTaskOverlay(ctx, originatorUserID, agent)
	if err != nil {
		slog.Warn("runtime mcp overlay: BuildTaskOverlay failed; task will run without composio overlay",
			"originator_user_id", util.UUIDToString(originatorUserID),
			"agent_id", util.UUIDToString(agent.ID),
			"error", err,
		)
		return runtimeMCPOverlayData{}
	}
	if len(result.MCPOverlay) == 0 {
		slog.Debug("runtime mcp overlay: no composio overlay for task",
			"originator_user_id", util.UUIDToString(originatorUserID),
			"agent_id", util.UUIDToString(agent.ID),
		)
		return runtimeMCPOverlayData{}
	}
	data := runtimeMCPOverlayData{Overlay: result.MCPOverlay}
	if len(result.ConnectedApps) > 0 {
		raw, err := json.Marshal(result.ConnectedApps)
		if err != nil {
			slog.Warn("runtime mcp overlay: marshal connected app metadata failed",
				"originator_user_id", util.UUIDToString(originatorUserID),
				"agent_id", util.UUIDToString(agent.ID),
				"error", err,
			)
			return data
		}
		data.ConnectedApps = raw
	}
	return data
}

// resolveOriginatorFromTriggerComment returns the top-of-chain HUMAN user
// id for a comment that triggered an Enqueue* path. The chain rules
// (MUL-3869):
//
//   - trigger comment authored by a member → originator = author_id (that
//     member IS the top-of-chain human).
//   - trigger comment authored by an agent → read the parent task via
//     comment.source_task_id and inherit its originator_user_id. This is
//     the load-bearing case for agent fan-out: agent A @-mentions agent B,
//     comment author is A, but we MUST surface the human who originally
//     told A to run, not lose the originator at the first agent hop.
//   - missing comment / unknown source task / NULL parent originator →
//     invalid pgtype.UUID. BuildTaskOverlay treats that as "no overlay"
//     (gate 1).
//
// A nil receiver / nil Queries falls through to invalid so unit-test
// setups that don't wire a DB stay safe. workspaceID scopes the comment lookup
// to the task's workspace so a foreign comment UUID cannot resolve an
// originator from another tenant (MUL-4252).
func (s *TaskService) resolveOriginatorFromTriggerComment(ctx context.Context, workspaceID, commentID pgtype.UUID) pgtype.UUID {
	// The originator VALUE is independent of the agent-authored source label, so
	// any label works here; comment_source is passed only as a placeholder.
	return s.attributionFromTriggerComment(ctx, workspaceID, commentID, attribution.SourceCommentSource).UserID
}

// attributionFromTriggerComment resolves the full attribution (accountable
// human + provenance label + delegation lineage + evidence) for a
// comment-triggered run. It performs the DB reads and hands the gathered facts
// to the pure attribution.ClassifyComment rules so the classification stays
// side-effect-free and unit-tested. The returned UserID is byte-identical to
// the pre-MUL-4302 originator resolution, so authorization behavior (Composio
// overlay, canInvokeAgent A2A gate) is unchanged. workspaceID scopes the comment
// lookup to the task's workspace (MUL-4252).
//
// agentAuthoredSource selects the label for an agent-authored trigger comment:
// attribution.SourceCommentSource for the issue-assignee-reacting path,
// attribution.SourceDelegation for an explicit mention / thread-parent /
// squad-leader path.
func (s *TaskService) attributionFromTriggerComment(ctx context.Context, workspaceID, commentID pgtype.UUID, agentAuthoredSource attribution.Source) attribution.Result {
	if s == nil || s.Queries == nil || !commentID.Valid {
		return attribution.Result{Source: attribution.SourceUnattributed}
	}
	comment, err := s.Queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{
		ID:          commentID,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		return attribution.Result{Source: attribution.SourceUnattributed}
	}
	return s.attributionFromComment(ctx, comment, agentAuthoredSource)
}

// attributionFromComment classifies a run from an already-loaded trigger comment,
// so a caller that already has the row (e.g. to inspect author_type) does not
// re-read it. Kept byte-identical to the inline logic attributionFromTriggerComment
// used before, so authorization behavior is unchanged.
func (s *TaskService) attributionFromComment(ctx context.Context, comment db.Comment, agentAuthoredSource attribution.Source) attribution.Result {
	facts := attribution.CommentFacts{
		CommentID:  comment.ID,
		AuthorType: comment.AuthorType,
		AuthorID:   comment.AuthorID,
	}
	// For an agent-authored comment, walk comment.source_task_id → parent task →
	// parent.originator_user_id (set by every agent comment-write path since
	// migration 120). A NULL/missing source task leaves ParentOriginator
	// invalid, which ClassifyComment maps to unattributed.
	if comment.AuthorType == "agent" && comment.SourceTaskID.Valid {
		facts.SourceTaskID = comment.SourceTaskID
		if parent, err := s.Queries.GetAgentTask(ctx, comment.SourceTaskID); err == nil {
			facts.ParentOriginator = parent.OriginatorUserID
			facts.ParentAccountable = parent.AccountableUserID
		}
	}
	return attribution.ClassifyComment(facts, agentAuthoredSource)
}

// resolveOriginatorForIssueTask returns the top-of-chain human for issue-backed
// dispatches. Comment-triggered runs keep the existing comment-chain semantics;
// direct issue assignment/creation falls back to the issue's member creator.
// Agent-created issues that carry an explicit task-origin link — quick_create
// (daemon quick-create flow) or agent_create (an agent's ordinary `issue
// create`, MUL-4305) — inherit that origin task's originator, since origin_id
// points at the agent_task_queue row that created the issue. Other
// agent/system origins, including autopilot, deliberately remain unattributed.
func (s *TaskService) resolveOriginatorForIssueTask(ctx context.Context, issue db.Issue, triggerCommentID pgtype.UUID) pgtype.UUID {
	return s.attributionForIssueTask(ctx, issue, triggerCommentID, attribution.SourceCommentSource, pgtype.UUID{}).UserID
}

// attributionForIssueTask resolves the full attribution for an issue-backed
// enqueue. Comment-triggered runs keep the comment-chain semantics; direct
// assignment/creation falls back to the issue's member creator; agent-created
// quick-create issues inherit the origin task's human as a delegation. The
// accountable-human value is byte-identical to resolveOriginatorForIssueTask,
// which now delegates here — so there is a single source of truth and
// authorization is unaffected. agentAuthoredSource labels the agent-authored
// trigger comment case (see attributionFromTriggerComment).
func (s *TaskService) attributionForIssueTask(ctx context.Context, issue db.Issue, triggerCommentID pgtype.UUID, agentAuthoredSource attribution.Source, actorUserID pgtype.UUID) attribution.Result {
	// A direct member action is the accountable human AND originator, ahead of any
	// trigger comment, origin, or rule (MUL-4302 §4/§5). This covers assign/promote,
	// a manual autopilot trigger, and a manual rerun — the last of which may INHERIT
	// a triggerCommentID for the daemon's prompt context, but must still attribute to
	// the member who clicked rerun, not the original comment's human. So the actor is
	// checked before the trigger-comment / origin branches.
	if actorUserID.Valid {
		return attribution.ClassifyDirect(attribution.DirectFacts{IssueID: issue.ID, ActorUserID: actorUserID})
	}
	if triggerCommentID.Valid {
		if s == nil || s.Queries == nil {
			return attribution.Result{Source: attribution.SourceUnattributed}
		}
		// workspace-scoped so a foreign comment UUID cannot resolve a human from
		// another tenant (MUL-4252).
		comment, err := s.Queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{
			ID:          triggerCommentID,
			WorkspaceID: issue.WorkspaceID,
		})
		if err != nil {
			return attribution.Result{Source: attribution.SourceUnattributed}
		}
		// A member/agent trigger comment resolves the human (direct_human / delegation
		// / comment_source). A SYSTEM-authored comment — today the Stage-completion
		// child-done comment (issue_child_done.go), which wakes the parent assignee
		// and threads no actor — carries no human and is not part of any delegation
		// chain. Classifying it would degrade straight to owner_fallback (the agent's
		// own owner), which is wrong for a Stage cascade: the woken run should be
		// accountable to whoever caused the PARENT issue to exist. So for a system
		// comment we skip the comment branch and fall through to the parent issue's
		// own provenance below — the same creator / agent_create-origin /
		// autopilot-origin chain a direct enqueue resolves — reaching owner_fallback
		// only if that provenance itself has no human (MUL-4302; raised by Bohan on
		// the stage-cascade fallback).
		if comment.AuthorType != "system" {
			return s.attributionFromComment(ctx, comment, agentAuthoredSource)
		}
	}
	// Autopilot-origin issues (origin_id is the autopilot id) from a schedule /
	// webhook trigger: no human authorized the run, so originator stays NULL, but it
	// is accountable to the human currently RESPONSIBLE for the firing trigger's
	// effective config (creator, then last substantive editor) — trigger_owner
	// (MUL-4302; Elon must-fix), degrading to the rule publisher when no such member
	// is recoverable. Resolved the same way run_only dispatch resolves
	// it, so both autopilot execution modes attribute identically. (A manual trigger
	// carries an actor and is already handled above.) The issue only stores the
	// autopilot id, so bridge issue → active run → trigger_id to find the trigger.
	if s != nil && s.Queries != nil && issue.OriginType.Valid &&
		issue.OriginType.String == "autopilot" && issue.OriginID.Valid {
		var triggerID pgtype.UUID
		if run, err := s.Queries.GetAutopilotRunByIssue(ctx, issue.ID); err == nil {
			triggerID = run.TriggerID
		}
		return triggerOwnerAttribution(ctx, s.Queries, triggerID, issue.WorkspaceID, issue.OriginID, attribution.EvidenceIssueAssignment, issue.ID)
	}
	facts := attribution.DirectFacts{
		IssueID:     issue.ID,
		CreatorType: issue.CreatorType,
		CreatorID:   issue.CreatorID,
	}
	// Member-created issues resolve without a DB read. Only origin-linked
	// agent-created issues (quick_create, agent_create) need to load the origin
	// task to inherit its human, and only when the DB is wired (nil Queries keeps
	// unit-test setups safe and yields unattributed). Both origin types stamp
	// origin_id with the agent_task_queue row that created the issue, so the
	// top-of-chain human is that task's originator_user_id (MUL-4305).
	if !(issue.CreatorType == "member" && issue.CreatorID.Valid) &&
		s != nil && s.Queries != nil && issue.OriginType.Valid && issue.OriginID.Valid &&
		(issue.OriginType.String == "quick_create" || issue.OriginType.String == "agent_create") {
		facts.OriginType = issue.OriginType.String
		facts.OriginTaskID = issue.OriginID
		if task, err := s.Queries.GetAgentTask(ctx, issue.OriginID); err == nil {
			facts.OriginOriginator = task.OriginatorUserID
			facts.OriginAccountable = task.AccountableUserID
		}
	}
	return attribution.ClassifyDirect(facts)
}

// ruleOwnerAttribution resolves the rule_owner attribution for an autopilot run
// from its active (latest) rule version snapshot (MUL-4302 §3.4). Shared by both
// autopilot execution modes — run_only dispatch and the create_issue enqueue path —
// so they attribute identically. originator stays NULL (an autopilot carries no
// human's authority); only the audit-accountable side is set, to the version's
// member publisher. A missing version (autopilot published before this feature, or
// none yet) or a non-member/absent publisher degrades to unattributed rather than
// fabricating a human. Never returns an error: attribution must not fail an
// enqueue, and a degraded label is the honest fallback.
func ruleOwnerAttribution(ctx context.Context, q *db.Queries, workspaceID, autopilotID pgtype.UUID, evidenceKind attribution.EvidenceKind, evidenceRefID pgtype.UUID) attribution.Result {
	if q == nil || !autopilotID.Valid {
		return attribution.RuleOwner(pgtype.UUID{}, pgtype.UUID{}, evidenceKind, evidenceRefID)
	}
	ver, err := q.GetActiveAutopilotRuleVersion(ctx, db.GetActiveAutopilotRuleVersionParams{
		WorkspaceID: workspaceID,
		AutopilotID: autopilotID,
	})
	if err != nil {
		return attribution.RuleOwner(pgtype.UUID{}, pgtype.UUID{}, evidenceKind, evidenceRefID)
	}
	var publisher pgtype.UUID
	if ver.PublishedByType == "member" {
		publisher = ver.PublishedByID
	}
	return attribution.RuleOwner(publisher, ver.ID, evidenceKind, evidenceRefID)
}

// triggerOwnerAttribution resolves an autopilot schedule/webhook run to the human
// currently RESPONSIBLE for the firing trigger's effective config (MUL-4302; Bohan +
// Elon must-fix). triggerID is the autopilot_run's trigger_id. The trigger row's
// published_by starts at the creator and transfers to whoever later substantively
// edits it, so the run attributes to whoever last shaped what fires it — not the
// original creator. A trigger with no recorded publisher (predating this migration)
// or an agent publisher degrades to ruleOwnerAttribution (rule publisher, then
// owner_fallback) — the same coarser behavior autopilots had before, so nothing
// regresses. Never errors: attribution must not fail an enqueue.
func triggerOwnerAttribution(ctx context.Context, q *db.Queries, triggerID, workspaceID, autopilotID pgtype.UUID, evidenceKind attribution.EvidenceKind, evidenceRefID pgtype.UUID) attribution.Result {
	if q != nil && triggerID.Valid {
		// published_by is the member CURRENTLY responsible for this trigger's
		// effective config: the creator until someone substantively edits it (that
		// trigger's cron/filter/webhook, or an autopilot-level change that bumps all
		// its triggers), then the editor. So a run attributes to whoever last shaped
		// what fires it, not the original creator — and editing another trigger never
		// moves this one (MUL-4302; Elon must-fix).
		if trig, err := q.GetAutopilotTrigger(ctx, triggerID); err == nil &&
			trig.PublishedByType.Valid && trig.PublishedByType.String == "member" && trig.PublishedByID.Valid {
			return attribution.TriggerOwner(trig.PublishedByID, evidenceKind, evidenceRefID)
		}
	}
	return ruleOwnerAttribution(ctx, q, workspaceID, autopilotID, evidenceKind, evidenceRefID)
}

// ErrAttributionFailClosed signals that a run resolved to no precise accountable
// human and the enqueue is REFUSED rather than started. It covers three cases, all
// of which mean "we cannot guarantee an accountable human for this run" (MUL-4302
// §1/§3.5): the workspace opted into fail-closed; the workspace policy could not be
// read (so we cannot confirm fallback is allowed — fail closed, don't run); or
// owner_fallback has no agent owner to fall back to. Enqueue paths surface it so the
// run never starts.
var ErrAttributionFailClosed = errors.New("attribution: no precise accountable human and enqueue refused (fail-closed policy, policy read failed, or no agent owner)")

// applyAttributionFallback applies the workspace's degraded-attribution policy to a
// resolved attribution whose source came back unattributed (no precise human). A
// PRECISE attribution passes through untouched (no policy read at all). For an
// unattributed run the accountable-never-null guarantee is enforced fail-closed —
// we never silently enqueue a task that could run with a NULL accountable_user_id:
//
//   - policy read fails (or no workspace) → REFUSE. We cannot confirm the workspace
//     permits fallback, so we do not run an unattributable task on a transient DB
//     hiccup. (Only the rare unattributed path pays this; precise runs never read.)
//   - fail-closed workspace → REFUSE.
//   - otherwise → owner_fallback (accountable = agent owner, audit-only, originator
//     untouched). If there is no valid agent owner, owner_fallback stays
//     unattributed → REFUSE rather than enqueue a NULL-accountable task.
//
// Keeping this at the enqueue boundary (not inside the pure classifiers) means
// owner_fallback needs the agent owner, which every enqueue path has in hand.
func (s *TaskService) applyAttributionFallback(ctx context.Context, attr attribution.Result, agent db.Agent) (attribution.Result, error) {
	if attr.Source != attribution.SourceUnattributed {
		return attr, nil
	}
	if s == nil || s.Queries == nil || !agent.WorkspaceID.Valid {
		return attr, fmt.Errorf("%w: workspace policy unavailable", ErrAttributionFailClosed)
	}
	failClosed, err := s.Queries.GetWorkspaceAttributionFailClosed(ctx, agent.WorkspaceID)
	if err != nil {
		// Cannot confirm the workspace allows fallback → fail closed rather than
		// silently run an unattributable task.
		return attr, fmt.Errorf("%w: policy read failed: %v", ErrAttributionFailClosed, err)
	}
	if failClosed {
		return attr, ErrAttributionFailClosed
	}
	fallback := attribution.OwnerFallback(attr, agent.OwnerID)
	if fallback.Source == attribution.SourceUnattributed {
		// owner_fallback could not resolve an accountable human (no valid agent
		// owner): refuse rather than enqueue a NULL-accountable task.
		return attr, fmt.Errorf("%w: no agent owner to attribute", ErrAttributionFailClosed)
	}
	return fallback, nil
}

// attributionCreateParams maps a resolved attribution onto the CreateAgentTask
// provenance columns. originator_source is always stamped (never NULL for a new
// row); delegation lineage and evidence are stamped only when present.
func attributionCreateParams(attr attribution.Result) (source pgtype.Text, delegatedFrom pgtype.UUID, evidenceKind pgtype.Text, evidenceRef pgtype.UUID) {
	source = pgtype.Text{String: attr.Source.String(), Valid: true}
	delegatedFrom = attr.DelegatedFromTaskID
	evidenceKind = pgtype.Text{String: string(attr.EvidenceKind), Valid: attr.EvidenceKind != ""}
	evidenceRef = attr.EvidenceRefID
	return
}

// OriginatorForIssueTask exposes resolveOriginatorForIssueTask to callers
// outside the service package (the squad-leader access gate in the handler
// layer) so the gate judges the top-of-chain human with the exact same
// resolution the enqueue path persists on the task row. Without a shared entry
// point the gate saw an empty originator for agent-triggered assigns and denied
// private leaders that the write path would have attributed correctly
// (MUL-4305).
func (s *TaskService) OriginatorForIssueTask(ctx context.Context, issue db.Issue, triggerCommentID pgtype.UUID) pgtype.UUID {
	return s.resolveOriginatorForIssueTask(ctx, issue, triggerCommentID)
}

func (s *TaskService) captureTaskDispatched(ctx context.Context, task db.AgentTaskQueue) {
	if s.Metrics != nil {
		source, runtimeMode, _ := s.taskMetricsContext(ctx, task)
		s.Metrics.RecordTaskDispatched(util.UUIDToString(task.ID), source, runtimeMode, taskQueueWaitSeconds(task))
	}
}

func (s *TaskService) AnalyticsContextForTask(ctx context.Context, task db.AgentTaskQueue) analytics.TaskContext {
	return s.taskAnalyticsContext(ctx, task)
}

func (s *TaskService) captureTaskStarted(ctx context.Context, task db.AgentTaskQueue) {
	if s.Metrics != nil {
		source, runtimeMode, provider := s.taskMetricsContext(ctx, task)
		s.Metrics.RecordTaskStarted(source, runtimeMode, provider)
	}
}

func (s *TaskService) captureTaskCompleted(ctx context.Context, task db.AgentTaskQueue) {
	if s.Metrics != nil {
		source, runtimeMode, _ := s.taskMetricsContext(ctx, task)
		s.Metrics.RecordTaskTerminal(util.UUIDToString(task.ID), source, runtimeMode, task.Status, taskRunSeconds(task), taskTotalSeconds(task), task.Attempt)
	}
}

func (s *TaskService) captureTaskFailed(ctx context.Context, task db.AgentTaskQueue) {
	failureReason := taskFailureReason(task)
	if s.Metrics != nil {
		source, runtimeMode, _ := s.taskMetricsContext(ctx, task)
		s.Metrics.RecordTaskTerminal(util.UUIDToString(task.ID), source, runtimeMode, task.Status, taskRunSeconds(task), taskTotalSeconds(task), task.Attempt)
		s.Metrics.RecordTaskFailed(source, runtimeMode, failureReason)
	}
}

func (s *TaskService) captureTaskCancelled(ctx context.Context, task db.AgentTaskQueue) {
	if s.Metrics != nil {
		source, runtimeMode, _ := s.taskMetricsContext(ctx, task)
		s.Metrics.RecordTaskTerminal(util.UUIDToString(task.ID), source, runtimeMode, task.Status, taskRunSeconds(task), taskTotalSeconds(task), task.Attempt)
	}
	// Revoke any mat_ task tokens minted for this task. Cancellation is
	// a terminal transition, so the running agent process no longer
	// needs to call back; eagerly deleting the token closes the
	// window where a compromised process could keep authenticating
	// against the API until the 24h expiry. Failure is non-fatal — the
	// expiry / FK cascade are the durable guards. MUL-2600.
	if err := s.Queries.DeleteTaskTokensByTask(ctx, task.ID); err != nil {
		slog.Warn("cancel task: failed to revoke task tokens",
			"task_id", util.UUIDToString(task.ID), "error", err)
	}
}

// costUSDTicks is the provider's own price for this usage in 1e-10 USD, or 0
// when it reported none — the metrics layer prefers it over its rate table.
func (s *TaskService) CaptureTaskUsage(ctx context.Context, task db.AgentTaskQueue, provider, model string, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens, costUSDTicks int64) {
	if s.Metrics == nil {
		return
	}
	source, runtimeMode, _ := s.taskMetricsContext(ctx, task)
	s.Metrics.RecordLLMUsage(source, runtimeMode, provider, model, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens, costUSDTicks)
}

func (s *TaskService) CaptureQueuedExpiredTasks(ctx context.Context, tasks []db.AgentTaskQueue) {
	if s.Metrics == nil {
		return
	}
	for _, task := range tasks {
		source, runtimeMode, _ := s.taskMetricsContext(ctx, task)
		s.Metrics.RecordTaskQueuedExpired(source, runtimeMode)
	}
}

func (s *TaskService) CaptureLeaseExpiredTasks(ctx context.Context, tasks []db.AgentTaskQueue) {
	if s.Metrics == nil {
		return
	}
	for _, task := range tasks {
		source, _, _ := s.taskMetricsContext(ctx, task)
		s.Metrics.RecordTaskLeaseExpired(source)
	}
}

func (s *TaskService) cachedTaskAnalyticsContext(task db.AgentTaskQueue) (analytics.TaskContext, bool) {
	key := taskAnalyticsContextKey(task)
	if key == "" {
		return analytics.TaskContext{}, false
	}
	s.analyticsContextMu.Lock()
	defer s.analyticsContextMu.Unlock()
	if s.analyticsContextCache == nil {
		return analytics.TaskContext{}, false
	}
	tc, ok := s.analyticsContextCache[key]
	return tc, ok
}

func (s *TaskService) storeTaskAnalyticsContext(task db.AgentTaskQueue, tc analytics.TaskContext) {
	if tc.WorkspaceID == "" {
		return
	}
	key := taskAnalyticsContextKey(task)
	if key == "" {
		return
	}
	s.analyticsContextMu.Lock()
	defer s.analyticsContextMu.Unlock()
	if s.analyticsContextCache == nil {
		s.analyticsContextCache = make(map[string]analytics.TaskContext)
	}
	if _, ok := s.analyticsContextCache[key]; !ok {
		s.analyticsContextOrder = append(s.analyticsContextOrder, key)
		if len(s.analyticsContextOrder) > taskAnalyticsContextCacheMax {
			oldest := s.analyticsContextOrder[0]
			s.analyticsContextOrder = s.analyticsContextOrder[1:]
			delete(s.analyticsContextCache, oldest)
		}
	}
	s.analyticsContextCache[key] = tc
}

func taskAnalyticsContextKey(task db.AgentTaskQueue) string {
	taskID := util.UUIDToString(task.ID)
	if taskID == "" {
		return ""
	}
	return strings.Join([]string{
		taskID,
		util.UUIDToString(task.RuntimeID),
		util.UUIDToString(task.IssueID),
		util.UUIDToString(task.ChatSessionID),
		util.UUIDToString(task.AutopilotRunID),
	}, "|")
}

func (s *TaskService) taskMetricsContext(ctx context.Context, task db.AgentTaskQueue) (source, runtimeMode, provider string) {
	tc := s.taskAnalyticsContext(ctx, task)
	source = "other"
	switch {
	case task.ChatSessionID.Valid:
		source = "chat"
	case task.IssueID.Valid:
		if tc.Source == analytics.SourceAutopilot {
			source = "autopilot_issue"
		} else {
			source = "issue"
		}
	case task.AutopilotRunID.Valid:
		source = "autopilot"
	default:
		if _, ok := s.parseQuickCreateContext(task); ok {
			source = "quick_create"
		} else if tc.Source != "" {
			source = tc.Source
		}
	}
	return source, tc.RuntimeMode, tc.Provider
}

func (s *TaskService) taskAnalyticsContext(ctx context.Context, task db.AgentTaskQueue) analytics.TaskContext {
	if tc, ok := s.cachedTaskAnalyticsContext(task); ok {
		return tc
	}
	tc := analytics.TaskContext{
		AgentID: util.UUIDToString(task.AgentID),
		TaskID:  util.UUIDToString(task.ID),
		Source:  analytics.SourceManual,
	}
	if task.IssueID.Valid {
		tc.IssueID = util.UUIDToString(task.IssueID)
	}
	if task.ChatSessionID.Valid {
		tc.ChatSessionID = util.UUIDToString(task.ChatSessionID)
		tc.Source = analytics.SourceChat
	}
	if task.AutopilotRunID.Valid {
		tc.AutopilotRunID = util.UUIDToString(task.AutopilotRunID)
		tc.Source = analytics.SourceAutopilot
	}

	if task.RuntimeID.Valid {
		if rt, err := s.Queries.GetAgentRuntime(ctx, task.RuntimeID); err == nil {
			tc.WorkspaceID = util.UUIDToString(rt.WorkspaceID)
			tc.RuntimeMode = rt.RuntimeMode
			tc.Provider = rt.Provider
		}
	}
	if tc.WorkspaceID == "" || tc.RuntimeMode == "" {
		if agent, err := s.Queries.GetAgent(ctx, task.AgentID); err == nil {
			if tc.WorkspaceID == "" {
				tc.WorkspaceID = util.UUIDToString(agent.WorkspaceID)
			}
			if tc.RuntimeMode == "" {
				tc.RuntimeMode = agent.RuntimeMode
			}
		}
	}

	if task.IssueID.Valid {
		if issue, err := s.Queries.GetIssue(ctx, task.IssueID); err == nil {
			tc.WorkspaceID = util.UUIDToString(issue.WorkspaceID)
			if issue.CreatorType == "member" {
				tc.UserID = util.UUIDToString(issue.CreatorID)
			}
			if issue.OriginType.Valid {
				switch issue.OriginType.String {
				case "autopilot":
					tc.Source = analytics.SourceAutopilot
					if ap, err := s.Queries.GetAutopilot(ctx, issue.OriginID); err == nil {
						if ap.CreatedByType == "member" {
							tc.UserID = util.UUIDToString(ap.CreatedByID)
						}
					}
				case "quick_create":
					tc.Source = analytics.SourceManual
				}
			}
		}
	}
	if task.ChatSessionID.Valid {
		if cs, err := s.Queries.GetChatSession(ctx, task.ChatSessionID); err == nil {
			tc.WorkspaceID = util.UUIDToString(cs.WorkspaceID)
			tc.UserID = util.UUIDToString(cs.CreatorID)
		}
	}
	if task.AutopilotRunID.Valid {
		if run, err := s.Queries.GetAutopilotRun(ctx, task.AutopilotRunID); err == nil {
			if ap, err := s.Queries.GetAutopilot(ctx, run.AutopilotID); err == nil {
				tc.WorkspaceID = util.UUIDToString(ap.WorkspaceID)
				if ap.CreatedByType == "member" {
					tc.UserID = util.UUIDToString(ap.CreatedByID)
				}
			}
		}
	}
	if qc, ok := s.parseQuickCreateContext(task); ok {
		tc.WorkspaceID = qc.WorkspaceID
		tc.UserID = qc.RequesterID
		tc.Source = analytics.Sour…28950 tokens truncated…tly.
		if !triggerCommentID.Valid {
			coalescedCommentIDs = append([]pgtype.UUID{}, sourceTask.CoalescedCommentIds...)
			if sourceTask.TriggerCommentID.Valid {
				triggerCommentID = sourceTask.TriggerCommentID
			} else if len(coalescedCommentIDs) > 0 {
				triggerCommentID, coalescedCommentIDs, err = s.promoteNewestSurvivingComment(ctx, coalescedCommentIDs)
				if err != nil {
					return nil, fmt.Errorf("repair source comment plan: %w", err)
				}
			}
		}
	} else {
		switch {
		case issue.AssigneeType.String == "agent" && issue.AssigneeID.Valid:
			agentID = issue.AssigneeID
		case issue.AssigneeType.String == "squad" && issue.AssigneeID.Valid:
			squad, err := s.Queries.GetSquad(ctx, issue.AssigneeID)
			if err != nil {
				return nil, fmt.Errorf("issue is assigned to a squad but squad not found")
			}
			agentID = squad.LeaderID
			isLeader = true
			squadID = issue.AssigneeID
		default:
			return nil, fmt.Errorf("issue is not assigned to an agent or squad")
		}
	}

	// Re-validate invoke permission on the RESOLVED target before mutating
	// anything (MUL-4525). For a task_id rerun this gates the historical agent,
	// so a since-reassigned issue can't be used to re-fire a private agent the
	// operator may only view. A block fails closed: no prior task is cancelled,
	// no new task is created.
	if canInvoke != nil {
		targetAgent, err := s.Queries.GetAgent(ctx, agentID)
		if err != nil {
			return nil, fmt.Errorf("load target agent: %w", err)
		}
		if !canInvoke(targetAgent) {
			return nil, ErrRerunInvokeNotAllowed
		}
	}

	// Cancel only the target agent's active/queued tasks on this issue.
	cancelled, err := s.Queries.CancelAgentTasksByIssueAndAgent(ctx, db.CancelAgentTasksByIssueAndAgentParams{
		IssueID: issueID,
		AgentID: agentID,
	})
	if err != nil {
		slog.Warn("rerun: cancel prior tasks failed",
			"issue_id", util.UUIDToString(issueID),
			"agent_id", util.UUIDToString(agentID),
			"error", err,
		)
	}
	for _, t := range cancelled {
		s.captureTaskCancelled(ctx, t)
		s.ReconcileAgentStatus(ctx, t.AgentID)
		s.broadcastTaskEvent(ctx, protocol.EventTaskCancelled, t)
	}

	// A manual rerun is a NEW direct_human trigger attributed to the rerunning
	// member, not the original run's human (MUL-4302 §5); actorUserID carries them.
	// sourceTaskID is the rerun lineage: it rides the CreateAgentTask insert
	// (rerun_of_task_id) so the queued event / daemon claim never sees a NULL
	// lineage, and it stays distinct from system-retry's retry_of_task_id (§5).
	task, err := s.enqueueRerunTask(ctx, issue, agentID, triggerCommentID, coalescedCommentIDs, isLeader, squadID, actorUserID, sourceTaskID)
	if err != nil {
		return nil, err
	}
	slog.Info("issue rerun enqueued",
		"task_id", util.UUIDToString(task.ID),
		"issue_id", util.UUIDToString(issueID),
		"agent_id", util.UUIDToString(agentID),
		"source_task_id", util.UUIDToString(sourceTaskID),
		"is_leader", isLeader,
		"cancelled_prior", len(cancelled),
	)
	return &task, nil
}

// promoteNewestSurvivingComment repairs a manual rerun whose original trigger
// was deleted (the FK clears trigger_comment_id while the UUID-array plan
// survives). Promoting before enqueue lets the normal enqueue path recompute
// originator and user-scoped connected-app capabilities from the real comment,
// rather than carrying the deleted trigger's stale security context.
func (s *TaskService) promoteNewestSurvivingComment(ctx context.Context, ids []pgtype.UUID) (pgtype.UUID, []pgtype.UUID, error) {
	type survivingComment struct {
		id        pgtype.UUID
		createdAt time.Time
	}
	survivors := make([]survivingComment, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if !id.Valid {
			continue
		}
		key := util.UUIDToString(id)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		comment, err := s.Queries.GetComment(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return pgtype.UUID{}, nil, err
		}
		survivors = append(survivors, survivingComment{id: comment.ID, createdAt: comment.CreatedAt.Time})
	}
	if len(survivors) == 0 {
		return pgtype.UUID{}, nil, nil
	}
	newest := 0
	for i := 1; i < len(survivors); i++ {
		if survivors[i].createdAt.After(survivors[newest].createdAt) ||
			(survivors[i].createdAt.Equal(survivors[newest].createdAt) &&
				util.UUIDToString(survivors[i].id) > util.UUIDToString(survivors[newest].id)) {
			newest = i
		}
	}
	remaining := make([]pgtype.UUID, 0, len(survivors)-1)
	for i, comment := range survivors {
		if i != newest {
			remaining = append(remaining, comment.id)
		}
	}
	return survivors[newest].id, remaining, nil
}

// enqueueRerunTask enqueues a fresh task for the given agent on the issue.
// When the target agent is the issue's single-agent assignee we use the
// assignee-driven path (enqueueIssueTask) so the issue-assignee bookkeeping
// stays in sync; otherwise (squad member, prior assignee that has since been
// reassigned, mention agent) we use the mention path.
//
// force_fresh_session is pinned to true on every rerun row on purpose. It is
// the rollback-safe legacy signal: an OLD claim handler (mid rolling deploy)
// gates the whole resume lookup on !force_fresh_session, so it starts clean
// instead of resuming via the (agent, issue) most-recent query — which could
// pick a different execution than the one the user clicked. The NEW claim
// handler ignores this flag for reruns and instead reads the exact source task
// (rerun_of_task_id) to reuse its workdir and, when the failure did not poison
// the conversation, resume its session (MUL-4869).
func (s *TaskService) enqueueRerunTask(ctx context.Context, issue db.Issue, agentID pgtype.UUID, triggerCommentID pgtype.UUID, coalescedCommentIDs []pgtype.UUID, isLeader bool, squadID pgtype.UUID, actorUserID pgtype.UUID, rerunOfTaskID pgtype.UUID) (db.AgentTaskQueue, error) {
	if issue.AssigneeType.String == "agent" && issue.AssigneeID.Valid &&
		util.UUIDToString(issue.AssigneeID) == util.UUIDToString(agentID) {
		return s.enqueueIssueTaskWithCommentPlan(ctx, issue, triggerCommentID, coalescedCommentIDs, true, "", actorUserID, rerunOfTaskID)
	}
	return s.enqueueMentionTaskWithCommentPlan(ctx, issue, agentID, triggerCommentID, coalescedCommentIDs, isLeader, squadID, true, "", actorUserID, rerunOfTaskID)
}

// HandleFailedTasks runs the post-failure side effects for a batch of
// freshly-failed tasks: optional auto-retry, task:failed event broadcast,
// agent status reconciliation, and (when an issue has no remaining active
// task and isn't being retried) resetting the issue back to todo so the
// daemon can pick it up again.
//
// All callers that surface a task as failed — sweepers, FailTask,
// recover-orphans — funnel through here so the same UI-consistency
// guarantees apply on every code path.
func (s *TaskService) HandleFailedTasks(ctx context.Context, tasks []db.AgentTaskQueue) int {
	if len(tasks) == 0 {
		return 0
	}

	affectedAgents := make(map[string]pgtype.UUID)
	processedIssues := make(map[string]bool)
	retriedIssues := make(map[string]bool)
	retried := 0

	for _, t := range tasks {
		// Auto-retry first so the issue stays in_progress rather than
		// flapping todo → in_progress within a tick.
		if child, _ := s.MaybeRetryFailedTask(ctx, t); child != nil {
			retried++
			if t.IssueID.Valid {
				retriedIssues[util.UUIDToString(t.IssueID)] = true
			}
		}

		failureReason := "agent_error"
		if t.FailureReason.Valid && t.FailureReason.String != "" {
			failureReason = t.FailureReason.String
		}
		s.captureTaskFailed(ctx, t)

		workspaceID := ""
		if t.IssueID.Valid {
			if issue, err := s.Queries.GetIssue(ctx, t.IssueID); err == nil {
				workspaceID = util.UUIDToString(issue.WorkspaceID)
				// Reset stuck in_progress issues only when no other active
				// task exists for the issue and no retry was just enqueued.
				issueKey := util.UUIDToString(t.IssueID)
				if issue.Status == "in_progress" && !processedIssues[issueKey] && !retriedIssues[issueKey] {
					processedIssues[issueKey] = true
					hasActive, checkErr := s.Queries.HasActiveTaskForIssue(ctx, t.IssueID)
					if checkErr != nil {
						slog.Warn("handle failed tasks: active check failed",
							"issue_id", issueKey,
							"error", checkErr,
						)
					} else if !hasActive {
						updatedIssue, updateErr := s.Queries.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{
							ID:          t.IssueID,
							Status:      "todo",
							WorkspaceID: issue.WorkspaceID,
						})
						if updateErr != nil {
							slog.Warn("handle failed tasks: reset stuck issue failed",
								"issue_id", issueKey,
								"error", updateErr,
							)
						} else {
							// This direct reset bypasses the HTTP UpdateIssue
							// handler that normally emits issue:updated, so emit
							// it here too. Without it the board / status-filter
							// caches keep showing the issue as in_progress until
							// the next write touches it (#4648 / MUL-3782).
							s.broadcastIssueUpdated(updatedIssue, issue.Status)
						}
					}
				}
			}
		}
		if workspaceID == "" {
			workspaceID = s.ResolveTaskWorkspaceID(ctx, t)
		}

		if workspaceID != "" {
			s.Bus.Publish(events.Event{
				Type:        protocol.EventTaskFailed,
				WorkspaceID: workspaceID,
				ActorType:   "system",
				Payload: map[string]any{
					"task_id":        util.UUIDToString(t.ID),
					"agent_id":       util.UUIDToString(t.AgentID),
					"issue_id":       util.UUIDToString(t.IssueID),
					"status":         "failed",
					"failure_reason": failureReason,
				},
			})
		}

		affectedAgents[util.UUIDToString(t.AgentID)] = t.AgentID
	}

	for _, agentID := range affectedAgents {
		s.ReconcileAgentStatus(ctx, agentID)
	}
	s.notifyTasksFinished(tasks)
	return retried
}

// runInTx executes fn inside a single DB transaction. If TxStarter is nil
// (e.g. some tests construct TaskService directly), fn runs against the
// regular Queries handle without transactional guarantees.
func (s *TaskService) runInTx(ctx context.Context, fn func(*db.Queries) error) error {
	if s.TxStarter == nil {
		return fn(s.Queries)
	}
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := fn(s.Queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReportProgress broadcasts a progress update via the event bus.
func (s *TaskService) ReportProgress(ctx context.Context, taskID string, workspaceID string, summary string, step, total int) {
	s.Bus.Publish(events.Event{
		Type:        protocol.EventTaskProgress,
		WorkspaceID: workspaceID,
		ActorType:   "system",
		ActorID:     "",
		TaskID:      taskID,
		Payload: protocol.TaskProgressPayload{
			TaskID:  taskID,
			Summary: summary,
			Step:    step,
			Total:   total,
		},
	})
}

// ReconcileAgentStatus refreshes agent status from the current active task set.
func (s *TaskService) ReconcileAgentStatus(ctx context.Context, agentID pgtype.UUID) {
	agent, err := s.Queries.RefreshAgentStatusFromTasks(ctx, agentID)
	if err != nil {
		return
	}
	slog.Debug("agent status reconciled", "agent_id", util.UUIDToString(agentID), "status", agent.Status)
	s.publishAgentStatus(agent)
}

func (s *TaskService) updateAgentStatus(ctx context.Context, agentID pgtype.UUID, status string) {
	agent, err := s.Queries.UpdateAgentStatus(ctx, db.UpdateAgentStatusParams{
		ID:     agentID,
		Status: status,
	})
	if err != nil {
		return
	}
	s.publishAgentStatus(agent)
}

func (s *TaskService) publishAgentStatus(agent db.Agent) {
	s.Bus.Publish(events.Event{
		Type:        protocol.EventAgentStatus,
		WorkspaceID: util.UUIDToString(agent.WorkspaceID),
		ActorType:   "system",
		ActorID:     "",
		Payload:     map[string]any{"agent": agentToMap(agent)},
	})
}

// LoadAgentSkills loads an agent's skills with their files for task execution.
func (s *TaskService) LoadAgentSkills(ctx context.Context, agentID pgtype.UUID) []AgentSkillData {
	skills, err := s.Queries.ListAgentSkills(ctx, agentID)
	if err != nil || len(skills) == 0 {
		return nil
	}

	result := make([]AgentSkillData, 0, len(skills))
	for _, sk := range skills {
		data := AgentSkillData{
			ID:          util.UUIDToString(sk.ID),
			Name:        sk.Name,
			Description: sk.Description,
			Content:     sk.Content,
		}
		files, _ := s.Queries.ListSkillFiles(ctx, sk.ID)
		for _, f := range files {
			data.Files = append(data.Files, AgentSkillFileData{Path: f.Path, Content: f.Content})
		}
		result = append(result, data)
	}
	return result
}

// LoadAgentSkillBundles returns every skill visible to an agent, including
// built-ins, with stable bundle hashes and lightweight refs for slim claims.
func (s *TaskService) LoadAgentSkillBundles(ctx context.Context, agentID pgtype.UUID) ([]AgentSkillData, []AgentSkillRefData) {
	skills := s.LoadAgentSkills(ctx, agentID)
	skills = append(skills, s.BuiltinSkills()...)
	return BuildAgentSkillBundles(skills)
}

func BuildAgentSkillBundles(skills []AgentSkillData) ([]AgentSkillData, []AgentSkillRefData) {
	bundles := make([]AgentSkillData, 0, len(skills))
	refs := make([]AgentSkillRefData, 0, len(skills))
	for _, skill := range skills {
		source := skill.Source
		id := skill.ID
		if source == "" {
			if id == "" {
				source = skillbundle.SourceBuiltin
			} else {
				source = skillbundle.SourceWorkspace
			}
		}
		if id == "" && source == skillbundle.SourceBuiltin {
			id = "builtin:" + skill.Name
		}
		skill.Source = source
		skill.ID = id

		files := make([]skillbundle.File, 0, len(skill.Files))
		for _, file := range skill.Files {
			files = append(files, skillbundle.File{Path: file.Path, Content: file.Content})
		}
		manifest := skillbundle.BuildManifest(skillbundle.Skill{
			ID:          skill.ID,
			Source:      skill.Source,
			Name:        skill.Name,
			Description: skill.Description,
			Content:     skill.Content,
			Files:       files,
		})
		skill.Hash = manifest.Hash
		skill.SizeBytes = manifest.SizeBytes
		fileRefsByPath := make(map[string]skillbundle.FileRef, len(manifest.Files))
		for _, file := range manifest.Files {
			fileRefsByPath[file.Path] = file
		}
		for i := range skill.Files {
			if ref, ok := fileRefsByPath[skill.Files[i].Path]; ok {
				skill.Files[i].SHA256 = ref.SHA256
				skill.Files[i].SizeBytes = ref.SizeBytes
			}
		}
		bundles = append(bundles, skill)

		refFiles := make([]AgentSkillFileRefData, 0, len(manifest.Files))
		for _, file := range manifest.Files {
			refFiles = append(refFiles, AgentSkillFileRefData{
				Path:      file.Path,
				SHA256:    file.SHA256,
				SizeBytes: file.SizeBytes,
			})
		}
		refs = append(refs, AgentSkillRefData{
			ID:          skill.ID,
			Source:      skill.Source,
			Name:        skill.Name,
			Description: skill.Description,
			Hash:        manifest.Hash,
			SizeBytes:   manifest.SizeBytes,
			FileCount:   manifest.FileCount,
			Files:       refFiles,
		})
	}
	return bundles, refs
}

// AgentSkillData represents a skill for task execution responses.
type AgentSkillData struct {
	ID          string               `json:"id"`
	Source      string               `json:"source,omitempty"`
	Name        string               `json:"name"`
	Description string               `json:"description,omitempty"`
	Hash        string               `json:"hash,omitempty"`
	SizeBytes   int64                `json:"size_bytes,omitempty"`
	Content     string               `json:"content"`
	Files       []AgentSkillFileData `json:"files,omitempty"`
}

// AgentSkillFileData represents a supporting file within a skill.
type AgentSkillFileData struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	SHA256    string `json:"sha256,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

type AgentSkillRefData struct {
	ID          string                  `json:"id"`
	Source      string                  `json:"source"`
	Name        string                  `json:"name"`
	Description string                  `json:"description,omitempty"`
	Hash        string                  `json:"hash"`
	SizeBytes   int64                   `json:"size_bytes"`
	FileCount   int                     `json:"file_count"`
	Files       []AgentSkillFileRefData `json:"files,omitempty"`
}

type AgentSkillFileRefData struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// computeChatElapsedMs returns the wall-clock duration from task creation
// (user hit send) to terminal state (completed/failed). Stored on the
// assistant chat_message so the UI can render "Replied in 38s" /
// "Failed after 12s". Uses created_at — not started_at — because users
// experience total wait time, including queue + dispatch, not just the
// daemon's actual run time.
func computeChatElapsedMs(task db.AgentTaskQueue) pgtype.Int8 {
	if !task.CompletedAt.Valid || !task.CreatedAt.Valid {
		return pgtype.Int8{}
	}
	ms := task.CompletedAt.Time.Sub(task.CreatedAt.Time).Milliseconds()
	if ms < 0 {
		ms = 0
	}
	return pgtype.Int8{Int64: ms, Valid: true}
}

func priorityToInt(p string) int32 {
	switch p {
	case "urgent":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

// NotifyTaskEnqueued is the cross-package shim for callers outside
// TaskService (e.g. AutopilotService.dispatchRunOnly) that insert a
// row into agent_task_queue directly. Invalidates the empty-claim
// cache and kicks the daemon WS so the new task is claimed without
// waiting for the next poll.
func (s *TaskService) NotifyTaskEnqueued(ctx context.Context, task db.AgentTaskQueue) {
	s.captureTaskQueued(ctx, task)
	s.notifyTaskAvailable(task)
}

// NotifyTaskFinished invalidates a runtime's empty-claim verdict and emits a
// best-effort daemon wakeup after a task reaches a terminal state. The task ID
// is deliberately omitted from the wakeup payload: the completed task itself
// is not available; the hint only means that a queued successor may have
// become claimable because an agent-capacity or serialization barrier cleared.
func (s *TaskService) NotifyTaskFinished(task db.AgentTaskQueue) {
	s.notifyRuntimeMayHaveWork(task.RuntimeID, "")
}

// notifyTasksFinished is the batch form used by bulk terminal transitions.
// Coalesce by runtime so cancelling many tasks on one machine produces one
// cache bump and one websocket hint rather than a burst of identical work.
func (s *TaskService) notifyTasksFinished(tasks []db.AgentTaskQueue) {
	seen := make(map[string]struct{}, len(tasks))
	for _, task := range tasks {
		if !task.RuntimeID.Valid {
			continue
		}
		runtimeKey := util.UUIDToString(task.RuntimeID)
		if _, ok := seen[runtimeKey]; ok {
			continue
		}
		seen[runtimeKey] = struct{}{}
		s.notifyRuntimeMayHaveWork(task.RuntimeID, "")
	}
}

// notifyTaskAvailable runs after a task has been inserted: bumps the
// runtime's invalidation version so any in-flight claim that is about
// to write an "empty" verdict will have it rejected on read, then
// kicks the daemon WS so the daemon claims without waiting for its
// next poll. Order matters — Bump must happen before the wakeup,
// otherwise the wakeup-driven claim could read the still-current
// empty verdict and return null.
func (s *TaskService) notifyTaskAvailable(task db.AgentTaskQueue) {
	s.notifyRuntimeMayHaveWork(task.RuntimeID, util.UUIDToString(task.ID))
}

// notifyRuntimeMayHaveWork is the shared bump-before-wakeup primitive for both
// fresh enqueues and terminal transitions that can unblock queued work.
func (s *TaskService) notifyRuntimeMayHaveWork(runtimeID pgtype.UUID, taskID string) {
	if !runtimeID.Valid {
		return
	}
	runtimeKey := util.UUIDToString(runtimeID)
	// Use a background context: the cache bump / wakeup must outlive
	// the request that created the task, otherwise an early client
	// disconnect could leave the empty verdict in place and stall the
	// just-queued task until the TTL expires. The cache itself bounds
	// every Redis call with a short timeout so a wedged Redis cannot
	// block enqueue.
	s.EmptyClaim.Bump(context.Background(), runtimeKey)
	if s.Wakeup == nil {
		return
	}
	s.Wakeup.NotifyTaskAvailable(runtimeKey, taskID)
}

func (s *TaskService) broadcastTaskDispatch(ctx context.Context, task db.AgentTaskQueue) {
	var payload map[string]any
	if task.Context != nil {
		json.Unmarshal(task.Context, &payload)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payload["task_id"] = util.UUIDToString(task.ID)
	payload["runtime_id"] = util.UUIDToString(task.RuntimeID)
	payload["issue_id"] = util.UUIDToString(task.IssueID)
	payload["agent_id"] = util.UUIDToString(task.AgentID)
	// chat_session_id is the routing key the chat window uses to writethrough
	// `chatKeys.pendingTask` to status="running" the moment the daemon claims
	// the task. Without it the pill stays stuck at "Queued" until completion.
	if task.ChatSessionID.Valid {
		payload["chat_session_id"] = util.UUIDToString(task.ChatSessionID)
	}

	workspaceID := s.ResolveTaskWorkspaceID(ctx, task)
	if workspaceID == "" {
		return
	}
	s.Bus.Publish(events.Event{
		Type:        protocol.EventTaskDispatch,
		WorkspaceID: workspaceID,
		ActorType:   "system",
		ActorID:     "",
		Payload:     payload,
	})
}

func (s *TaskService) broadcastTaskEvent(ctx context.Context, eventType string, task db.AgentTaskQueue) {
	workspaceID := s.ResolveTaskWorkspaceID(ctx, task)
	if workspaceID == "" {
		return
	}
	payload := map[string]any{
		"task_id":  util.UUIDToString(task.ID),
		"agent_id": util.UUIDToString(task.AgentID),
		"issue_id": util.UUIDToString(task.IssueID),
		"status":   task.Status,
	}
	if task.ChatSessionID.Valid {
		payload["chat_session_id"] = util.UUIDToString(task.ChatSessionID)
	}
	s.Bus.Publish(events.Event{
		Type:        eventType,
		WorkspaceID: workspaceID,
		ActorType:   "system",
		ActorID:     "",
		Payload:     payload,
	})
}

// ResolveTaskWorkspaceID determines the workspace ID for a task.
// For issue tasks, it comes from the issue. For chat tasks, from the chat session.
// For autopilot tasks, from the autopilot via its run.
// Returns "" when none of the links resolve — callers treat that as "not found".
func (s *TaskService) ResolveTaskWorkspaceID(ctx context.Context, task db.AgentTaskQueue) string {
	if task.IssueID.Valid {
		if issue, err := s.Queries.GetIssue(ctx, task.IssueID); err == nil {
			return util.UUIDToString(issue.WorkspaceID)
		}
	}
	if task.ChatSessionID.Valid {
		if cs, err := s.Queries.GetChatSession(ctx, task.ChatSessionID); err == nil {
			return util.UUIDToString(cs.WorkspaceID)
		}
	}
	if task.AutopilotRunID.Valid {
		if run, err := s.Queries.GetAutopilotRun(ctx, task.AutopilotRunID); err == nil {
			if ap, err := s.Queries.GetAutopilot(ctx, run.AutopilotID); err == nil {
				return util.UUIDToString(ap.WorkspaceID)
			}
		}
	}
	// Quick-create tasks have no issue / chat / autopilot link — workspace
	// lives in the context JSONB. Returning "" here is what blocked
	// requireDaemonTaskAccess (404 on /start, /progress, /complete, /fail
	// for the daemon) and silently dropped task:dispatch / task:completed
	// broadcasts, which is why quick-create tasks appeared stuck queued.
	if qc, ok := s.parseQuickCreateContext(task); ok {
		return qc.WorkspaceID
	}
	return ""
}

func (s *TaskService) broadcastChatDone(ctx context.Context, task db.AgentTaskQueue, msg *db.ChatMessage) {
	workspaceID := s.ResolveTaskWorkspaceID(ctx, task)
	if workspaceID == "" {
		return
	}
	payload := protocol.ChatDonePayload{
		ChatSessionID: util.UUIDToString(task.ChatSessionID),
		TaskID:        util.UUIDToString(task.ID),
	}
	if msg != nil {
		payload.MessageID = util.UUIDToString(msg.ID)
		payload.Content = msg.Content
		payload.MessageKind = msg.MessageKind
		if msg.CreatedAt.Valid {
			payload.CreatedAt = msg.CreatedAt.Time.UTC().Format(time.RFC3339Nano)
		}
		if msg.ElapsedMs.Valid {
			payload.ElapsedMs = msg.ElapsedMs.Int64
		}
	}
	s.Bus.Publish(events.Event{
		Type:          protocol.EventChatDone,
		WorkspaceID:   workspaceID,
		ActorType:     "system",
		ActorID:       "",
		ChatSessionID: util.UUIDToString(task.ChatSessionID),
		Payload:       payload,
	})
}

// broadcastIssueUpdated publishes the issue:updated event the frontend's
// realtime reconcile (onIssueUpdated) relies on to move an issue between status
// columns / status filters and reconcile their bucket counts. prevStatus is the
// issue's status before the write so the client can gate that reconcile on
// status_changed.
//
// The `issue` payload is a map (issueToMap), which the workspace WS fanout
// (listeners.go SubscribeAll) marshals and broadcasts as-is — that is what
// drives the UI reconcile. Note this does NOT cover the full HTTP UpdateIssue
// side effects: the activity-log and inbox listeners type-assert `issue` to a
// handler.IssueResponse and skip a map, so a background status reset does not
// emit status-change activity / notifications. That is intentional for the
// realtime-staleness fix (#4648 / MUL-3782); folding those side effects in
// would mean unifying the payload type and is left as a follow-up.
func (s *TaskService) broadcastIssueUpdated(issue db.Issue, prevStatus string) {
	prefix := s.getIssuePrefix(issue.WorkspaceID)
	s.Bus.Publish(events.Event{
		Type:        protocol.EventIssueUpdated,
		WorkspaceID: util.UUIDToString(issue.WorkspaceID),
		ActorType:   "system",
		ActorID:     "",
		Payload: map[string]any{
			"issue":          issueToMap(issue, prefix),
			"status_changed": prevStatus != issue.Status,
			"prev_status":    prevStatus,
		},
	})
}

func (s *TaskService) getIssuePrefix(workspaceID pgtype.UUID) string {
	ws, err := s.Queries.GetWorkspace(context.Background(), workspaceID)
	if err != nil {
		return ""
	}
	return ws.IssuePrefix
}

func (s *TaskService) createAgentComment(ctx context.Context, issueID, agentID pgtype.UUID, content, commentType string, parentID, sourceTaskID pgtype.UUID) {
	if content == "" {
		return
	}
	// Look up issue to get workspace ID for mention expansion and broadcasting.
	issue, err := s.Queries.GetIssue(ctx, issueID)
	if err != nil {
		return
	}
	// Resolve the thread root for thread-level side effects without overwriting
	// parentID. The stored parent_id must remain the exact comment being replied
	// to; recursive thread reads recover the root when needed.
	var rootComment *db.Comment
	if parentID.Valid {
		if root, err := s.Queries.GetThreadRoot(ctx, db.GetThreadRootParams{
			CommentID:   parentID,
			WorkspaceID: issue.WorkspaceID,
		}); err == nil {
			rootComment = &root
		}
	}
	comment, err := s.Queries.CreateComment(ctx, db.CreateCommentParams{
		IssueID:      issueID,
		WorkspaceID:  issue.WorkspaceID,
		AuthorType:   "agent",
		AuthorID:     agentID,
		Content:      content,
		Type:         commentType,
		ParentID:     parentID,
		SourceTaskID: sourceTaskID,
	})
	if err != nil {
		return
	}
	s.CancelDeferredEscalationsForIssueAgent(ctx, issueID, agentID)
	s.Bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: util.UUIDToString(issue.WorkspaceID),
		ActorType:   "agent",
		ActorID:     util.UUIDToString(agentID),
		Payload: map[string]any{
			"comment": map[string]any{
				"id":             util.UUIDToString(comment.ID),
				"issue_id":       util.UUIDToString(comment.IssueID),
				"author_type":    comment.AuthorType,
				"author_id":      util.UUIDToString(comment.AuthorID),
				"content":        comment.Content,
				"type":           comment.Type,
				"parent_id":      util.UUIDToPtr(comment.ParentID),
				"source_task_id": util.UUIDToPtr(comment.SourceTaskID),
				"created_at":     comment.CreatedAt.Time.Format("2006-01-02T15:04:05Z"),
			},
			"issue_title":  issue.Title,
			"issue_status": issue.Status,
		},
	})
	s.AutoUnresolveThreadOnReply(ctx, rootComment, util.UUIDToString(issue.WorkspaceID), "agent", util.UUIDToString(agentID))
}

// AutoUnresolveThreadOnReply clears resolved_at on the thread root when a
// reply lands in a resolved thread, and broadcasts comment:unresolved. Shared
// between the user-facing Handler.CreateComment path and the agent-facing
// TaskService.createAgentComment path so the resolved-then-replied state can
// never desync (one of the bugs Emacs flagged on PR #2300). Errors are logged
// — the reply itself already committed, the desync is recoverable on next read.
func (s *TaskService) AutoUnresolveThreadOnReply(ctx context.Context, parent *db.Comment, workspaceID, actorType, actorID string) {
	if parent == nil || !parent.ResolvedAt.Valid {
		return
	}
	updated, err := s.Queries.UnresolveComment(ctx, parent.ID)
	if err != nil {
		slog.Warn("auto-unresolve on reply failed", "error", err, "comment_id", util.UUIDToString(parent.ID))
		return
	}
	s.Bus.Publish(events.Event{
		Type:        protocol.EventCommentUnresolved,
		WorkspaceID: workspaceID,
		ActorType:   actorType,
		ActorID:     actorID,
		Payload: map[string]any{
			"comment": map[string]any{
				"id":               util.UUIDToString(updated.ID),
				"issue_id":         util.UUIDToString(updated.IssueID),
				"author_type":      updated.AuthorType,
				"author_id":        util.UUIDToString(updated.AuthorID),
				"content":          updated.Content,
				"type":             updated.Type,
				"parent_id":        util.UUIDToPtr(updated.ParentID),
				"created_at":       util.TimestampToString(updated.CreatedAt),
				"updated_at":       util.TimestampToString(updated.UpdatedAt),
				"resolved_at":      util.TimestampToPtr(updated.ResolvedAt),
				"resolved_by_type": util.TextToPtr(updated.ResolvedByType),
				"resolved_by_id":   util.UUIDToPtr(updated.ResolvedByID),
			},
		},
	})
}

func issueToMap(issue db.Issue, issuePrefix string) map[string]any {
	return map[string]any{
		"id":              util.UUIDToString(issue.ID),
		"workspace_id":    util.UUIDToString(issue.WorkspaceID),
		"number":          issue.Number,
		"identifier":      issuePrefix + "-" + strconv.Itoa(int(issue.Number)),
		"title":           issue.Title,
		"description":     util.TextToPtr(issue.Description),
		"status":          issue.Status,
		"priority":        issue.Priority,
		"assignee_type":   util.TextToPtr(issue.AssigneeType),
		"assignee_id":     util.UUIDToPtr(issue.AssigneeID),
		"creator_type":    issue.CreatorType,
		"creator_id":      util.UUIDToString(issue.CreatorID),
		"parent_issue_id": util.UUIDToPtr(issue.ParentIssueID),
		"position":        issue.Position,
		"start_date":      util.DateToPtr(issue.StartDate),
		"due_date":        util.DateToPtr(issue.DueDate),
		"created_at":      util.TimestampToString(issue.CreatedAt),
		"updated_at":      util.TimestampToString(issue.UpdatedAt),
	}
}

// parseQuickCreateContext returns the quick-create payload if the task's
// context JSONB contains type == "quick_create"; otherwise the bool is
// false so callers can short-circuit. Tasks linked to an issue / chat /
// autopilot are never quick-create even if they happen to carry a
// context blob, so those are filtered up front.
func (s *TaskService) parseQuickCreateContext(task db.AgentTaskQueue) (QuickCreateContext, bool) {
	if task.IssueID.Valid || task.ChatSessionID.Valid || task.AutopilotRunID.Valid {
		return QuickCreateContext{}, false
	}
	if len(task.Context) == 0 {
		return QuickCreateContext{}, false
	}
	var qc QuickCreateContext
	if err := json.Unmarshal(task.Context, &qc); err != nil {
		return QuickCreateContext{}, false
	}
	if qc.Type != QuickCreateContextType {
		return QuickCreateContext{}, false
	}
	return qc, true
}

// notifyQuickCreateCompleted writes a success inbox notification to the
// requester pointing at the issue the agent just created. The issue is
// stamped with origin_type=quick_create + origin_id=<task_id> by the
// daemon-injected MULTICA_QUICK_CREATE_TASK_ID env var, so this lookup is
// deterministic — robust against the same agent creating other issues in
// parallel (e.g. assignment task running while max_concurrent_tasks > 1
// permits another quick-create alongside it).
func (s *TaskService) notifyQuickCreateCompleted(ctx context.Context, task db.AgentTaskQueue, qc QuickCreateContext) {
	requesterID, err := util.ParseUUID(qc.RequesterID)
	if err != nil {
		slog.Warn("quick-create completion: invalid requester id", "task_id", util.UUIDToString(task.ID), "error", err)
		return
	}
	workspaceID, err := util.ParseUUID(qc.WorkspaceID)
	if err != nil {
		slog.Warn("quick-create completion: invalid workspace id", "task_id", util.UUIDToString(task.ID), "error", err)
		return
	}
	issue, err := s.Queries.GetIssueByOrigin(ctx, db.GetIssueByOriginParams{
		WorkspaceID: workspaceID,
		OriginType:  pgtype.Text{String: "quick_create", Valid: true},
		OriginID:    task.ID,
	})
	if err != nil {
		// No issue created — agent ran to completion but the CLI call must
		// have failed. Surface as a failure inbox so the user sees something.
		slog.Warn("quick-create completion: no issue found, writing failure inbox",
			"task_id", util.UUIDToString(task.ID),
			"agent_id", util.UUIDToString(task.AgentID),
			"workspace_id", qc.WorkspaceID,
		)
		s.notifyQuickCreateFailed(ctx, task, qc, "agent finished without creating an issue")
		return
	}

	// Link the new issue back to this task so subsequent reads of the task
	// (Activity tab, Recent work, etc.) render it as a normal issue task
	// (kind = "direct") instead of staying on the "Creating issue" active-
	// wording label. Best-effort: a write failure here doesn't block the
	// inbox notification, which is the more important signal to the user.
	if err := s.Queries.LinkTaskToIssue(ctx, db.LinkTaskToIssueParams{
		ID:      task.ID,
		IssueID: issue.ID,
	}); err != nil {
		slog.Warn("quick-create completion: link task→issue failed",
			"task_id", util.UUIDToString(task.ID),
			"issue_id", util.UUIDToString(issue.ID),
			"error", err,
		)
	}

	// Subscribe the requester so they receive notifications for follow-up
	// comments and updates. The DB row's creator_type/creator_id is the
	// agent (it ran the CLI), but the human who triggered the quick-create
	// is the semantic creator from a UX perspective — without this they
	// only see the one-shot completion inbox and miss everything after.
	// Best-effort: log on failure but don't block the inbox notification.
	if err := s.Queries.AddIssueSubscriber(ctx, db.AddIssueSubscriberParams{
		IssueID:  issue.ID,
		UserType: "member",
		UserID:   requesterID,
		Reason:   "creator",
	}); err != nil {
		slog.Warn("quick-create completion: subscribe requester failed",
			"task_id", util.UUIDToString(task.ID),
			"issue_id", util.UUIDToString(issue.ID),
			"requester_id", qc.RequesterID,
			"error", err,
		)
	} else {
		s.Bus.Publish(events.Event{
			Type:        protocol.EventSubscriberAdded,
			WorkspaceID: qc.WorkspaceID,
			ActorType:   "agent",
			ActorID:     util.UUIDToString(task.AgentID),
			Payload: map[string]any{
				"issue_id":  util.UUIDToString(issue.ID),
				"user_type": "member",
				"user_id":   qc.RequesterID,
				"reason":    "creator",
			},
		})
	}
	prefix := s.getIssuePrefix(workspaceID)
	identifier := fmt.Sprintf("%s-%d", prefix, issue.Number)
	details, _ := json.Marshal(map[string]any{
		"task_id":         util.UUIDToString(task.ID),
		"agent_id":        util.UUIDToString(task.AgentID),
		"issue_id":        util.UUIDToString(issue.ID),
		"identifier":      identifier,
		"original_prompt": qc.Prompt,
	})
	item, err := s.Queries.CreateInboxItem(ctx, db.CreateInboxItemParams{
		WorkspaceID:   workspaceID,
		RecipientType: "member",
		RecipientID:   requesterID,
		Type:          "quick_create_done",
		Severity:      "info",
		IssueID:       issue.ID,
		Title:         issue.Title,
		Body:          pgtype.Text{},
		ActorType:     pgtype.Text{String: "agent", Valid: true},
		ActorID:       task.AgentID,
		Details:       details,
	})
	if err != nil {
		slog.Error("quick-create completion: inbox write failed", "task_id", util.UUIDToString(task.ID), "error", err)
		return
	}
	s.publishQuickCreateInbox(item, qc.WorkspaceID, util.UUIDToString(task.AgentID), issue.Status)
}

// notifyQuickCreateFailed writes a failure inbox notification carrying the
// original prompt + agent ID so the frontend can render an "Edit as
// advanced form" entry that pre-fills the legacy create-issue modal
// without asking the user to retype.
func (s *TaskService) notifyQuickCreateFailed(ctx context.Context, task db.AgentTaskQueue, qc QuickCreateContext, errMsg string) {
	requesterID, err := util.ParseUUID(qc.RequesterID)
	if err != nil {
		return
	}
	workspaceID, err := util.ParseUUID(qc.WorkspaceID)
	if err != nil {
		return
	}
	if errMsg == "" {
		errMsg = "Quick create did not finish successfully"
	}
	details, _ := json.Marshal(map[string]any{
		"task_id":         util.UUIDToString(task.ID),
		"agent_id":        util.UUIDToString(task.AgentID),
		"original_prompt": qc.Prompt,
		"error":           redact.Text(errMsg),
	})
	item, err := s.Queries.CreateInboxItem(ctx, db.CreateInboxItemParams{
		WorkspaceID:   workspaceID,
		RecipientType: "member",
		RecipientID:   requesterID,
		Type:          "quick_create_failed",
		Severity:      "action_required",
		IssueID:       pgtype.UUID{},
		Title:         "Quick create failed",
		Body:          pgtype.Text{String: redact.Text(errMsg), Valid: true},
		ActorType:     pgtype.Text{String: "agent", Valid: true},
		ActorID:       task.AgentID,
		Details:       details,
	})
	if err != nil {
		slog.Error("quick-create failure: inbox write failed", "task_id", util.UUIDToString(task.ID), "error", err)
		return
	}
	s.publishQuickCreateInbox(item, qc.WorkspaceID, util.UUIDToString(task.AgentID), "")
}

// publishQuickCreateInbox emits the WS event so the requester's inbox list
// updates immediately. Mirrors the payload shape used by the other inbox
// listeners (notification_listeners.go).
func (s *TaskService) publishQuickCreateInbox(item db.InboxItem, workspaceID, agentID, issueStatus string) {
	resp := map[string]any{
		"id":             util.UUIDToString(item.ID),
		"workspace_id":   util.UUIDToString(item.WorkspaceID),
		"recipient_type": item.RecipientType,
		"recipient_id":   util.UUIDToString(item.RecipientID),
		"type":           item.Type,
		"severity":       item.Severity,
		"issue_id":       util.UUIDToPtr(item.IssueID),
		"title":          item.Title,
		"body":           util.TextToPtr(item.Body),
		"read":           item.Read,
		"archived":       item.Archived,
		"created_at":     util.TimestampToString(item.CreatedAt),
		"actor_type":     util.TextToPtr(item.ActorType),
		"actor_id":       util.UUIDToPtr(item.ActorID),
		"details":        json.RawMessage(item.Details),
		"issue_status":   issueStatus,
	}
	s.Bus.Publish(events.Event{
		Type:        protocol.EventInboxNew,
		WorkspaceID: workspaceID,
		ActorType:   "agent",
		ActorID:     agentID,
		Payload:     map[string]any{"item": resp},
	})
}

// agentToMap builds a simple map for broadcasting agent status updates.
func agentToMap(a db.Agent) map[string]any {
	var rc any
	if a.RuntimeConfig != nil {
		json.Unmarshal(a.RuntimeConfig, &rc)
	}
	return map[string]any{
		"id":                   util.UUIDToString(a.ID),
		"workspace_id":         util.UUIDToString(a.WorkspaceID),
		"runtime_id":           util.UUIDToString(a.RuntimeID),
		"name":                 a.Name,
		"description":          a.Description,
		"avatar_url":           util.TextToPtr(a.AvatarUrl),
		"runtime_mode":         a.RuntimeMode,
		"runtime_config":       rc,
		"visibility":           a.Visibility,
		"status":               a.Status,
		"max_concurrent_tasks": a.MaxConcurrentTasks,
		"owner_id":             util.UUIDToPtr(a.OwnerID),
		"skills":               []any{},
		"created_at":           util.TimestampToString(a.CreatedAt),
		"updated_at":           util.TimestampToString(a.UpdatedAt),
		"archived_at":          util.TimestampToPtr(a.ArchivedAt),
		"archived_by":          util.UUIDToPtr(a.ArchivedBy),
	}
}
