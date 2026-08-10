package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/issueguard"
	"github.com/multica-ai/multica/server/internal/issueposition"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// IssueService is the single service-layer entry point for creating issues.
// Both the HTTP `POST /issues` handler and the future Lark `/issue` command
// call into Create so that duplicate guard, issue numbering, attachment
// linking, broadcast, analytics, and agent/squad enqueue stay aligned. The
// service deliberately does NOT depend on http.Request — callers parse
// their own transport and pass a fully-resolved IssueCreateParams.
type IssueService struct {
	Queries   *db.Queries
	TxStarter TxStarter
	Bus       *events.Bus
	Analytics analytics.Client
	// Metrics is the shared business-metrics collector. Wired by
	// cmd/server/router.go after construction; nil in tests / self-hosted
	// without the metrics listener — obsmetrics.RecordEvent treats a nil
	// Metrics as "PostHog only", so leaving it unset is safe.
	Metrics     *obsmetrics.BusinessMetrics
	TaskService *TaskService
}

func NewIssueService(q *db.Queries, tx TxStarter, bus *events.Bus, ac analytics.Client, ts *TaskService) *IssueService {
	return &IssueService{
		Queries:     q,
		TxStarter:   tx,
		Bus:         bus,
		Analytics:   ac,
		TaskService: ts,
	}
}

// IssueCreateParams carries the already-validated, already-resolved inputs
// to IssueService.Create. The handler owns the parsing step that turns its
// request payload into this struct; the service stays transport-agnostic.
type IssueCreateParams struct {
	WorkspaceID   pgtype.UUID
	Title         string
	Description   pgtype.Text
	Status        string
	Priority      string
	AssigneeType  pgtype.Text
	AssigneeID    pgtype.UUID
	CreatorType   string // "agent" or "member"
	CreatorID     pgtype.UUID
	ParentIssueID pgtype.UUID
	ProjectID     pgtype.UUID
	StartDate     pgtype.Date
	DueDate       pgtype.Date
	OriginType    pgtype.Text
	OriginID      pgtype.UUID
	AttachmentIDs []pgtype.UUID
	// LabelIDs are the issue-scoped labels to attach to the new issue. They
	// are validated and written inside the create transaction (see Create),
	// so the issue is never committed with a partial or wrong label set. An
	// unknown or non-issue label id fails the whole create with
	// ErrIssueLabelNotFound rather than being silently dropped.
	LabelIDs       []pgtype.UUID
	AllowDuplicate bool
	// Stage groups this issue into an ordered barrier group under its parent
	// (NULL = unstaged). See issue_child_done.go for the staged-barrier wake.
	Stage pgtype.Int4
	// ClientKey is an optional, caller-generated sha256:<64 lowercase hex>
	// idempotency key. Its binding is workspace-scoped and committed in the
	// same transaction as the issue row.
	ClientKey string
}

// IssueCreateOpts groups optional knobs for IssueService.Create. Most
// callers leave it zero-valued.
type IssueCreateOpts struct {
	// BroadcastPayload, if non-nil, is invoked after the issue row is
	// created and attachments are linked. Its return value is sent as
	// the EventIssueCreated payload via the event bus. The HTTP handler
	// uses this hook to inject its IssueResponse without forcing this
	// package to depend on handler-layer types. If nil, the service
	// emits a minimal `{"issue_id": <uuid>}` payload — enough for cache
	// invalidation, but front-ends that expect the full response shape
	// must provide BroadcastPayload. The labels argument is the authoritative
	// snapshot attached in the create transaction, so the emitted payload can
	// carry the new issue's labels and every online client renders it already
	// labeled instead of blank until a refetch.
	BroadcastPayload func(issue db.Issue, attachments []db.Attachment, labels []db.IssueLabel) map[string]any

	// ActorID overrides the actor ID used for broadcast + analytics
	// when it differs from the creator on the row. Agent-created issues
	// use the agent UUID here (the creator_id column is the daemon
	// owner). Empty falls back to CreatorID.
	ActorID string

	// AnalyticsAgentID is the agent associated with the issue for
	// analytics purposes (assignee agent or, for agent-created issues,
	// the creator agent). Resolved by the caller because it depends on
	// transport context.
	AnalyticsAgentID string

	// Platform tags the IssueCreated analytics + business-metrics event
	// with the client surface the request came in on (web / desktop /
	// daemon / lark / autopilot). Derived from middleware's client
	// metadata at the handler layer.
	Platform string
}

