package handler

import (
	"context"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type PRHandoffCandidateResponse struct {
	URL       string `json:"url"`
	State     string `json:"state"`
	TaskID    string `json:"task_id"`
	RepoOwner string `json:"repo_owner"`
	RepoName  string `json:"repo_name"`
	Number    int32  `json:"number"`
}

type PRHandoffResponse struct {
	State      string                       `json:"state"`
	Candidates []PRHandoffCandidateResponse `json:"candidates"`
}

func (h *Handler) loadIssuePRHandoff(ctx context.Context, issueID pgtype.UUID, hasNativeLink bool) (PRHandoffResponse, error) {
	handoff := PRHandoffResponse{State: "missing", Candidates: []PRHandoffCandidateResponse{}}
	candidates, err := h.Queries.ListLatestIssuePRHandoffCandidates(ctx, issueID)
	if err != nil {
		return handoff, err
	}
	for _, candidate := range candidates {
		handoff.Candidates = append(handoff.Candidates, PRHandoffCandidateResponse{
			URL:       candidate.Url,
			State:     candidate.State,
			TaskID:    uuidToString(candidate.TaskID),
			RepoOwner: candidate.RepoOwner,
			RepoName:  candidate.RepoName,
			Number:    candidate.PrNumber,
		})
	}
	switch {
	case len(handoff.Candidates) > 1:
		handoff.State = "multiple_candidates_needs_review"
	case len(handoff.Candidates) == 1:
		handoff.State = handoff.Candidates[0].State
	case hasNativeLink:
		handoff.State = "linked"
	}
	return handoff, nil
}

func (h *Handler) linkAwaitingHandoffsForPR(ctx context.Context, workspaceID pgtype.UUID, pr db.GithubPullRequest) []string {
	candidates, err := h.Queries.ListAwaitingPRHandoffCandidates(ctx, db.ListAwaitingPRHandoffCandidatesParams{
		WorkspaceID: workspaceID,
		RepoOwner:   strings.ToLower(pr.RepoOwner),
		RepoName:    strings.ToLower(pr.RepoName),
		PrNumber:    pr.PrNumber,
	})
	if err != nil {
		slog.Warn("github: list awaiting PR handoffs failed", "error", err)
		return nil
	}
	linked := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		issue, err := h.Queries.GetIssue(ctx, candidate.IssueID)
		if err != nil || issue.WorkspaceID != workspaceID {
			continue
		}
		if err := h.Queries.LinkIssueToPullRequest(ctx, db.LinkIssueToPullRequestParams{
			IssueID:             candidate.IssueID,
			PullRequestID:       pr.ID,
			CloseIntent:         false,
			PreserveCloseIntent: true,
			LinkedByType:        strToText("system"),
			LinkedByID:          pgtype.UUID{},
		}); err != nil {
			slog.Warn("github: link awaited PR handoff failed", "error", err)
			continue
		}
		if err := h.Queries.MarkIssuePRHandoffCandidateLinked(ctx, candidate.ID); err != nil {
			slog.Warn("github: mark PR handoff linked failed", "error", err)
			continue
		}
		linked = append(linked, uuidToString(candidate.IssueID))
	}
	return linked
}
