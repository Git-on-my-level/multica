package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/attribution"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

const memberRescopeRequired = "member_rescope_required"

const memberRescopeMessage = "Member re-scope required. A workspace member must post a new comment beginning with RESCOPE: and describe a genuinely different task. " + taskfailure.CodexSafetyRefusalDiagnostic

// HasMemberRescopeDirective accepts a directive at the start of a newly
// authored comment, with a scope on the same line. Quoted or fenced copies
// and a bare marker do not authorize a run.
func HasMemberRescopeDirective(body string) bool {
	line, _, _ := strings.Cut(body, "\n")
	remainder, marked := strings.CutPrefix(line, "RESCOPE:")
	return marked && strings.TrimSpace(remainder) != ""
}

func qualifiesForMemberRescope(task db.AgentTaskQueue) bool {
	return task.IssueID.Valid && task.AgentID.Valid && !task.ChatSessionID.Valid &&
		task.FailureReason.String == taskfailure.ReasonCodexSafetyRefusal.String() &&
		strings.TrimSpace(task.Error.String) == taskfailure.CodexSafetyRefusalDiagnostic
}

func isDirectMemberComment(task db.AgentTaskQueue) bool {
	return task.TriggerCommentID.Valid && task.TriggerEvidenceRefID == task.TriggerCommentID &&
		task.OriginatorUserID.Valid && task.OriginatorSource.String == string(attribution.SourceDirectHuman) &&
		task.TriggerEvidenceKind.String == string(attribution.EvidenceComment) &&
		!task.DelegatedFromTaskID.Valid && !task.RetryOfTaskID.Valid && !task.RerunOfTaskID.Valid
}

type memberRescopeStore interface {
	GetMemberRescopeReceipt(context.Context, db.GetMemberRescopeReceiptParams) (db.MemberRescopeReceipt, error)
	GetMemberRescopeTrigger(context.Context, db.GetMemberRescopeTriggerParams) (db.GetMemberRescopeTriggerRow, error)
	AdmitMemberRescopeTask(context.Context, db.AdmitMemberRescopeTaskParams) (db.AgentTaskQueue, error)
	CancelAgentTaskWithReason(context.Context, db.CancelAgentTaskWithReasonParams) (db.AgentTaskQueue, error)
}

// applyMemberRescope runs under the per-agent claim lock. Every supported
// trigger reaches this boundary before the runtime receives a task. Receipt
// reads are distinguished from mutations so only missing/unreadable evidence
// fails open. A failed write rolls the claim back.
func applyMemberRescope(ctx context.Context, store memberRescopeStore, task db.AgentTaskQueue) (db.AgentTaskQueue, bool, error) {
	receipt, err := store.GetMemberRescopeReceipt(ctx, db.GetMemberRescopeReceiptParams{
		IssueID: task.IssueID,
		AgentID: task.AgentID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return task, false, nil
	}
	if err != nil {
		return task, true, err
	}
	if !receipt.ArmedAt.Valid || receipt.AdmittedTaskID.Valid != receipt.AdmittedAt.Valid {
		return task, false, nil
	}
	if receipt.AdmittedTaskID == task.ID {
		return task, false, nil
	}

	var trigger db.GetMemberRescopeTriggerRow
	if isDirectMemberComment(task) {
		trigger, err = store.GetMemberRescopeTrigger(ctx, db.GetMemberRescopeTriggerParams{
			CommentID:        task.TriggerCommentID,
			IssueID:          task.IssueID,
			OriginatorUserID: task.OriginatorUserID,
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return task, true, err
		}
	}

	if receipt.AdmittedAt.Valid {
		// A queued predecessor or replay of an older comment cannot ride
		// through a different task's one-shot authorization. A fresh comment
		// can replace an older queued task through normal comment coalescing.
		created := task.CreatedAt
		if trigger.CreatedAt.Valid {
			created = trigger.CreatedAt
		}
		if !created.Valid || created.Time.After(receipt.AdmittedAt.Time) {
			return task, false, nil
		}
	} else if trigger.Eligible.Valid && trigger.Eligible.Bool && trigger.CreatedAt.Valid &&
		trigger.CreatedAt.Time.After(receipt.ArmedAt.Time) {
		admitted, err := store.AdmitMemberRescopeTask(ctx, db.AdmitMemberRescopeTaskParams{
			IssueID:       task.IssueID,
			AgentID:       task.AgentID,
			RefusalTaskID: receipt.RefusalTaskID,
			ArmedAt:       receipt.ArmedAt,
			TaskID:        task.ID,
		})
		return admitted, false, err
	}

	cancelled, err := store.CancelAgentTaskWithReason(ctx, db.CancelAgentTaskWithReasonParams{
		ID:            task.ID,
		Error:         pgtype.Text{String: memberRescopeMessage, Valid: true},
		FailureReason: pgtype.Text{String: memberRescopeRequired, Valid: true},
	})
	return cancelled, false, err
}

func (s *TaskService) guardMemberRescopeClaim(ctx context.Context, tx pgx.Tx, task db.AgentTaskQueue) (db.AgentTaskQueue, error) {
	if tx == nil || !task.IssueID.Valid || task.ChatSessionID.Valid {
		return task, nil
	}
	// PostgreSQL aborts a transaction on a failed statement. Isolate the
	// optional evidence reads so unreadable receipts can really fail open.
	check, err := tx.Begin(ctx)
	if err != nil {
		return task, err
	}
	defer check.Rollback(ctx)
	outcome, readFailed, err := applyMemberRescope(ctx, s.Queries.WithTx(check), task)
	if readFailed {
		if rollbackErr := check.Rollback(ctx); rollbackErr != nil {
			return task, rollbackErr
		}
		slog.Warn("member re-scope evidence unreadable", "task_id", util.UUIDToString(task.ID))
		return task, nil
	}
	if err != nil {
		return task, err
	}
	return outcome, check.Commit(ctx)
}

func (s *TaskService) recordMemberRescopeRefusal(ctx context.Context, tx pgx.Tx, task db.AgentTaskQueue) error {
	if tx == nil || !qualifiesForMemberRescope(task) {
		return nil
	}
	check, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer check.Rollback(ctx)
	err = s.Queries.WithTx(check).ArmMemberRescopeReceipt(ctx, db.ArmMemberRescopeReceiptParams{
		IssueID: task.IssueID, AgentID: task.AgentID, RefusalTaskID: task.ID,
	})
	if err != nil {
		if rollbackErr := check.Rollback(ctx); rollbackErr != nil {
			return rollbackErr
		}
		// The provider failure is the authoritative record. A storage
		// problem writing the derived receipt must not roll it back.
		slog.Warn("member re-scope receipt could not be armed", "task_id", util.UUIDToString(task.ID))
		return nil
	}
	return check.Commit(ctx)
}