// ErrActiveDuplicate signals that the duplicate guard found an active
// issue with the same (workspace, project, parent, title) tuple and
// AllowDuplicate was false. The IssueCreateResult.DuplicateIssue field is
// populated when this error is returned so callers can render the
// conflict (HTTP 409, Lark card, etc.).
var ErrActiveDuplicate = errors.New("active duplicate issue exists")

// ErrParentIssueNotFound signals that the supplied ParentIssueID does
// not exist in the issue's workspace. The service refuses to create
// orphaned or cross-workspace child issues; callers translate this into
// their transport's 400 / Lark card error.
var ErrParentIssueNotFound = errors.New("parent issue not found in this workspace")

// ErrProjectNotFound signals that the supplied ProjectID does not exist
// in the issue's workspace. Cross-workspace project IDs are rejected
// here so every create entry (HTTP `POST /issues`, Lark `/issue`, future
// MCP / API key callers) enforces the same workspace boundary without
// having to remember it. Callers translate this into 400.
var ErrProjectNotFound = errors.New("project not found in this workspace")

// ErrIssueLabelNotFound signals that one of the supplied LabelIDs does not
// exist in the issue's workspace or is not an issue-scoped label. The whole
// create is rejected so a new issue is never born with a partial or wrong
// label set. Callers translate this into their transport's 400.
var ErrIssueLabelNotFound = errors.New("issue label not found in this workspace")

// ErrIssueClientKeyConflict means this workspace already bound the supplied
// client key to a create request with different semantic inputs.
var ErrIssueClientKeyConflict = errors.New("issue client key already used with different semantics")

// ErrIssueClientKeyTargetMissing means the key remains permanently consumed,
// but its issue was later deleted. Retrying must not silently recreate it.
var ErrIssueClientKeyTargetMissing = errors.New("issue client key target no longer exists")

// ErrIssueClientKeyAssignedDispatchUnsupported prevents a client-key create
// from claiming crash-safe promotion when the successful create would enqueue
// an agent/squad task only after commit. Backlog assignments do not dispatch
// and remain eligible.
var ErrIssueClientKeyAssignedDispatchUnsupported = errors.New("issue client key does not support assigned creates that dispatch immediately")

// ErrInvalidIssueClientKey means a non-empty key did not use the canonical
// sha256:<64 lowercase hex> wire format.
var ErrInvalidIssueClientKey = errors.New("invalid issue client key")

// IssueCreateResult is the typed return from IssueService.Create.
//
//   - On the happy path: Issue is the new row, Attachments lists the
//     linked attachments (may be empty), DuplicateIssue is nil.
//   - On ErrActiveDuplicate: DuplicateIssue is the row that blocked the
//     create; Issue and Attachments are zero.
type IssueCreateResult struct {
	Issue       db.Issue
	Attachments []db.Attachment
	// Labels is the authoritative set of labels attached to the new issue in
	// the create transaction (empty when none were requested). Callers echo it
	// on the create response + issue:created event so every client renders the
	// new issue already labeled and a new client can detect that the backend
	// understood label_ids (see the create handler's compatibility contract).
	Labels         []db.IssueLabel
	DuplicateIssue *db.Issue
	// Reused is true when ClientKey resolved an already-created issue. The
	// caller must not interpret this as another create transition.
	Reused bool
}

