package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// RecoverChildAttention drains durable review/blocked handoffs. Attention is
// independent of terminal stage barriers: it asks the owner to review or help,
// never accepts a child or promotes the next stage. The database captures the
// signal atomically with the child status; this worker atomically replaces it
// with a comment and queued run. Neither crash window loses or duplicates work.
func (s *TaskService) RecoverChildAttention(ctx context.Context, limit int32) (int, error) {
	if limit < 1 || limit > 100 {
		limit = 32
	}
	parents, err := s.Queries.ListDueChildAttentionParents(ctx, limit)
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, parentID := range parents {
		changed, err := s.recoverChildAttentionParent(ctx, parentID)
		if err != nil {
			// Keep failed obligations durable, but don't let an unavailable owner
			// occupy every batch or cause a tight retry loop.
			slog.Warn("child attention: handoff deferred", "parent_id", util.UUIDToString(parentID), "error", err)
			continue
		}
		if changed {
			queued++
		}
	}
	return queued, nil
}

func (s *TaskService) recoverChildAttentionParent(ctx context.Context, parentID pgtype.UUID) (queued bool, err error) {
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return false, err
	}
	var ids, generations []pgtype.UUID
	defer func() {
		tx.Rollback(ctx)
		if err != nil && len(ids) > 0 {
			// Compare immutable signal generations after rollback: a fresh
			// child transition must not inherit an older delivery's retry delay.
			if deferErr := s.Queries.DeferChildAttention(ctx, db.DeferChildAttentionParams{ChildIds: ids, Generations: generations}); deferErr != nil {
				err = errors.Join(err, fmt.Errorf("defer child attention: %w", deferErr))
			}
		}
	}()
	q := s.Queries.WithTx(tx)
	locked, err := q.TryLockChildAttentionParent(ctx, parentID)
	if err != nil || !locked {
		return false, err
	}
	// Match runtime teardown and workspace deletion: workspace, runtime, agent,
	// issue, attention rows. Taking the issue first deadlocks against workspace
	// teardown when CreateAgentTask subsequently acquires its workspace fence.
	if _, err := q.LockChildAttentionWorkspace(ctx, parentID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	lockedRuntimeID, runtimeLockErr := q.LockChildAttentionRuntime(ctx, parentID)
	if runtimeLockErr != nil && !errors.Is(runtimeLockErr, pgx.ErrNoRows) {
		return false, runtimeLockErr
	}
	// Freeze runtime reassignment through enqueue, but never wait behind an
	// agent-first mutation while holding the runtime fence. Retry contention.
	lockedAgentID, agentLockErr := q.LockChildAttentionAgent(ctx, parentID)
	if agentLockErr != nil && !errors.Is(agentLockErr, pgx.ErrNoRows) {
		return false, agentLockErr
	}
	parent, parentErr := q.LockChildAttentionParent(ctx, parentID)
	if parentErr != nil && !errors.Is(parentErr, pgx.ErrNoRows) {
		return false, parentErr
	}
	rows, err := q.LockChildAttention(ctx, parentID)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	ids = make([]pgtype.UUID, 0, len(rows))
	generations = make([]pgtype.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ChildID)
		generations = append(generations, row.Generation)
	}
	settle := func() error {
		if err := q.DeleteChildAttention(ctx, ids); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if errors.Is(parentErr, pgx.ErrNoRows) {
		return false, settle()
	}
	// Preserve the established parked/closed/human-parent behavior. A parked
	// parent stays inert when observed by capture or delivery. A brief parked
	// interval between those observations does not cancel an existing handoff.
	status := issuestatus.Effective(ctx, q, parent.WorkspaceID, parent.Status)
	if status == "backlog" || status == "done" || status == "cancelled" || parent.Status == "triage" ||
		(parent.AssigneeType.Valid && parent.AssigneeType.String == "member") {
		return false, settle()
	}
	if !parent.AssigneeID.Valid || !parent.AssigneeType.Valid {
		return false, fmt.Errorf("parent has no owner")
	}
	var lines []string
	for _, row := range rows {
		child, err := q.GetIssue(ctx, row.ChildID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, err
		}
		if child.ParentIssueID != parent.ID || child.WorkspaceID != parent.WorkspaceID || row.WorkspaceID != parent.WorkspaceID ||
			issuestatus.Effective(ctx, q, child.WorkspaceID, child.Status) != row.Status {
			continue
		}
		// Titles are deliberately absent: untrusted titles cannot inject
		// instructions or mentions into the platform's coordination comment.
		lines = append(lines, fmt.Sprintf("- [Child #%d](mention://issue/%s): `%s`", child.Number, util.UUIDToString(child.ID), row.Status))
	}
	if len(lines) == 0 {
		return false, settle()
	}
	agentID := parent.AssigneeID
	var squadID pgtype.UUID
	switch parent.AssigneeType.String {
	case "agent":
	case "squad":
		squad, err := q.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: parent.AssigneeID, WorkspaceID: parent.WorkspaceID})
		if err != nil {
			return false, err
		}
		agentID, squadID = squad.LeaderID, squad.ID
	default:
		return false, fmt.Errorf("unsupported parent owner type")
	}
	if lockedAgentID != agentID {
		return false, fmt.Errorf("parent owner changed while acquiring handoff locks")
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: parent.WorkspaceID})
	if err != nil {
		return false, err
	}
	if agent.RuntimeID != lockedRuntimeID {
		return false, fmt.Errorf("parent runtime changed while acquiring handoff locks")
	}
	if agent.ArchivedAt.Valid || !agent.RuntimeID.Valid {
		return false, fmt.Errorf("parent owner is archived or has no runtime")
	}
	content := "Child issues need your attention:\n" + strings.Join(lines, "\n") +
		"\n\nReview delivered work or help resolve the blocker, then continue coordinating the parent. " +
		"This is an attention handoff, not acceptance or a completed stage. Leave delivered children in `in_review`; " +
		"do not mark them `done` or advance a stage without the plan's required acceptance. " +
		"Read the parent plan and current child state before acting."
	comment, err := q.CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), IssueID: parent.ID, WorkspaceID: parent.WorkspaceID,
		AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Content: content, Type: "system",
	})
	if err != nil {
		return false, err
	}
	// Use existing attribution, archive, triage and runtime-overlay policy,
	// but no pre-commit broadcast or wake. A distinct comment thread preserves
	// this obligation behind an unrelated queued/dispatched/running parent run.
	writer := NewTaskService(q, s.TxStarter, nil, events.New())
	writer.FeatureFlags, writer.Composio, writer.Entitlements = s.FeatureFlags, s.Composio, s.Entitlements
	var task db.AgentTaskQueue
	if squadID.Valid {
		task, err = writer.EnqueueTaskForSquadLeader(ctx, parent, agentID, squadID, comment.ID, OriginDerived)
	} else {
		task, err = writer.EnqueueTaskForMention(ctx, parent, agentID, comment.ID, OriginDerived)
	}
	if err != nil {
		return false, err
	}
	if err := settle(); err != nil {
		return false, err
	}
	// These are latency hints only. The committed queue remains claimable if
	// the process exits before either notification is delivered.
	if s.Bus != nil {
		s.Bus.Publish(events.Event{
			Type: protocol.EventCommentCreated, WorkspaceID: util.UUIDToString(parent.WorkspaceID), ActorType: "system",
			Payload: map[string]any{"comment": commentEventFields(comment.Comment()), "issue_title": parent.Title,
				"issue_status": parent.Status, "issue_revision": comment.IssueRevision},
		})
		s.broadcastTaskEvent(ctx, protocol.EventTaskQueued, task)
	}
	s.NotifyTaskEnqueued(ctx, task)
	return true, nil
}
