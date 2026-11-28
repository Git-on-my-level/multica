package service

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// RuntimeAllowsUser evaluates machine-owner consent after workspace membership
// has been checked by the caller. Sharing never grants machine administration.
func RuntimeAllowsUser(rt db.AgentRuntime, userID pgtype.UUID) bool {
	return rt.OwnerID.Valid && userID.Valid && (rt.OwnerID == userID ||
		rt.Visibility == "public" || (rt.Visibility == "private" && slices.Contains(rt.SharedWith, userID)))
}

// PrivateRuntimePolicyPeer returns this runtime owner's peer from the active
// workspace policy. It never changes the owner's persisted sharing list.
func PrivateRuntimePolicyPeer(rt db.AgentRuntime, members []pgtype.UUID) pgtype.UUID {
	if rt.Visibility == "private" && len(members) == 2 {
		switch rt.OwnerID {
		case members[0]:
			return members[1]
		case members[1]:
			return members[0]
		}
	}
	return pgtype.UUID{}
}

// RuntimeAllowsUserWithPolicy extends the owner's grants with the operator's
// active pair. Binding and delivery callers pass their transaction's queries
// and lock the two memberships until commit. NOWAIT avoids waiting on member
// removal while holding the runtime lock that removal needs.
func RuntimeAllowsUserWithPolicy(ctx context.Context, q *db.Queries, rt db.AgentRuntime, userID pgtype.UUID, lockMembership bool) (bool, error) {
	if RuntimeAllowsUser(rt, userID) {
		return true, nil
	}
	if rt.Visibility != "private" || !rt.OwnerID.Valid || !userID.Valid {
		return false, nil
	}
	members, err := q.GetPrivateRuntimePolicyMembers(ctx, rt.WorkspaceID)
	if err != nil || PrivateRuntimePolicyPeer(rt, members) != userID {
		return false, err
	}
	if !lockMembership {
		return true, nil
	}
	locked, err := q.LockRuntimeShareMembers(ctx, db.LockRuntimeShareMembersParams{
		WorkspaceID: rt.WorkspaceID, UserIds: members,
	})
	return len(locked) == len(members) && err == nil, err
}

// RuntimeAllowsAgentWithPolicy checks the workspace boundary before applying
// the policy to an existing binding, including legacy bindings.
func RuntimeAllowsAgentWithPolicy(ctx context.Context, q *db.Queries, rt db.AgentRuntime, agent db.Agent, lockMembership bool) (bool, error) {
	if rt.WorkspaceID != agent.WorkspaceID {
		return RuntimeAllowsUser(rt, agent.OwnerID), nil
	}
	return RuntimeAllowsUserWithPolicy(ctx, q, rt, agent.OwnerID, lockMembership)
}