// Create runs the full issue-creation pipeline atomically end-to-end:
//
//  1. Begin transaction.
//  2. Resolve & validate parent / project belong to the same workspace.
//  3. Lock & check the duplicate guard.
//  4. Increment the workspace issue counter.
//  5. Insert the issue row (with optional origin stamping).
//  6. Commit.
//  7. Link any pre-uploaded attachments (post-commit, idempotent).
//  8. Publish EventIssueCreated to the bus (payload via opts.BroadcastPayload).
//  9. Capture the IssueCreated analytics event.
//  10. Enqueue an agent task or trigger the squad leader when the issue is
//     assigned and not in `backlog`.
//
// Validation that lives in the service (parent existence, project
// workspace membership, parent → project back-fill) is enforced here so
// every create entry — HTTP `POST /issues`, Lark `/issue`, future
// MCP/API-key callers — shares the same workspace boundary semantics.
// Caller-owned validation is limited to transport-shaped checks: title
// required, RFC3339 date format, assignee pair sanity.
func (s *IssueService) Create(ctx context.Context, p IssueCreateParams, opts IssueCreateOpts) (IssueCreateResult, error) {
	clientKeyHash := ""
	semanticDigest := ""
	if p.ClientKey != "" {
		var err error
		clientKeyHash, err = util.ParseSHA256ClientKey(p.ClientKey)
		if err != nil {
			return IssueCreateResult{}, ErrInvalidIssueClientKey
		}
		if p.Status != "backlog" && p.AssigneeType.Valid &&
			(p.AssigneeType.String == "agent" || p.AssigneeType.String == "squad") {
			return IssueCreateResult{}, ErrIssueClientKeyAssignedDispatchUnsupported
		}
		semanticDigest, err = issueCreateSemanticDigest(p)
		if err != nil {
			return IssueCreateResult{}, fmt.Errorf("digest issue create semantics: %w", err)
		}
	}

	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return IssueCreateResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)

	if p.ClientKey != "" {
		reserved, err := qtx.ReserveIssueCreateClientKey(ctx, db.ReserveIssueCreateClientKeyParams{
			WorkspaceID:    p.WorkspaceID,
			ClientKeyHash:  clientKeyHash,
			SemanticDigest: semanticDigest,
		})
		if err != nil {
			return IssueCreateResult{}, fmt.Errorf("reserve issue client key: %w", err)
		}
		if reserved == 0 {
			binding, err := qtx.GetIssueCreateClientKey(ctx, db.GetIssueCreateClientKeyParams{
				WorkspaceID:   p.WorkspaceID,
				ClientKeyHash: clientKeyHash,
			})
			if err != nil {
				return IssueCreateResult{}, fmt.Errorf("read issue client key: %w", err)
			}
			if binding.SemanticDigest != semanticDigest {
				return IssueCreateResult{}, ErrIssueClientKeyConflict
			}
			if !binding.IssueID.Valid {
				return IssueCreateResult{}, fmt.Errorf("issue client key has no bound issue")
			}
			issue, err := qtx.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{
				ID:          binding.IssueID,
				WorkspaceID: p.WorkspaceID,
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return IssueCreateResult{}, ErrIssueClientKeyTargetMissing
				}
				return IssueCreateResult{}, fmt.Errorf("read issue client key target: %w", err)
			}
			// Complete a best-effort attachment link if the authority committed
			// the issue but the original process crashed before its post-commit
			// attachment step. LinkAttachmentsToIssue only updates unbound rows.
			if len(p.AttachmentIDs) > 0 {
				if err := qtx.LinkAttachmentsToIssue(ctx, db.LinkAttachmentsToIssueParams{
					IssueID:     issue.ID,
					WorkspaceID: issue.WorkspaceID,
					Column3:     p.AttachmentIDs,
				}); err != nil {
					slog.Warn("retry attachment link failed", "issue_id", util.UUIDToString(issue.ID), "error", err)
				}
			}
			attachments, err := qtx.ListAttachmentsByIssue(ctx, db.ListAttachmentsByIssueParams{
				IssueID: issue.ID, WorkspaceID: p.WorkspaceID,
			})
			if err != nil {
				return IssueCreateResult{}, fmt.Errorf("list replayed issue attachments: %w", err)
			}
			labels, err := qtx.ListLabelsByIssue(ctx, db.ListLabelsByIssueParams{
				IssueID: issue.ID, WorkspaceID: p.WorkspaceID,
			})
			if err != nil {
				return IssueCreateResult{}, fmt.Errorf("list replayed issue labels: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return IssueCreateResult{}, fmt.Errorf("commit issue client key replay: %w", err)
			}
			return IssueCreateResult{Issue: issue, Attachments: attachments, Labels: labels, Reused: true}, nil
		}
	}

	// Resolve and validate parent / project before reading from the
	// duplicate guard so a forged parent or project ID is rejected
	// before we touch the issue counter. Both checks scope by
	// WorkspaceID — there is no path from this service to a row in a
	// foreign workspace.
	projectID := p.ProjectID
	if p.ParentIssueID.Valid {
		parent, err := qtx.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{
			ID:          p.ParentIssueID,
			WorkspaceID: p.WorkspaceID,
		})
		if err != nil || !parent.ID.Valid {
			return IssueCreateResult{}, ErrParentIssueNotFound
		}
		// Back-fill project from parent when the caller did not pin
		// one explicitly. Matches the long-standing HTTP behavior: a
		// sub-issue inherits its parent's project unless overridden.
		if !projectID.Valid {
			projectID = parent.ProjectID
		}
	}
	if projectID.Valid {
		if _, err := qtx.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{
			ID:          projectID,
			WorkspaceID: p.WorkspaceID,
		}); err != nil {
			return IssueCreateResult{}, ErrProjectNotFound
		}
	}

	// Validate labels before we increment the issue counter so a stale or
	// wrong-scope selection fails the create cheaply. The de-duplicated rows
	// are attached to the issue below, inside this same transaction, and
	// echoed back as the authoritative snapshot in the result.
	labels, err := validateIssueLabels(ctx, qtx, p.WorkspaceID, p.LabelIDs)
	if err != nil {
		return IssueCreateResult{}, err
	}

	duplicate, found, err := issueguard.LockAndFindActiveDuplicate(ctx, qtx, p.WorkspaceID, projectID, p.ParentIssueID, p.Title, p.AllowDuplicate)
	if err != nil {
		return IssueCreateResult{}, fmt.Errorf("duplicate guard: %w", err)
	}
	if found {
		dup := duplicate
		return IssueCreateResult{DuplicateIssue: &dup}, ErrActiveDuplicate
	}

	issueNumber, err := qtx.IncrementIssueCounter(ctx, p.WorkspaceID)
	if err != nil {
		return IssueCreateResult{}, fmt.Errorf("increment counter: %w", err)
	}

	// New issues sort to the top of their (workspace, status) column for
	// manual ordering. Computed inside the tx, after IncrementIssueCounter
	// has already taken the workspace row lock, so two concurrent creates
	// in the same workspace see each other's positions and don't both
	// land on the same min-1 slot. Concurrent manual reorder via
	// UpdateIssue(position) does NOT take this lock, so a create racing
	// a reorder is still allowed to collide on position — manual ordering
	// is best-effort and the UI tolerates equal positions by falling back
	// to the secondary ORDER BY key.
	newPosition, err := issueposition.NextTopPosition(ctx, tx, p.WorkspaceID, p.Status)
	if err != nil {
		return IssueCreateResult{}, fmt.Errorf("next top position: %w", err)
	}

	var issue db.Issue
	if p.OriginType.Valid {
		issue, err = qtx.CreateIssueWithOrigin(ctx, db.CreateIssueWithOriginParams{
			WorkspaceID:   p.WorkspaceID,
			Title:         p.Title,
			Description:   p.Description,
			Status:        p.Status,
			Priority:      p.Priority,
			AssigneeType:  p.AssigneeType,
			AssigneeID:    p.AssigneeID,
			CreatorType:   p.CreatorType,
			CreatorID:     p.CreatorID,
			ParentIssueID: p.ParentIssueID,
			Position:      newPosition,
			StartDate:     p.StartDate,
			DueDate:       p.DueDate,
			Number:        issueNumber,
			ProjectID:     projectID,
			OriginType:    p.OriginType,
			OriginID:      p.OriginID,
			Stage:         p.Stage,
		})
	} else {
		issue, err = qtx.CreateIssue(ctx, db.CreateIssueParams{
			WorkspaceID:   p.WorkspaceID,
			Title:         p.Title,
			Description:   p.Description,
			Status:        p.Status,
			Priority:      p.Priority,
			AssigneeType:  p.AssigneeType,
			AssigneeID:    p.AssigneeID,
			CreatorType:   p.CreatorType,
			CreatorID:     p.CreatorID,
			ParentIssueID: p.ParentIssueID,
			Position:      newPosition,
			StartDate:     p.StartDate,
			DueDate:       p.DueDate,
			Number:        issueNumber,
			ProjectID:     projectID,
			Stage:         p.Stage,
		})
	}
	if err != nil {
		return IssueCreateResult{}, fmt.Errorf("create issue: %w", err)
	}

	// Attach labels inside the create transaction so the issue and its
	// labels commit together — the old flow created the issue first and
	// attached labels in a second, non-atomic round-trip whose partial
	// failure left the issue mis-categorized. AttachLabelToIssue is
	// workspace/resource_type-guarded and ON CONFLICT DO NOTHING, and the
	// ids were already validated above.
	for _, label := range labels {
		if err := qtx.AttachLabelToIssue(ctx, db.AttachLabelToIssueParams{
			IssueID:     issue.ID,
			LabelID:     label.ID,
			WorkspaceID: p.WorkspaceID,
		}); err != nil {
			return IssueCreateResult{}, fmt.Errorf("attach issue label: %w", err)
		}
	}

	if clientKeyHash != "" {
		bound, err := qtx.BindIssueCreateClientKey(ctx, db.BindIssueCreateClientKeyParams{
			IssueID:        issue.ID,
			WorkspaceID:    p.WorkspaceID,
			ClientKeyHash:  clientKeyHash,
			SemanticDigest: semanticDigest,
		})
		if err != nil {
			return IssueCreateResult{}, fmt.Errorf("bind issue client key: %w", err)
		}
		if bound != 1 {
			return IssueCreateResult{}, fmt.Errorf("bind issue client key: expected one reservation, updated %d", bound)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return IssueCreateResult{}, fmt.Errorf("commit: %w", err)
	}

	attachments := s.linkAttachments(ctx, issue, p.AttachmentIDs)

	actorID := opts.ActorID
	if actorID == "" {
		actorID = util.UUIDToString(issue.CreatorID)
	}

	s.publishIssueCreated(issue, attachments, labels, p.CreatorType, actorID, opts)
	s.captureCreatedAnalytics(issue, p.CreatorType, actorID, opts)
	s.maybeEnqueueOnAssign(ctx, issue, p.CreatorType, actorID)

	return IssueCreateResult{Issue: issue, Attachments: attachments, Labels: labels}, nil
}

