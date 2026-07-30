package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/githubpr"
	"github.com/multica-ai/multica/server/internal/issueguard"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type safeActionSignal struct {
	Allowed    bool   `json:"allowed"`
	ReasonCode string `json:"reason_code"`
}

// InspectIssueCoordination returns one permission-checked, point-in-time
// ownership/handoff snapshot so agents do not race multiple independently
// fetched issue, run, and PR views before deciding whether to rerun or hand off.
func (h *Handler) InspectIssueCoordination(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	prefix := h.getIssuePrefix(r.Context(), issue.WorkspaceID)

	var project any
	if issue.ProjectID.Valid {
		row, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{
			ID: issue.ProjectID, WorkspaceID: issue.WorkspaceID,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to inspect issue project")
			return
		}
		project = projectToResponse(row)
	}
	assignee := h.coordinationAssignee(r, issue)

	activeRows, err := h.Queries.ListActiveTasksByIssue(r.Context(), issue.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to inspect active issue runs")
		return
	}
	activeRuns := make([]map[string]any, 0, len(activeRows))
	for _, row := range activeRows {
		activeRuns = append(activeRuns, coordinationTaskResponse(row))
	}
	allRuns, err := h.Queries.ListTasksByIssue(r.Context(), issue.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to inspect issue runs")
		return
	}
	var latestRun any
	if len(allRuns) > 0 {
		latestRun = coordinationTaskResponse(allRuns[0])
	}

	prRows, err := h.Queries.ListPullRequestsByIssue(r.Context(), issue.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to inspect issue pull requests")
		return
	}
	prs := make([]GitHubPullRequestResponse, 0, len(prRows))
	for _, row := range prRows {
		prs = append(prs, issuePullRequestRowToResponse(row, h.PRRefresh.Enabled()))
	}
	handoff, err := h.loadIssuePRHandoff(r.Context(), issue.ID, len(prs) > 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to inspect pull request handoff")
		return
	}

	noActive := len(activeRuns) == 0
	hasAssignee := issue.AssigneeType.Valid && issue.AssigneeID.Valid
	isTerminal := issue.Status == "done" || issue.Status == "cancelled"
	safeToRerun := noActive && hasAssignee && !isTerminal
	safeToReassign := noActive && !isTerminal
	reassignReason := "active_run"
	if isTerminal {
		reassignReason = "terminal_issue"
	}
	safeActions := map[string]safeActionSignal{
		"rerun": {
			Allowed:    safeToRerun,
			ReasonCode: coordinationReason(safeToRerun, "safe", "active_or_unroutable"),
		},
		"reassign": {
			Allowed:    safeToReassign,
			ReasonCode: coordinationReason(safeToReassign, "safe", reassignReason),
		},
		"create_duplicate": {
			Allowed:    false,
			ReasonCode: "use_existing_issue",
		},
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issue":         issueToResponse(issue, prefix),
		"project":       project,
		"assignee":      assignee,
		"active_runs":   activeRuns,
		"latest_run":    latestRun,
		"pull_requests": prs,
		"pr_handoff":    handoff,
		"safe_actions":  safeActions,
	})
}

func coordinationTaskResponse(task db.AgentTaskQueue) map[string]any {
	return map[string]any{
		"id":             uuidToString(task.ID),
		"agent_id":       uuidToString(task.AgentID),
		"runtime_id":     uuidToString(task.RuntimeID),
		"status":         task.Status,
		"priority":       task.Priority,
		"attempt":        task.Attempt,
		"max_attempts":   task.MaxAttempts,
		"parent_task_id": uuidToPtr(task.ParentTaskID),
		"is_leader_task": task.IsLeaderTask,
		"dispatched_at":  timestampToPtr(task.DispatchedAt),
		"started_at":     timestampToPtr(task.StartedAt),
		"completed_at":   timestampToPtr(task.CompletedAt),
		"created_at":     timestampToString(task.CreatedAt),
	}
}

func coordinationReason(allowed bool, yes, no string) string {
	if allowed {
		return yes
	}
	return no
}

func (h *Handler) coordinationAssignee(r *http.Request, issue db.Issue) any {
	if !issue.AssigneeType.Valid || !issue.AssigneeID.Valid {
		return nil
	}
	id := uuidToString(issue.AssigneeID)
	switch issue.AssigneeType.String {
	case "agent":
		if agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
			ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID,
		}); err == nil {
			ready, reason, _ := service.AgentReadiness(r.Context(), h.Queries, agent)
			return map[string]any{"type": "agent", "id": id, "name": agent.Name, "ready": ready, "reason": reason}
		}
	case "member":
		if member, err := h.Queries.GetMember(r.Context(), issue.AssigneeID); err == nil {
			if user, userErr := h.Queries.GetUser(r.Context(), member.UserID); userErr == nil {
				return map[string]any{"type": "member", "id": id, "name": user.Name}
			}
		}
	case "squad":
		if squad, err := h.Queries.GetSquadInWorkspace(r.Context(), db.GetSquadInWorkspaceParams{
			ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID,
		}); err == nil {
			return map[string]any{"type": "squad", "id": id, "name": squad.Name, "leader_id": uuidToString(squad.LeaderID)}
		}
	}
	return map[string]any{"type": issue.AssigneeType.String, "id": id}
}

type issueRouteRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Repository  string `json:"repository"`
	ProjectID   string `json:"project_id"`
}

// RouteIssueCoordination is advisory and read-only. The create path's locked
// duplicate guard remains authoritative; safe_to_create is a live snapshot,
// never a reservation.
func (h *Handler) RouteIssueCoordination(w http.ResponseWriter, r *http.Request) {
	var req issueRouteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	workspaceID := h.resolveWorkspaceID(r)
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	wsID := member.WorkspaceID

	projects, err := h.Queries.ListProjects(r.Context(), db.ListProjectsParams{WorkspaceID: wsID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to inspect projects")
		return
	}
	projectIDs := make([]pgtype.UUID, 0, len(projects))
	for _, project := range projects {
		projectIDs = append(projectIDs, project.ID)
	}
	resources, err := h.Queries.ListProjectResourcesForProjects(r.Context(), projectIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to inspect project resources")
		return
	}

	var repoKey string
	if req.Repository != "" {
		var valid bool
		repoKey, valid = githubpr.RepositoryFromRemote(req.Repository)
		if !valid {
			writeError(w, http.StatusBadRequest, "repository must be a registered GitHub clone URL")
			return
		}
	}
	repositoryMatches := make([]map[string]any, 0)
	projectMatchIDs := map[string]struct{}{}
	projectRepositoryCounts := map[string]int{}
	for _, resource := range resources {
		if resource.ResourceType != "github_repo" {
			continue
		}
		projectID := uuidToString(resource.ProjectID)
		projectRepositoryCounts[projectID]++
		var ref struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(resource.ResourceRef, &ref) != nil {
			continue
		}
		key, valid := githubpr.RepositoryFromRemote(ref.URL)
		if !valid || repoKey == "" || key != repoKey {
			continue
		}
		projectMatchIDs[projectID] = struct{}{}
		repositoryMatches = append(repositoryMatches, map[string]any{
			"scope": "project", "project_id": projectID, "resource_id": uuidToString(resource.ID), "repository": key,
		})
	}
	if repoKey != "" {
		if workspace, wsErr := h.Queries.GetWorkspace(r.Context(), wsID); wsErr == nil {
			var refs []struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(workspace.Repos, &refs)
			for _, ref := range refs {
				if key, valid := githubpr.RepositoryFromRemote(ref.URL); valid && key == repoKey {
					repositoryMatches = append(repositoryMatches, map[string]any{"scope": "workspace", "repository": key})
				}
			}
		}
	}

	var selectedProject *ProjectResponse
	if req.ProjectID != "" {
		projectID, valid := parseUUIDOrBadRequest(w, req.ProjectID, "project_id")
		if !valid {
			return
		}
		for _, project := range projects {
			if project.ID == projectID {
				resp := projectToResponse(project)
				selectedProject = &resp
				break
			}
		}
		if selectedProject == nil {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
	} else if len(projectMatchIDs) == 1 {
		for _, project := range projects {
			if _, found := projectMatchIDs[uuidToString(project.ID)]; found {
				resp := projectToResponse(project)
				selectedProject = &resp
				break
			}
		}
	} else if len(projects) == 1 {
		resp := projectToResponse(projects[0])
		selectedProject = &resp
	}

	duplicates, err := h.Queries.FindActiveDuplicateIssuesByTitle(r.Context(), db.FindActiveDuplicateIssuesByTitleParams{
		WorkspaceID: wsID,
		Title:       issueguard.NormalizeTitle(req.Title),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to inspect duplicate issues")
		return
	}
	matchingIssues := make([]IssueResponse, 0, len(duplicates))
	blockedByOwner := false
	prefix := h.getIssuePrefix(r.Context(), wsID)
	for _, duplicate := range duplicates {
		matchingIssues = append(matchingIssues, issueToResponse(duplicate, prefix))
		if selectedProject != nil && (!duplicate.ProjectID.Valid || uuidToString(duplicate.ProjectID) != selectedProject.ID) {
			continue
		}
		active, activeErr := h.Queries.ListActiveTasksByIssue(r.Context(), duplicate.ID)
		if activeErr != nil {
			writeError(w, http.StatusInternalServerError, "failed to inspect duplicate ownership")
			return
		}
		if len(active) > 0 {
			blockedByOwner = true
		}
	}

	matchingPRs := []GitHubPullRequestResponse{}
	if repoKey != "" {
		parts := strings.SplitN(repoKey, "/", 2)
		rows, listErr := h.Queries.ListOpenPullRequestsByRepository(r.Context(), db.ListOpenPullRequestsByRepositoryParams{
			WorkspaceID: wsID, RepoOwner: parts[0], RepoName: parts[1],
		})
		if listErr != nil {
			writeError(w, http.StatusInternalServerError, "failed to inspect matching pull requests")
			return
		}
		for _, row := range rows {
			matchingPRs = append(matchingPRs, githubPullRequestToResponse(row, h.PRRefresh.Enabled()))
		}
	}

	agents, err := h.Queries.ListAgents(r.Context(), wsID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to inspect routing agents")
		return
	}
	taskSnapshot, err := h.Queries.ListWorkspaceAgentTaskSnapshot(r.Context(), wsID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to inspect agent availability")
		return
	}
	activeCounts := map[string]int{}
	for _, task := range taskSnapshot {
		if task.Status == "queued" || task.Status == "dispatched" || task.Status == "running" || task.Status == "waiting_local_directory" {
			activeCounts[uuidToString(task.AgentID)]++
		}
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), workspaceID)
	allowedAgents, accessOK := h.accessibleAgentIDs(r.Context(), workspaceID, actorType, actorID, member.Role)
	if !accessOK {
		writeError(w, http.StatusInternalServerError, "failed to inspect agent access")
		return
	}
	candidateAgents := make([]map[string]any, 0, len(agents))
	for _, agent := range agents {
		id := uuidToString(agent.ID)
		if _, allowed := allowedAgents[id]; !allowed {
			continue
		}
		ready, reason, _ := service.AgentReadiness(r.Context(), h.Queries, agent)
		capacity := int(agent.MaxConcurrentTasks)
		if capacity <= 0 {
			capacity = 1
		}
		available := ready && activeCounts[id] < capacity
		reasonCode := "available"
		if !ready {
			reasonCode = "runtime_unavailable"
		} else if !available {
			reasonCode = "at_capacity"
		}
		candidateAgents = append(candidateAgents, map[string]any{
			"id": id, "name": agent.Name, "available": available, "reason_code": reasonCode, "reason": reason,
			"active_task_count": activeCounts[id], "max_concurrent_tasks": capacity,
		})
	}

	decision := "safe_to_create"
	reasons := []string{}
	if len(matchingIssues) > 0 {
		if blockedByOwner {
			decision = "blocked_by_active_owner"
			reasons = append(reasons, "an exact-title issue has an active run")
		} else {
			decision = "use_existing_issue"
			reasons = append(reasons, "an active exact-title issue already exists")
		}
	} else if selectedProject != nil && repoKey != "" &&
		projectRepositoryCounts[selectedProject.ID] > 0 {
		if _, matchesSelected := projectMatchIDs[selectedProject.ID]; !matchesSelected {
			decision = "needs_project_selection"
			reasons = append(reasons, "repository is not attached to the selected project")
		}
	} else if (req.ProjectID == "" && len(projectMatchIDs) > 1) ||
		(repoKey != "" && len(repositoryMatches) == 0) ||
		(selectedProject == nil && len(projects) > 1) {
		decision = "needs_project_selection"
		if selectedProject == nil && len(projects) > 1 && len(projectMatchIDs) == 0 {
			reasons = append(reasons, "multiple projects are available")
		} else if len(projectMatchIDs) > 1 {
			reasons = append(reasons, "repository is attached to multiple projects")
		} else {
			reasons = append(reasons, "repository is not registered in the workspace")
		}
	}

	projectCandidates := make([]ProjectResponse, 0, len(projects))
	for _, project := range projects {
		if repoKey == "" {
			projectCandidates = append(projectCandidates, projectToResponse(project))
			continue
		}
		if _, found := projectMatchIDs[uuidToString(project.ID)]; found {
			projectCandidates = append(projectCandidates, projectToResponse(project))
		}
	}
	var existingIssue any
	if len(matchingIssues) > 0 {
		existingIssue = matchingIssues[0]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"decision":               decision,
		"selected_project":       selectedProject,
		"project_candidates":     projectCandidates,
		"existing_issue":         existingIssue,
		"matching_issues":        matchingIssues,
		"matching_pull_requests": matchingPRs,
		"repository_matches":     repositoryMatches,
		"candidate_agents":       candidateAgents,
		"reasons":                reasons,
	})
}
