package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// MergeReturnCustody is opt-in and belongs only to the merge repair path.
// The revision is captured AFTER a no-start assignment/status write is read back.
type MergeReturnCustody struct {
	Revision   int64     `json:"revision"`
	AgentID    string    `json:"agent_id"`
	RuntimeID  string    `json:"runtime_id"`
	ProjectID  string    `json:"project_id"`
	PR         string    `json:"pr"`
	Head       string    `json:"head"`
	Branch     string    `json:"branch"`
	ObservedAt time.Time `json:"observed_at"`
	MergerID   string    `json:"merger_id"`
}

var errMergeReturnCustody = errors.New("merge-return custody changed; owner routing required")

func readMergeReturnCustody(raw []byte) (MergeReturnCustody, error) {
	var c MergeReturnCustody
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	for _, id := range []string{c.AgentID, c.RuntimeID, c.ProjectID, c.MergerID} {
		if _, err := util.ParseUUID(id); err != nil {
			return c, fmt.Errorf("merge-return custody requires UUID identities")
		}
	}
	if c.Revision < 1 || c.PR == "" || c.Head == "" || c.Branch == "" || c.ObservedAt.IsZero() || c.AgentID == c.MergerID {
		return c, fmt.Errorf("merge-return custody requires revision, original agent, PR/head/branch and observation time")
	}
	if c.ObservedAt.After(time.Now()) {
		return c, fmt.Errorf("merge-return observation time is in the future")
	}
	return c, nil
}

func checkMergeReturnCustody(ctx context.Context, tx pgx.Tx, q *db.Queries, w db.IssueWakeup, raw []byte) error {
	c, err := readMergeReturnCustody(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", errMergeReturnCustody, err)
	}
	i, err := q.GetIssue(ctx, w.IssueID)
	if err != nil {
		return err
	}
	return checkMergeReturnIssue(ctx, tx, q, w, c, i)
}

// ValidateMergeReturnPreparation runs under the same issue lock as the
// no-start ownership write, fencing a ruling arriving after client preflight.
func ValidateMergeReturnPreparation(ctx context.Context, tx pgx.Tx, q *db.Queries, i db.Issue, params db.UpdateIssueParams, raw []byte) error {
	c, err := readMergeReturnCustody(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", errMergeReturnCustody, err)
	}
	if i.Status != "merge" || i.AssigneeType.String != "agent" || util.UUIDToString(i.AssigneeID) != c.MergerID ||
		i.Revision != c.Revision-1 || !params.ExpectedRevision.Valid || params.ExpectedRevision.Int64 != i.Revision ||
		params.Status.String != "in_progress" || params.AssigneeType.String != "agent" || util.UUIDToString(params.AssigneeID) != c.AgentID || params.ProjectID != i.ProjectID {
		return errMergeReturnCustody
	}
	i.Status, i.AssigneeType, i.AssigneeID, i.Revision = "in_progress", params.AssigneeType, params.AssigneeID, c.Revision
	return checkMergeReturnIssue(ctx, tx, q, db.IssueWakeup{IssueID: i.ID, AgentID: params.AssigneeID}, c, i)
}