// issueCreateSemanticDigest hashes the normalized authority inputs without
// retaining any title, description, prompt, brief, or transcript in the
// idempotency table. Slice order is normalized because label/attachment sets
// are semantically unordered.
func issueCreateSemanticDigest(p IssueCreateParams) (string, error) {
	uuid := func(v pgtype.UUID) string {
		if !v.Valid {
			return ""
		}
		return util.UUIDToString(v)
	}
	text := func(v pgtype.Text) *string {
		if !v.Valid {
			return nil
		}
		value := v.String
		return &value
	}
	date := func(v pgtype.Date) string {
		if !v.Valid {
			return ""
		}
		return v.Time.Format("2006-01-02")
	}
	int4 := func(v pgtype.Int4) int32 {
		if !v.Valid {
			return 0
		}
		return v.Int32
	}
	uuidSet := func(values []pgtype.UUID) []string {
		out := make([]string, 0, len(values))
		seen := make(map[string]struct{}, len(values))
		for _, value := range values {
			if value.Valid {
				id := util.UUIDToString(value)
				if _, exists := seen[id]; !exists {
					seen[id] = struct{}{}
					out = append(out, id)
				}
			}
		}
		sort.Strings(out)
		return out
	}
	canonical := struct {
		Version        int      `json:"v"`
		Title          string   `json:"title"`
		Description    *string  `json:"description"`
		Status         string   `json:"status"`
		Priority       string   `json:"priority"`
		AssigneeType   *string  `json:"assignee_type"`
		AssigneeID     string   `json:"assignee_id"`
		CreatorType    string   `json:"creator_type"`
		CreatorID      string   `json:"creator_id"`
		ParentIssueID  string   `json:"parent_issue_id"`
		ProjectID      string   `json:"project_id"`
		StartDate      string   `json:"start_date"`
		DueDate        string   `json:"due_date"`
		OriginType     *string  `json:"origin_type"`
		OriginID       string   `json:"origin_id"`
		Stage          int32    `json:"stage"`
		AttachmentIDs  []string `json:"attachment_ids"`
		LabelIDs       []string `json:"label_ids"`
		AllowDuplicate bool     `json:"allow_duplicate"`
	}{
		Version: 1, Title: p.Title, Description: text(p.Description), Status: p.Status, Priority: p.Priority,
		AssigneeType: text(p.AssigneeType), AssigneeID: uuid(p.AssigneeID), CreatorType: p.CreatorType,
		CreatorID: uuid(p.CreatorID), ParentIssueID: uuid(p.ParentIssueID), ProjectID: uuid(p.ProjectID),
		StartDate: date(p.StartDate), DueDate: date(p.DueDate), OriginType: text(p.OriginType),
		OriginID: uuid(p.OriginID), Stage: int4(p.Stage), AttachmentIDs: uuidSet(p.AttachmentIDs),
		LabelIDs: uuidSet(p.LabelIDs), AllowDuplicate: p.AllowDuplicate,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// validateIssueLabels checks that every requested label exists in the
// workspace and is issue-scoped, returning the de-duplicated label rows to
// attach. Returning the full rows (not just ids) lets Create echo an
// authoritative label snapshot on the create response + issue:created event
// without a second query. It mirrors the workspace + resource_type='issue'
// guard already enforced by AttachLabelToIssue so an unknown or wrong-scope id
// surfaces as ErrIssueLabelNotFound instead of a silent no-op insert. Invalid
// (zero) UUIDs are skipped. The label count per issue is small, so a GetLabel
// per distinct id is fine and avoids introducing a new batch query.
func validateIssueLabels(ctx context.Context, qtx *db.Queries, workspaceID pgtype.UUID, labelIDs []pgtype.UUID) ([]db.IssueLabel, error) {
	if len(labelIDs) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(labelIDs))
	deduped := make([]db.IssueLabel, 0, len(labelIDs))
	for _, labelID := range labelIDs {
		if !labelID.Valid {
			continue
		}
		key := util.UUIDToString(labelID)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		label, err := qtx.GetLabel(ctx, db.GetLabelParams{
			ID:          labelID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrIssueLabelNotFound
			}
			return nil, fmt.Errorf("get issue label: %w", err)
		}
		if label.ResourceType != "issue" {
			return nil, ErrIssueLabelNotFound
		}
		deduped = append(deduped, label)
	}
	return deduped, nil
}

// linkAttachments links the given attachment IDs to the newly created
// issue and returns the re-fetched attachment rows so callers can build
// their response without a second query. Errors are logged and swallowed
// — attachment linking is a best-effort post-commit step, and a stale
// attachment row doesn't justify failing the whole create.
func (s *IssueService) linkAttachments(ctx context.Context, issue db.Issue, ids []pgtype.UUID) []db.Attachment {
	if len(ids) == 0 {
		return nil
	}
	if err := s.Queries.LinkAttachmentsToIssue(ctx, db.LinkAttachmentsToIssueParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
		Column3:     ids,
	}); err != nil {
		slog.Error("failed to link attachments to issue",
			"issue_id", util.UUIDToString(issue.ID),
			"error", err)
		return nil
	}
	list, err := s.Queries.ListAttachmentsByIssue(ctx, db.ListAttachmentsByIssueParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		slog.Warn("failed to list attachments for new issue",
			"issue_id", util.UUIDToString(issue.ID),
			"error", err)
		return nil
	}
	return list
}

