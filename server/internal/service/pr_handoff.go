package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/githubpr"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// recordTaskPRHandoff persists only canonical GitHub PR URLs reported in the
// completing task's final output. It runs in the completion transaction: a
// storage failure rolls completion back, and callback replay remains
// idempotent through the task_id+url unique index.
func (s *TaskService) recordTaskPRHandoff(ctx context.Context, q *db.Queries, task db.AgentTaskQueue, result []byte) error {
	if !task.IssueID.Valid {
		return nil
	}
	var payload protocol.TaskCompletedPayload
	if err := json.Unmarshal(result, &payload); err != nil {
		return nil
	}
	refs := githubpr.ExtractCanonical(payload.Output)
	if ref, ok := githubpr.ParseCanonical(payload.PRURL); ok {
		refs = appendDistinctPRReference(refs, ref)
	}
	if len(refs) == 0 {
		return nil
	}
	// Lock the workspace and issue keys through candidate persistence. The
	// candidate table intentionally has no cross-tenant FKs, so this lock pairs
	// with the issue/workspace delete sweeps and prevents a completion racing
	// just behind a sweep from leaving an orphan row.
	issue, err := q.GetIssueForPRHandoff(ctx, task.IssueID)
	if err != nil {
		return fmt.Errorf("load issue for PR handoff: %w", err)
	}
	allowed, err := registeredGitHubRepositories(ctx, q, issue)
	if err != nil {
		return err
	}
	candidates := classifyTaskPRHandoff(payload.Output, payload.PRURL, allowed)
	for _, candidate := range candidates {
		ref := candidate.Reference
		owner := strings.ToLower(ref.Owner)
		repo := strings.ToLower(ref.Repo)
		if _, err := q.UpsertIssuePRHandoffCandidate(ctx, db.UpsertIssuePRHandoffCandidateParams{
			WorkspaceID: issue.WorkspaceID,
			IssueID:     issue.ID,
			TaskID:      task.ID,
			Url:         ref.URL,
			RepoOwner:   owner,
			RepoName:    repo,
			PrNumber:    ref.Number,
			State:       candidate.State,
		}); err != nil {
			return fmt.Errorf("record PR handoff candidate: %w", err)
		}
	}
	return nil
}

type taskPRHandoffCandidate struct {
	Reference githubpr.Reference
	State     string
}

func classifyTaskPRHandoff(output, prURL string, allowed map[string]struct{}) []taskPRHandoffCandidate {
	refs := githubpr.ExtractCanonical(output)
	if ref, ok := githubpr.ParseCanonical(prURL); ok {
		refs = appendDistinctPRReference(refs, ref)
	}
	ambiguous := len(refs) != 1
	candidates := make([]taskPRHandoffCandidate, 0, len(refs))
	for _, ref := range refs {
		key := strings.ToLower(ref.Owner + "/" + ref.Repo)
		state := "candidate_detected"
		if _, ok := allowed[key]; !ok {
			state = "invalid_external_pr"
		} else if !ambiguous {
			state = "awaiting_mirror"
		}
		candidates = append(candidates, taskPRHandoffCandidate{Reference: ref, State: state})
	}
	return candidates
}

func appendDistinctPRReference(refs []githubpr.Reference, candidate githubpr.Reference) []githubpr.Reference {
	for _, ref := range refs {
		if strings.EqualFold(ref.URL, candidate.URL) {
			return refs
		}
	}
	return append(refs, candidate)
}

// registeredGitHubRepositories follows the daemon checkout contract: project
// github_repo resources override workspace repositories when at least one is
// attached; otherwise workspace repos are the verification scope.
func registeredGitHubRepositories(ctx context.Context, q *db.Queries, issue db.Issue) (map[string]struct{}, error) {
	repos := map[string]struct{}{}
	projectScoped := false
	if issue.ProjectID.Valid {
		resources, err := q.ListProjectResources(ctx, issue.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("list project repositories for PR handoff: %w", err)
		}
		for _, resource := range resources {
			if resource.ResourceType != "github_repo" {
				continue
			}
			projectScoped = true
			var ref struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(resource.ResourceRef, &ref) == nil {
				if key, ok := githubpr.RepositoryFromRemote(ref.URL); ok {
					repos[key] = struct{}{}
				}
			}
		}
	}
	if projectScoped {
		return repos, nil
	}
	workspace, err := q.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("load workspace repositories for PR handoff: %w", err)
	}
	var workspaceRepos []struct {
		URL string `json:"url"`
	}
	if len(workspace.Repos) > 0 {
		if err := json.Unmarshal(workspace.Repos, &workspaceRepos); err != nil {
			return nil, fmt.Errorf("decode workspace repositories for PR handoff: %w", err)
		}
	}
	for _, ref := range workspaceRepos {
		if key, ok := githubpr.RepositoryFromRemote(ref.URL); ok {
			repos[key] = struct{}{}
		}
	}
	return repos, nil
}

// reconcileTaskPRHandoff closes the completion/webhook crossing race after the
// completion transaction commits. The webhook runs the same address-based
// reconciliation after mirroring; whichever side commits second observes the
// first. Replays are harmless because both link and candidate updates are
// idempotent.
func (s *TaskService) reconcileTaskPRHandoff(ctx context.Context, task db.AgentTaskQueue) error {
	if !task.IssueID.Valid {
		return nil
	}
	candidates, err := s.Queries.ListLatestIssuePRHandoffCandidates(ctx, task.IssueID)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if candidate.TaskID != task.ID || candidate.State != "awaiting_mirror" {
			continue
		}
		pr, err := s.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
			WorkspaceID: candidate.WorkspaceID,
			RepoOwner:   candidate.RepoOwner,
			RepoName:    candidate.RepoName,
			PrNumber:    candidate.PrNumber,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if !installationConnected(ctx, s.Queries, candidate.WorkspaceID, pr.InstallationID) {
			continue
		}
		if err := linkHandoffCandidate(ctx, s.Queries, candidate, pr); err != nil {
			return err
		}
	}
	return nil
}

func installationConnected(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, installationID int64) bool {
	installations, err := q.ListGitHubInstallationsByWorkspace(ctx, workspaceID)
	if err != nil {
		return false
	}
	for _, installation := range installations {
		if installation.InstallationID == installationID {
			return true
		}
	}
	return false
}

func linkHandoffCandidate(ctx context.Context, q *db.Queries, candidate db.IssuePrHandoffCandidate, pr db.GithubPullRequest) error {
	if err := q.LinkIssueToPullRequest(ctx, db.LinkIssueToPullRequestParams{
		IssueID:             candidate.IssueID,
		PullRequestID:       pr.ID,
		CloseIntent:         false,
		ReferenceOnly:       false,
		PreserveCloseIntent: true,
		LinkedByType:        pgtype.Text{String: "system", Valid: true},
		LinkedByID:          pgtype.UUID{},
	}); err != nil {
		return err
	}
	return q.MarkIssuePRHandoffCandidateLinked(ctx, candidate.ID)
}
