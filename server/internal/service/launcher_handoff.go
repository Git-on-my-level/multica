package service

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// FailUnstartedLauncherClaim uses the normal atomic failure/recovery path,
// with an exact claim-generation fence and a separate no-work witness.
func (s *TaskService) FailUnstartedLauncherClaim(ctx context.Context, id pgtype.UUID, claim protocol.LauncherClaim) (*db.AgentTaskQueue, bool, error) {
	stamp, err := time.Parse(time.RFC3339Nano, claim.DispatchedAt)
	if err != nil {
		return nil, false, err
	}
	task, err := s.Queries.GetAgentTask(ctx, id)
	if err != nil {
		return nil, false, err
	}
	// Reject stale or observed work before overlay/network preparation. The
	// terminal SQL repeats these fences atomically with the transition.
	if task.Status != "dispatched" || task.StartedAt.Valid || !task.DispatchedAt.Valid || !task.DispatchedAt.Time.Equal(stamp) || task.SessionID.String != "" || task.WorkDir.String != "" || task.BranchName.String != "" || task.DurableWorkDir.String != "" || (len(task.Result) != 0 && string(task.Result) != "{}" && string(task.Result) != "null") {
		return &task, false, nil
	}
	rt, err := s.runtimeLookup().Get(ctx, task.RuntimeID)
	if err != nil {
		return nil, false, err
	}
	if rt.Provider != "opencode" {
		return &task, false, nil
	}
	return s.failTaskWithTransition(ctx, id, protocol.LauncherUnavailableDetail, "", "", "", "runtime_offline", false, "", "", &claim, pgtype.Timestamptz{Time: stamp, Valid: true})
}

func launcherHandoffEligible(task db.AgentTaskQueue) bool {
	return task.IssueID.Valid && !task.AutopilotRunID.Valid && !task.IsLeaderTask && !task.SquadID.Valid
}

// A peer must be independently healthy and invocable by the original human.
// Agent/runtime bindings, model options and privacy principals are preserved;
// there is no cross-owner, cross-model or host-pinned-directory fallback.
func (s *TaskService) launcherPeer(ctx context.Context, q *db.Queries, task db.AgentTaskQueue, claim protocol.LauncherClaim, only pgtype.UUID) (*db.Agent, error) {
	if !launcherHandoffEligible(task) || strings.TrimSpace(claim.Model) == "" {
		return nil, nil
	}
	source, err := q.GetAgent(ctx, task.AgentID)
	if err != nil {
		return nil, err
	}
	if source.Model.String != claim.Model || source.ThinkingLevel.String != claim.ThinkingLevel || source.ServiceTier.String != claim.ServiceTier {
		return nil, nil
	}
	lookup := s.runtimeLookup()
	lookup.Queries = q
	from, err := lookup.Get(ctx, task.RuntimeID)
	if err != nil {
		return nil, err
	}
	if from.Provider != "opencode" || !from.DaemonID.Valid || from.DaemonID.String == "" {
		return nil, nil
	}
	issue, err := q.GetIssue(ctx, task.IssueID)
	if err != nil {
		return nil, err
	}
	if issue.WorkspaceID != source.WorkspaceID {
		return nil, nil
	}
	if issue.ProjectID.Valid {
		resources, err := q.ListProjectResources(ctx, issue.ProjectID)
		if err != nil {
			return nil, err
		}
		for _, resource := range resources {
			if resource.ResourceType == "local_directory" {
				return nil, nil
			}
		}
	}
	peers, err := q.ListAgents(ctx, source.WorkspaceID)
	if err != nil {
		return nil, err
	}
	for _, peer := range peers {
		if peer.ID == source.ID || (only.Valid && peer.ID != only) || peer.OwnerID != source.OwnerID || !peer.RuntimeID.Valid || peer.RuntimeID == task.RuntimeID || peer.Model.String != claim.Model || peer.ThinkingLevel.String != claim.ThinkingLevel || peer.ServiceTier.String != claim.ServiceTier {
			continue
		}
		if !CanMemberInvokeAgent(ctx, q, peer, task.OriginatorUserID, source.WorkspaceID) {
			continue
		}
		rt, err := lookup.Get(ctx, peer.RuntimeID)
		if err != nil {
			return nil, err
		}
		if rt.Provider != from.Provider || !rt.DaemonID.Valid || rt.DaemonID.String == from.DaemonID.String || rt.Status != "online" || !rt.LastSeenAt.Valid || time.Since(rt.LastSeenAt.Time) > time.Duration(RuntimeClaimFreshnessSeconds)*time.Second {
			continue
		}
		var health struct {
			Version       string `json:"version"`
			OfflineReason struct {
				LauncherUnavailable bool `json:"launcher_unavailable"`
			} `json:"offline_reason"`
		}
		if json.Unmarshal(rt.Metadata, &health) != nil || health.Version == "" || health.OfflineReason.LauncherUnavailable || (rt.Visibility == "private" && !RuntimeAllowsUser(rt, peer.OwnerID)) {
			continue
		}
		probe := task
		probe.AgentID = peer.ID
		if pending, err := hasRunnableSuccessor(ctx, q, probe); err != nil {
			return nil, err
		} else if pending {
			continue
		}
		return &peer, nil
	}
	return nil, nil
}