func (s *IssueService) publishIssueCreated(issue db.Issue, attachments []db.Attachment, labels []db.IssueLabel, creatorType, actorID string, opts IssueCreateOpts) {
	if s.Bus == nil {
		return
	}
	var payload map[string]any
	if opts.BroadcastPayload != nil {
		payload = opts.BroadcastPayload(issue, attachments, labels)
	} else {
		// Minimal fallback so cache invalidations still fire even if the
		// caller forgot to supply a builder. Front-ends that expect the
		// full IssueResponse must pass BroadcastPayload.
		payload = map[string]any{"issue_id": util.UUIDToString(issue.ID)}
	}
	s.Bus.Publish(events.Event{
		Type:        protocol.EventIssueCreated,
		WorkspaceID: util.UUIDToString(issue.WorkspaceID),
		ActorType:   creatorType,
		ActorID:     actorID,
		Payload:     payload,
	})
}

func (s *IssueService) captureCreatedAnalytics(issue db.Issue, creatorType, actorID string, opts IssueCreateOpts) {
	if s.Analytics == nil {
		return
	}
	source, taskID, autopilotRunID := classifyOrigin(issue, opts)
	analyticsActorID := actorID
	if creatorType == "agent" {
		analyticsActorID = "agent:" + actorID
	}
	obsmetrics.RecordEvent(s.Analytics, s.Metrics, analytics.IssueCreated(
		analyticsActorID,
		util.UUIDToString(issue.WorkspaceID),
		util.UUIDToString(issue.ID),
		opts.AnalyticsAgentID,
		taskID,
		autopilotRunID,
		source,
		opts.Platform,
	))
}

