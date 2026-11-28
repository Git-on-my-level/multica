package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func mergeReturnFixture(t *testing.T) (principalFixture, *IssueWakeupService, pgtype.UUID, WakeupInput, string) {
	t.Helper()
	f, s, issue, author := conditionFixture(t)
	project := f.Project(t, "Merge return custody")
	merger := f.privateAgentOwnedBy(t, f.UserID, "merger")
	pin, _ := json.Marshal(map[string]any{"flow_review_author": map[string]any{"agent_id": author, "pr": "fixture/repair#7", "at": "2026-01-01T00:00:00Z"}})
	f.Exec(t, "UPDATE issue SET status='in_progress',assignee_type='agent',assignee_id=$2,project_id=$3,metadata=$4 WHERE id=$1", issue, author, project, pin)
	pr := f.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": f.WorkspaceID, "installation_id": 1, "repo_owner": "fixture", "repo_name": "repair", "pr_number": 7,
		"title": "PR", "state": "open", "html_url": "https://example.test/pr/7", "head_sha": "reviewed-head", "branch": "original-branch",
		"pr_created_at": "2026-01-01T00:00:00Z", "pr_updated_at": "2026-01-01T00:00:00Z",
	})
	f.Cleanup(t, "DELETE FROM issue_pull_request WHERE pull_request_id=$1", pr)
	f.Exec(t, "INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, pr)
	i, err := f.q.GetIssue(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.q.GetAgent(context.Background(), parseTestUUID(t, author))
	if err != nil {
		t.Fatal(err)
	}
	c := MergeReturnCustody{Revision: i.Revision, AgentID: author, RuntimeID: util.UUIDToString(a.RuntimeID), ProjectID: project, PR: "fixture/repair#7", Head: "reviewed-head", Branch: "original-branch", ObservedAt: time.Now().UTC(), MergerID: merger}
	raw, _ := json.Marshal(c)
	return f, s, issue, WakeupInput{AgentID: author, Kind: "event", Mode: "once", Instruction: "Assert original implementer and existing branch/worktree before any edit or push",
		Condition: condition(t, map[string]any{"type": "merge_return", "value": json.RawMessage(raw)})}, pr
}

func TestMergeReturnAdmissionAndRetry(t *testing.T) {
	for _, reason := range []string{"red CI", "ordinary merge defect"} {
		t.Run(reason, func(t *testing.T) {
			f, s, issue, in, _ := mergeReturnFixture(t)
			w := wakeCreate(t, f, s, issue, in)
			retry := wakeCreate(t, f, s, issue, in)
			if retry.ID != w.ID {
				t.Fatal("retry created a second repair rule")
			}
			w = wakeTick(t, f, s, w.ID)
			wakeDispatch(t, s, w)
			if got := wakeRuns(t, f, w.ID); got != 1 {
				t.Fatalf("queued repairs = %d", got)
			}
			task, err := f.q.GetAgentTask(context.Background(), w.LastTaskID)
			if err != nil {
				t.Fatal(err)
			}
			if task.AgentID != parseTestUUID(t, in.AgentID) || task.ForceFreshSession || task.Status != "queued" {
				t.Fatalf("admitted wrong custody/session: agent=%v fresh=%v status=%s", task.AgentID, task.ForceFreshSession, task.Status)
			}
			if err := s.CheckClaim(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			f.Comment(t, issueString(issue), "OWNER: newer ruling; stop repair")
			if err := s.CheckClaim(context.Background(), task); !errors.Is(err, errMergeReturnCustody) {
				t.Fatalf("claim accepted newer ruling: %v", err)
			}
			refused, err := f.q.LocklessWakeup(context.Background(), w.ID)
			if err != nil || !refused.LastError.Valid || !refused.DisabledAt.Valid {
				t.Fatalf("claim refusal evidence missing: %+v %v", refused, err)
			}
			i, err := f.q.GetIssue(context.Background(), issue)
			if err != nil || i.Status != "blocked" {
				t.Fatalf("claim refusal did not block expected owner: %+v %v", i, err)
			}
		})
	}
}

func issueString(id pgtype.UUID) string { return util.UUIDToString(id) }

func TestMergeReturnRefusesChangedAdmission(t *testing.T) {
	for _, change := range []string{"owner", "revision", "pin", "head", "branch", "ruling", "unlinked", "runtime", "project", "pr-state", "quoted-ruling", "vcs-link"} {
		t.Run(change, func(t *testing.T) {
			f, s, issue, in, pr := mergeReturnFixture(t)
			w := wakeCreate(t, f, s, issue, in)
			switch change {
			case "owner":
				f.Exec(t, "UPDATE issue SET assignee_id=$2 WHERE id=$1", issue, f.UserID)
			case "revision":
				f.Exec(t, "UPDATE issue SET revision=revision+1 WHERE id=$1", issue)
			case "pin":
				f.Exec(t, "UPDATE issue SET metadata='{}' WHERE id=$1", issue)
			case "head":
				f.Exec(t, "UPDATE github_pull_request SET head_sha='new' WHERE id=$1", pr)
			case "branch":
				f.Exec(t, "UPDATE github_pull_request SET branch='other' WHERE id=$1", pr)
			case "ruling":
				f.Comment(t, issueString(issue), "OWNER: do not resume this repair")
			case "unlinked":
				f.Exec(t, "DELETE FROM issue_pull_request WHERE issue_id=$1", issue)
			case "runtime":
				f.Exec(t, "UPDATE agent SET runtime_id=NULL WHERE id=$1", in.AgentID)
			case "project":
				f.Exec(t, "UPDATE issue SET project_id=$2 WHERE id=$1", issue, f.Project(t, "New project custody"))
			case "pr-state":
				f.Exec(t, "UPDATE github_pull_request SET state='merged' WHERE id=$1", pr)
			case "quoted-ruling":
				id := f.Comment(t, issueString(issue), "`OWNER:` stop this return")
				f.Exec(t, "UPDATE comment SET created_at='2026-02-01T00:00:00Z',updated_at='2026-02-01T00:00:00Z' WHERE id=$1", id)
			case "vcs-link":
				var vcs string
				if err := f.Pool.QueryRow(context.Background(), `INSERT INTO vcs_pull_request(workspace_id,connection_id,provider,repo_owner,repo_name,pr_number,title,state,html_url,pr_created_at,pr_updated_at,head_sha)
				 VALUES($1,gen_random_uuid(),'forgejo','fixture','custody',7,'PR','open','https://vcs.example.test/pr/7',now(),now(),'same-head') RETURNING id`, f.WorkspaceID).Scan(&vcs); err != nil {
					t.Fatal(err)
				}
				f.Cleanup(t, "DELETE FROM vcs_pull_request WHERE id=$1", vcs)
				f.Cleanup(t, "DELETE FROM issue_vcs_pull_request WHERE pull_request_id=$1", vcs)
				f.Exec(t, "INSERT INTO issue_vcs_pull_request(issue_id,pull_request_id) VALUES($1,$2)", issue, vcs)
			}
			if err := s.dispatch(context.Background(), w); err != nil {
				t.Fatal(err)
			}
			if wakeRuns(t, f, w.ID) != 0 {
				t.Fatal("changed custody dispatched a repair")
			}
			got, err := f.q.LocklessWakeup(context.Background(), w.ID)
			if err != nil || got.Enabled || !got.DisabledAt.Valid || !got.LastError.Valid {
				t.Fatalf("missing routing evidence: %+v %v", got, err)
			}
			i, err := f.q.GetIssue(context.Background(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if change == "owner" && i.Status != "in_progress" {
				t.Fatal("overwrote newer owner status")
			}
			if change != "owner" && i.Status != "blocked" {
				t.Fatal("unsafe repair was not blocked")
			}
		})
	}
}

func TestMergeReturnAcceptsRealFinalMergerReceipt(t *testing.T) {
	for _, suffix := range []string{"", "\n", "\r\n"} {
		t.Run("receipt"+suffix, func(t *testing.T) {
			f, s, issue, in, _ := mergeReturnFixture(t)
			w := wakeCreate(t, f, s, issue, in)
			var c WakeupCondition
			_ = json.Unmarshal(in.Condition, &c)
			custody, _ := readMergeReturnCustody(c.Value)
			_, err := f.q.CreateComment(context.Background(), db.CreateCommentParams{IssueID: issue, WorkspaceID: parseTestUUID(t, f.WorkspaceID),
				AuthorType: "agent", AuthorID: parseTestUUID(t, custody.MergerID), Content: "MERGE: CI red synthetic-check" + suffix, Type: "comment"})
			if err != nil {
				t.Fatal(err)
			}
			i, err := f.q.GetIssue(context.Background(), issue)
			if err != nil || i.Revision != custody.Revision+1 {
				t.Fatalf("fixture did not use real revision-bumping comment path: %v %v", i.Revision, err)
			}
			w = wakeTick(t, f, s, w.ID)
			if wakeRuns(t, f, w.ID) != 1 {
				t.Fatal("real final receipt blocked the repair")
			}
			task, err := f.q.GetAgentTask(context.Background(), w.LastTaskID)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.CheckClaim(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			retry := wakeCreate(t, f, s, issue, in)
			if retry.ID != w.ID {
				t.Fatal("retry after final receipt created another rule")
			}
		})
	}
}

func TestMergeReturnRejectsMalformedAndFutureCustody(t *testing.T) {
	for _, future := range []bool{false, true} {
		f, s, issue, in, _ := mergeReturnFixture(t)
		if future {
			var c WakeupCondition
			_ = json.Unmarshal(in.Condition, &c)
			custody, _ := readMergeReturnCustody(c.Value)
			custody.ObservedAt = time.Now().Add(time.Hour)
			in.Condition = condition(t, map[string]any{"type": "merge_return", "value": custody})
		} else {
			in.Condition = condition(t, map[string]any{"type": "merge_return", "value": map[string]any{"revision": 1}})
		}
		if _, err := s.Create(context.Background(), issue, parseTestUUID(t, f.UserID), pgtype.UUID{}, in); !errors.Is(err, ErrWakeupInput) {
			t.Fatalf("invalid custody registered: %v", err)
		}
	}
}

func TestMergeReturnPreparationRefusesRulingOrPRChangeBeforeAssignment(t *testing.T) {
	for _, change := range []string{"ruling", "head"} {
		t.Run(change, func(t *testing.T) {
			f, _, issue, in, pr := mergeReturnFixture(t)
			ctx := context.Background()
			var condition WakeupCondition
			_ = json.Unmarshal(in.Condition, &condition)
			custody, _ := readMergeReturnCustody(condition.Value)
			f.Exec(t, "UPDATE issue SET status='merge',assignee_id=$2 WHERE id=$1", issue, custody.MergerID)
			before, err := f.q.GetIssue(ctx, issue)
			if err != nil {
				t.Fatal(err)
			}
			custody.Revision = before.Revision + 1
			if change == "ruling" {
				f.Comment(t, issueString(issue), "RULING: stop before assignment")
			} else {
				f.Exec(t, "UPDATE github_pull_request SET head_sha='new-custody' WHERE id=$1", pr)
			}
			tx, err := f.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			q := f.q.WithTx(tx)
			locked, err := q.LockWakeupIssue(ctx, issue)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(custody)
			err = ValidateMergeReturnPreparation(ctx, tx, q, locked, db.UpdateIssueParams{ID: issue,
				AssigneeType: pgtype.Text{String: "agent", Valid: true}, AssigneeID: parseTestUUID(t, in.AgentID),
				Status: pgtype.Text{String: "in_progress", Valid: true}, ProjectID: before.ProjectID,
				ExpectedRevision: pgtype.Int8{Int64: before.Revision, Valid: true}}, raw)
			if !errors.Is(err, errMergeReturnCustody) {
				t.Fatalf("changed %s accepted: %v", change, err)
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			after, err := f.q.GetIssue(ctx, issue)
			if err != nil || after.Status != "merge" || after.AssigneeID != before.AssigneeID {
				t.Fatalf("source ownership changed: %+v %v", after, err)
			}
		})
	}
}

func TestMergeReturnAmbiguousIdentityDoesNotRegister(t *testing.T) {
	f, s, issue, in, _ := mergeReturnFixture(t)
	f.Exec(t, "UPDATE issue SET metadata='{}' WHERE id=$1", issue)
	_, err := s.Create(context.Background(), issue, parseTestUUID(t, f.UserID), pgtype.UUID{}, in)
	if !errors.Is(err, ErrWakeupInput) {
		t.Fatalf("ambiguous author registered: %v", err)
	}
	if got := f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1", issue); got != 0 {
		t.Fatalf("ambiguous author started %d runs", got)
	}
}

func TestMergeReturnDoesNotJoinUnguardedCarrier(t *testing.T) {
	for _, changed := range []bool{false, true} {
		f, s, issue, in, pr := mergeReturnFixture(t)
		w := wakeCreate(t, f, s, issue, in)
		carrier := wakeWaitingRun(t, f, issue, in.AgentID, f.UserID)
		w = wakeTick(t, f, s, w.ID)
		if w.LastTaskID.Valid {
			t.Fatal("fixture did not retain input behind existing queued run")
		}
		if changed {
			f.Exec(t, "UPDATE github_pull_request SET head_sha='changed-before-carrier-claim' WHERE id=$1", pr)
		}
		if notes := wakeClaim(t, f, s, carrier); notes != "" {
			t.Fatalf("guarded repair leaked into generic carrier: %s", notes)
		}
		if changed {
			wakeDispatch(t, s, w)
			if wakeRuns(t, f, w.ID) != 0 {
				t.Fatal("changed custody started a repair")
			}
		} else {
			f.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1", carrier)
			w = wakeTick(t, f, s, w.ID)
			if !w.LastTaskID.Valid || wakeRuns(t, f, w.ID) != 1 {
				t.Fatal("guarded input did not receive its own dispatch")
			}
		}
	}
}

func TestMergeReturnPreObservationRulingIsNotOrdinaryHistory(t *testing.T) {
	for _, ruling := range []bool{false, true} {
		f, s, issue, in, _ := mergeReturnFixture(t)
		text := "ordinary prior review evidence"
		if ruling {
			text = "RULING: owner routing supersedes the old author pin"
		}
		if _, err := f.q.CreateComment(context.Background(), db.CreateCommentParams{IssueID: issue, WorkspaceID: parseTestUUID(t, f.WorkspaceID),
			AuthorType: "member", AuthorID: parseTestUUID(t, f.UserID), Content: text, Type: "comment"}); err != nil {
			t.Fatal(err)
		}
		i, err := f.q.GetIssue(context.Background(), issue)
		if err != nil {
			t.Fatal(err)
		}
		var c WakeupCondition
		_ = json.Unmarshal(in.Condition, &c)
		custody, _ := readMergeReturnCustody(c.Value)
		custody.Revision, custody.ObservedAt = i.Revision, time.Now().UTC()
		in.Condition = condition(t, map[string]any{"type": "merge_return", "value": custody})
		_, err = s.Create(context.Background(), issue, parseTestUUID(t, f.UserID), pgtype.UUID{}, in)
		if ruling && (err == nil || !strings.Contains(err.Error(), "newer comment")) {
			t.Fatalf("pre-observation ruling was ignored: %v", err)
		}
		if !ruling && err != nil {
			t.Fatalf("ordinary pre-observation history blocked repair: %v", err)
		}
	}
}
