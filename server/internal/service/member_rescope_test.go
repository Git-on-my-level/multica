package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

func rescopeID(n byte) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte{n}, Valid: true}
}

type rescopeFixture struct {
	task       db.AgentTaskQueue
	receipt    db.MemberRescopeReceipt
	trigger    db.GetMemberRescopeTriggerRow
	readErr    error
	triggerErr error
	admitted   int
	cancelled  int
}

func (f *rescopeFixture) GetMemberRescopeReceipt(_ context.Context, input db.GetMemberRescopeReceiptParams) (db.MemberRescopeReceipt, error) {
	if f.readErr != nil {
		return db.MemberRescopeReceipt{}, f.readErr
	}
	if input.IssueID != f.receipt.IssueID || input.AgentID != f.receipt.AgentID {
		return db.MemberRescopeReceipt{}, pgx.ErrNoRows
	}
	return f.receipt, nil
}

func (f *rescopeFixture) GetMemberRescopeTrigger(_ context.Context, input db.GetMemberRescopeTriggerParams) (db.GetMemberRescopeTriggerRow, error) {
	if input.CommentID != f.task.TriggerCommentID || input.IssueID != f.task.IssueID || input.OriginatorUserID != f.task.OriginatorUserID {
		return db.GetMemberRescopeTriggerRow{}, pgx.ErrNoRows
	}
	return f.trigger, f.triggerErr
}

func (f *rescopeFixture) AdmitMemberRescopeTask(_ context.Context, input db.AdmitMemberRescopeTaskParams) (db.AgentTaskQueue, error) {
	if input.IssueID != f.receipt.IssueID || input.AgentID != f.receipt.AgentID ||
		input.RefusalTaskID != f.receipt.RefusalTaskID || input.ArmedAt != f.receipt.ArmedAt ||
		input.TaskID != f.task.ID || input.CommentID != f.task.TriggerCommentID || f.receipt.AdmittedTaskID.Valid {
		return db.AgentTaskQueue{}, pgx.ErrNoRows
	}
	f.admitted++
	f.receipt.AdmittedTaskID = input.TaskID
	f.receipt.AdmittedCommentID = input.CommentID
	f.receipt.AdmittedAt = pgtype.Timestamptz{Time: f.trigger.CreatedAt.Time.Add(time.Second), Valid: true}
	outcome := f.task
	outcome.ForceFreshSession = true
	return outcome, nil
}

func (f *rescopeFixture) CancelAgentTaskWithReason(_ context.Context, input db.CancelAgentTaskWithReasonParams) (db.AgentTaskQueue, error) {
	if input.ID != f.task.ID {
		return db.AgentTaskQueue{}, pgx.ErrNoRows
	}
	f.cancelled++
	outcome := f.task
	outcome.Status = "cancelled"
	outcome.Error = input.Error
	outcome.FailureReason = input.FailureReason
	return outcome, nil
}

func newRescopeFixture() *rescopeFixture {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &rescopeFixture{
		task: db.AgentTaskQueue{
			ID: rescopeID(1), IssueID: rescopeID(2), AgentID: rescopeID(3), Status: "dispatched",
			CreatedAt:            pgtype.Timestamptz{Time: now.Add(time.Second), Valid: true},
			TriggerCommentID:     rescopeID(4),
			TriggerEvidenceRefID: rescopeID(4),
			TriggerEvidenceKind:  pgtype.Text{String: "comment", Valid: true},
			OriginatorUserID:     rescopeID(5),
			OriginatorSource:     pgtype.Text{String: "direct_human", Valid: true},
		},
		receipt: db.MemberRescopeReceipt{
			IssueID: rescopeID(2), AgentID: rescopeID(3), RefusalTaskID: rescopeID(6),
			ArmedAt: pgtype.Timestamptz{Time: now, Valid: true},
		},
		trigger: db.GetMemberRescopeTriggerRow{
			CreatedAt: pgtype.Timestamptz{Time: now.Add(time.Second), Valid: true},
			Eligible:  pgtype.Bool{Bool: true, Valid: true},
		},
	}
}