// classifyOrigin maps the issue's origin_type / origin_id columns into the
// analytics source labels. Unknown origin_type falls back to SourceManual
// with the warning logged — analytics drift is preferable to dropping the
// event entirely.
func classifyOrigin(issue db.Issue, opts IssueCreateOpts) (source, taskID, autopilotRunID string) {
	source = analytics.SourceManual
	if !issue.OriginType.Valid {
		return source, "", ""
	}
	originID := util.UUIDToString(issue.OriginID)
	switch issue.OriginType.String {
	case "quick_create", "agent_create":
		// Both link the issue back to the agent_task_queue row that created it
		// (agent_create is the ordinary agent `issue create` path, MUL-4305);
		// surface that task id and keep the manual source label.
		return analytics.SourceManual, originID, ""
	case "autopilot":
		return analytics.SourceAutopilot, "", originID
	default:
		slog.Warn("analytics: unknown issue origin type",
			"origin_type", issue.OriginType.String,
			"issue_id", util.UUIDToString(issue.ID),
		)
		return analytics.SourceManual, "", ""
	}
}

func (s *IssueService) maybeEnqueueOnAssign(ctx context.Context, issue db.Issue, creatorType, actorID string) {
	if !issue.AssigneeType.Valid || !issue.AssigneeID.Valid {
		return
	}
	if s.shouldEnqueueAgentTask(ctx, issue) {
		if _, err := s.TaskService.EnqueueTaskForIssue(ctx, issue); err != nil {
			slog.Warn("enqueue agent task on create failed",
				"issue_id", util.UUIDToString(issue.ID),
				"error", err)
		}
	}
	if s.shouldEnqueueSquadLeaderOnAssign(ctx, issue) {
		s.enqueueSquadLeaderTask(ctx, issue, pgtype.UUID{}, creatorType, actorID)
	}
}

