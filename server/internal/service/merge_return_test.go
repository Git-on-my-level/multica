package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
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
	c := MergeReturnCustody{Revision: i.Revision, AgentID: author, ProjectID: project, PR: "fixture/repair#7", Head: "reviewed-head", Branch: "original-branch", ObservedAt: time.Now().UTC(), MergerID: merger}
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
		})
	}
}

func issueString(id pgtype.UUID) string { return util.UUIDToString(id) }

func TestMergeReturnRefusesChangedAdmission(t *testing.T) {
	for _, change := range []string{"owner", "revision", "pin", "head", "branch", "ruling", "unlinked"} {
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