func TestMemberRescopeClaimDecisions(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func(*rescopeFixture)
		status  string
		consume int
	}{
		{"new member directive", func(_ *rescopeFixture) {}, "dispatched", 1},
		{"ordinary mention", func(f *rescopeFixture) { f.trigger.Eligible.Bool = false }, "cancelled", 0},
		{"wakeup quoting directive", func(f *rescopeFixture) { f.task.TriggerEvidenceKind.String = "issue_wakeup" }, "cancelled", 0},
		{"direct assignment", func(f *rescopeFixture) { f.task.TriggerCommentID.Valid = false }, "cancelled", 0},
		{"agent with inherited member originator", func(f *rescopeFixture) { f.task.DelegatedFromTaskID = rescopeID(7) }, "cancelled", 0},
		{"automation with member author label", func(f *rescopeFixture) { f.trigger.Eligible.Bool = false }, "cancelled", 0},
		{"stale member directive", func(f *rescopeFixture) { f.trigger.CreatedAt = f.receipt.ArmedAt }, "cancelled", 0},
		{"manual rerun reusing member directive", func(f *rescopeFixture) {
			f.task.RerunOfTaskID = rescopeID(7)
			f.task.TriggerEvidenceKind.String = "rerun"
			f.task.TriggerEvidenceRefID = f.task.RerunOfTaskID
		}, "cancelled", 0},
		{"queued manual rerun with a new coalesced directive", func(f *rescopeFixture) {
			f.task.RerunOfTaskID = rescopeID(7)
		}, "dispatched", 1},
		{"automated retry copying member evidence", func(f *rescopeFixture) {
			f.task.RetryOfTaskID = rescopeID(7)
		}, "cancelled", 0},
		{"unrelated issue", func(f *rescopeFixture) { f.task.IssueID = rescopeID(7) }, "dispatched", 0},
		{"unrelated agent", func(f *rescopeFixture) { f.task.AgentID = rescopeID(7) }, "dispatched", 0},
		{"unreadable receipt fields", func(f *rescopeFixture) { f.receipt.ArmedAt.Valid = false }, "dispatched", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRescopeFixture()
			test.change(f)
			got, readFailed, err := applyMemberRescope(t.Context(), f, f.task)
			if err != nil || readFailed || got.Status != test.status || f.admitted != test.consume {
				t.Fatalf("status %q, consumed %d, readFailed=%t, err=%v", got.Status, f.admitted, readFailed, err)
			}
			if got.Status == "cancelled" &&
				(got.FailureReason.String != memberRescopeRequired || !strings.Contains(got.Error.String, taskfailure.CodexSafetyRefusalDiagnostic)) {
				t.Fatal("cancellation omitted the member action or original diagnostic")
			}
		})
	}
}

func TestMemberRescopeOneShotAndRearm(t *testing.T) {
	f := newRescopeFixture()
	claim := func(want string) db.AgentTaskQueue {
		t.Helper()
		got, readFailed, err := applyMemberRescope(t.Context(), f, f.task)
		if err != nil || readFailed || got.Status != want {
			t.Fatalf("status %q, want %q, readFailed=%t, err=%v", got.Status, want, readFailed, err)
		}
		return got
	}
	first := claim("dispatched")
	if !first.ForceFreshSession {
		t.Fatal("new scope reused the refused provider session")
	}
	f.task = first
	claim("dispatched") // Recover the same committed claim if delivery was lost.
	f.task.TriggerCommentID, f.task.TriggerEvidenceRefID = rescopeID(9), rescopeID(9)
	claim("cancelled") // Even a different valid marker cannot spend the original claim.
	f.task = first
	f.task.ID = rescopeID(8)
	f.task.CreatedAt.Time = f.receipt.AdmittedAt.Time.Add(time.Second)
	claim("cancelled") // A fresh queue row does not make its old trigger fresh.
	claim("cancelled")
	f.task.TriggerCommentID.Valid = false
	claim("dispatched") // A newly authored ordinary trigger after consumption.
	f.receipt.ArmedAt = f.task.CreatedAt
	f.receipt.RefusalTaskID = f.task.ID
	f.receipt.AdmittedAt = pgtype.Timestamptz{}
	f.receipt.AdmittedTaskID = pgtype.UUID{}
	f.receipt.AdmittedCommentID = pgtype.UUID{}
	claim("cancelled")
	if f.admitted != 1 || f.cancelled != 4 {
		t.Fatalf("consumed=%d, cancelled=%d", f.admitted, f.cancelled)
	}
}

func TestMemberRescopeUnreadableAuthorizationCannotClearKnownHold(t *testing.T) {
	f := newRescopeFixture()
	f.triggerErr = errors.New("synthetic directive read error")
	_, readFailed, err := applyMemberRescope(t.Context(), f, f.task)
	if !errors.Is(err, f.triggerErr) || readFailed || f.admitted != 0 || f.cancelled != 0 {
		t.Fatalf("known hold did not reject unreadable authorization: %v", err)
	}
}