// shouldEnqueueAgentTask returns true when an issue create or assignment
// should trigger the assigned agent. Backlog issues are skipped — backlog
// acts as a parking lot for pre-assigning without immediate execution.
// Mirrors handler.shouldEnqueueAgentTask; kept here to make the service
// self-contained, since both code paths must move together.
func (s *IssueService) shouldEnqueueAgentTask(ctx context.Context, issue db.Issue) bool {
	if issue.Status == "backlog" {
		return false
	}
	return s.isAgentAssigneeReady(ctx, issue)
}

func (s *IssueService) isAgentAssigneeReady(ctx context.Context, issue db.Issue) bool {
	if !issue.AssigneeType.Valid || issue.AssigneeType.String != "agent" || !issue.AssigneeID.Valid {
		return false
	}
	agent, err := s.Queries.GetAgent(ctx, issue.AssigneeID)
	if err != nil || !agent.RuntimeID.Valid || agent.ArchivedAt.Valid {
		return false
	}
	return true
}

func (s *IssueService) shouldEnqueueSquadLeaderOnAssign(ctx context.Context, issue db.Issue) bool {
	if issue.Status == "backlog" {
		return false
	}
	return s.isSquadLeaderReady(ctx, issue)
}

func (s *IssueService) isSquadLeaderReady(ctx context.Context, issue db.Issue) bool {
	if !issue.AssigneeType.Valid || issue.AssigneeType.String != "squad" || !issue.AssigneeID.Valid {
		return false
	}
	squad, err := s.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{
		ID:          issue.AssigneeID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		return false
	}
	agent, err := s.Queries.GetAgent(ctx, squad.LeaderID)
	if err != nil {
		return false
	}
	ready, _, err := AgentReadiness(ctx, s.Queries, agent)
	if err != nil {
		return false
	}
	return ready
}

func (s *IssueService) enqueueSquadLeaderTask(ctx context.Context, issue db.Issue, triggerCommentID pgtype.UUID, authorType, authorID string) {
	squad, err := s.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{
		ID:          issue.AssigneeID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		return
	}
	hasPending, err := s.Queries.HasPendingTaskForIssueAndAgent(ctx, db.HasPendingTaskForIssueAndAgentParams{
		IssueID: issue.ID,
		AgentID: squad.LeaderID,
		// Key dedup on the reviewed head (TEN-356).
		HeadSha: headShaText(s.TaskService.ResolveIssueReviewSHA(ctx, issue.ID)),
	})
	if err != nil || hasPending {
		return
	}
	if _, err := s.TaskService.EnqueueTaskForSquadLeader(ctx, issue, squad.LeaderID, squad.ID, triggerCommentID); err != nil {
		slog.Warn("enqueue squad leader task on create failed",
			"issue_id", util.UUIDToString(issue.ID),
			"squad_id", util.UUIDToString(squad.ID),
			"leader_id", util.UUIDToString(squad.LeaderID),
			"error", err)
	}
}