func checkMergeReturnIssue(ctx context.Context, tx pgx.Tx, q *db.Queries, w db.IssueWakeup, c MergeReturnCustody, i db.Issue) error {
	if i.Status != "in_progress" || i.TriageState.Valid || i.AssigneeType.String != "agent" ||
		util.UUIDToString(i.AssigneeID) != c.AgentID || util.UUIDToString(w.AgentID) != c.AgentID || util.UUIDToString(i.ProjectID) != c.ProjectID {
		return errMergeReturnCustody
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: w.AgentID, WorkspaceID: i.WorkspaceID})
	if err != nil {
		return err
	}
	if util.UUIDToString(agent.RuntimeID) != c.RuntimeID {
		return fmt.Errorf("%w: original runtime changed", errMergeReturnCustody)
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(i.Metadata, &metadata) != nil {
		return errMergeReturnCustody
	}
	pin := metadata["flow_review_author"]
	var encoded string
	if json.Unmarshal(pin, &encoded) == nil {
		pin = []byte(encoded)
	}
	var author struct {
		AgentID string    `json:"agent_id"`
		PR      string    `json:"pr"`
		At      time.Time `json:"at"`
	}
	if json.Unmarshal(pin, &author) != nil || author.AgentID != c.AgentID || author.PR != c.PR || author.At.IsZero() || author.At.After(c.ObservedAt) {
		return fmt.Errorf("%w: original-author pin missing or superseded", errMergeReturnCustody)
	}
	// Fail closed on a new ruling/hold/comment. Only the merger's ordinary
	// final receipt may follow registration without cancelling admission.
	var changed bool
	var receipts int64
	// A final merger receipt is a real comment and advances issue.revision.
	// Account for only that immutable receipt after registration. Every other
	// revision increment (including editing/deleting the receipt) still refuses.
	err = tx.QueryRow(ctx, `WITH observations AS (
		SELECT *, ($8::bool AND author_type='agent' AND author_id::text=$3 AND created_at >= $6
		 AND revision=1 AND deleted_at IS NULL AND btrim(content,E' \t\r\n') ~ $7) AS receipt
		FROM comment WHERE issue_id=$1)
		SELECT count(*) FILTER(WHERE receipt), COALESCE(bool_or(
		 (created_at>$2 OR updated_at>$2 OR ((created_at>$4 OR updated_at>$4) AND content ~* $5)) AND NOT receipt),false)
		FROM observations`, i.ID, c.ObservedAt, c.MergerID, author.At,
		"(?m)^[ \t>*`_]*(RULING|OWNER):", w.CreatedAt.Time, "^MERGE: (CI red|repair returned) [^\r\n]+$", w.CreatedAt.Valid).Scan(&receipts, &changed)
	if err != nil {
		return err
	}
	if changed || receipts > 1 || i.Revision != c.Revision+receipts {
		return fmt.Errorf("%w: newer comment; re-read owner evidence", errMergeReturnCustody)
	}
	prs, err := q.ListPullRequestsByIssue(ctx, i.ID)
	if err != nil {
		return err
	}
	vcs, err := q.ListVCSPullRequestsByIssue(ctx, i.ID)
	if err != nil {
		return err
	}
	if len(prs) != 1 || len(vcs) != 0 {
		return fmt.Errorf("%w: PR custody is ambiguous", errMergeReturnCustody)
	}
	p := prs[0]
	if fmt.Sprintf("%s/%s#%d", p.RepoOwner, p.RepoName, p.PrNumber) != c.PR || p.HeadSha != c.Head || p.Branch.String != c.Branch || p.State != "open" {
		return fmt.Errorf("%w: PR/head/branch changed", errMergeReturnCustody)
	}
	return nil
}

// Refuse under dispatch's issue lock. Preserve a newer owner's assignment and
// status; a repair still owned by the pinned implementer is parked visibly.
func (s *IssueWakeupService) refuseMergeReturn(ctx context.Context, tx pgx.Tx, q *db.Queries, issue db.Issue, w db.IssueWakeup, raw []byte, reason error) error {
	c, _ := readMergeReturnCustody(raw)
	prev := issue.Status
	changed := issue.Status == "in_progress" && issue.AssigneeType.String == "agent" && util.UUIDToString(issue.AssigneeID) == c.AgentID
	var err error
	if changed {
		issue, err = q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, Status: "blocked"})
		if err != nil {
			return err
		}
	}
	if err = q.PauseIssueWakeup(ctx, db.PauseIssueWakeupParams{ID: w.ID, PausedReason: pgtype.Text{}, BlockRuns: true}); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE issue_wakeup SET last_error=$2 WHERE id=$1", w.ID, reason.Error()); err != nil {
		return err
	}
	if err = q.DiscardWakeupReceipts(ctx, w.ID); err != nil {
		return err
	}
	cancelled, err := q.CancelUnstartedWakeupTasks(ctx, util.UUIDToString(w.ID))
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if changed && s.Tasks.Bus != nil {
		s.Tasks.broadcastIssueUpdated(ctx, issue, prev)
	}
	for _, task := range cancelled {
		s.Tasks.broadcastTaskEvent(ctx, protocol.EventTaskCancelled, task)
	}
	return nil
}