type rescopeSavepointFixture struct {
	pgx.Tx
	readErr    error
	writeErr   error
	queries    int
	writes     int
	commits    int
	rolledBack bool
}

func (f *rescopeSavepointFixture) Begin(context.Context) (pgx.Tx, error) { return f, nil }

type rescopeErrorRow struct{ err error }

func (r rescopeErrorRow) Scan(...any) error { return r.err }

func (f *rescopeSavepointFixture) QueryRow(context.Context, string, ...any) pgx.Row {
	f.queries++
	return rescopeErrorRow{f.readErr}
}

func (f *rescopeSavepointFixture) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	f.writes++
	return pgconn.NewCommandTag("INSERT 0 1"), f.writeErr
}

func (f *rescopeSavepointFixture) Commit(context.Context) error {
	f.commits++
	return nil
}

func (f *rescopeSavepointFixture) Rollback(context.Context) error {
	f.rolledBack = true
	return nil
}

func TestMemberRescopeUnreadableReceiptFailsOpen(t *testing.T) {
	f := &rescopeSavepointFixture{readErr: errors.New("synthetic evidence read error")}
	s := &TaskService{Queries: db.New(f)}
	task := newRescopeFixture().task
	got, err := s.guardMemberRescopeClaim(t.Context(), f, task)
	if err != nil || got.ID != task.ID || got.Status != "dispatched" ||
		f.queries != 1 || f.writes != 0 || !f.rolledBack || f.commits != 0 {
		t.Fatalf("read error did not fail open with rolled-back savepoint: %v", err)
	}
}

func TestMemberRescopeFirstFailureRecording(t *testing.T) {
	f := &rescopeSavepointFixture{}
	q := db.New(f)
	task := newRescopeFixture().task
	task.Status = "failed"
	task.Attempt, task.MaxAttempts = 1, 2
	task.Error = pgtype.Text{String: "API Error: Connection closed mid-response", Valid: true}
	task.FailureReason = pgtype.Text{String: taskfailure.Classify(task.Error.String).String(), Valid: true}
	if !retryEligible(task.FailureReason.String, task) {
		t.Fatal("ordinary transient error lost its retry")
	}
	if err := recordMemberRescopeRefusal(t.Context(), q, task); err != nil || f.writes != 0 {
		t.Fatal("transient error armed a receipt")
	}
	task.Error.String = taskfailure.CodexSafetyRefusalDiagnostic
	if err := recordMemberRescopeRefusal(t.Context(), q, task); err != nil || f.writes != 0 {
		t.Fatal("untyped diagnostic armed a receipt")
	}
	task.FailureReason.String = taskfailure.ReasonCodexSafetyRefusal.String()
	if err := recordMemberRescopeRefusal(t.Context(), q, task); err != nil || f.writes != 1 {
		t.Fatalf("first typed refusal did not arm a receipt: %v", err)
	}
	f.writeErr = errors.New("synthetic receipt write error")
	if err := recordMemberRescopeRefusal(t.Context(), q, task); !errors.Is(err, f.writeErr) {
		t.Fatalf("receipt write failure did not propagate to the failure transaction: %v", err)
	}
	if task.Error.String != taskfailure.CodexSafetyRefusalDiagnostic || retryEligible(task.FailureReason.String, task) {
		t.Fatal("first refusal diagnostic or terminality changed")
	}
}

func TestMemberRescopeRefusedSessionIsNotResumable(t *testing.T) {
	reason := taskfailure.ReasonCodexSafetyRefusal.String()
	if !ResumeUnsafeFailure(reason, taskfailure.CodexSafetyRefusalDiagnostic) {
		t.Fatal("refused session was eligible for a subsequent claim")
	}
}

func TestHasMemberRescopeDirective(t *testing.T) {
	for _, test := range []struct {
		body string
		want bool
	}{
		{"RESCOPE: Write a synthetic note.\nMore detail.", true},
		{"RESCOPE: \nBody on the next line.", false},
		{"> RESCOPE: Quoted text.", false},
		{"```\nRESCOPE: A code example.\n```", false},
		{"Changed instructions without the marker.", false},
	} {
		if HasMemberRescopeDirective(test.body) != test.want {
			t.Errorf("unexpected eligibility for %q", test.body)
		}
	}
}
