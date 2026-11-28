package service

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/dispatch"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestRuntimeSharedMemberAccess(t *testing.T) {
	owner := util.MustParseUUID("11111111-1111-4111-8111-111111111111")
	member := util.MustParseUUID("22222222-2222-4222-8222-222222222222")
	other := util.MustParseUUID("33333333-3333-4333-8333-333333333333")
	rt := db.AgentRuntime{OwnerID: owner, Visibility: "private", SharedWith: []pgtype.UUID{member}, Status: "online"}
	for _, tc := range []struct {
		id   pgtype.UUID
		want bool
	}{{owner, true}, {member, true}, {other, false}, {pgtype.UUID{}, false}} {
		if got := RuntimeAllowsUser(rt, tc.id); got != tc.want {
			t.Fatalf("access=%v want %v", got, tc.want)
		}
	}
	rt.SharedWith = nil
	if RuntimeAllowsUser(rt, member) {
		t.Fatal("removed member retained access")
	}
	rt.SharedWith = []pgtype.UUID{member}
	rt.OwnerID = pgtype.UUID{}
	if RuntimeAllowsUser(rt, member) {
		t.Fatal("ownerless runtime grants access")
	}
}

func TestAgentReadinessRuntimeAccess(t *testing.T) {
	pool := sharedTestPool(t)
	fixture := testutil.New(pool, "", "")
	tag := uuid.NewString()
	workspace := fixture.Workspace(t, "Runtime readiness workspace", "runtime-readiness-"+tag)
	owner := fixture.User(t, "Runtime owner", "owner-"+tag+"@example.test")
	other := fixture.User(t, "Other member", "member-"+tag+"@example.test")
	fixture.Member(t, workspace, owner, "owner")
	fixture.Member(t, workspace, other, "member")
	fx := testutil.New(pool, workspace, owner)
	privateRuntime := util.MustParseUUID(fx.Runtime(t, "Private runtime"))
	shared := fx.Runtime(t, "Explicitly shared runtime")
	fx.Exec(t, "UPDATE agent_runtime SET shared_with = ARRAY[$2::uuid] WHERE id = $1", shared, other)
	publicRuntime := util.MustParseUUID(fx.Runtime(t, "Public runtime", testutil.Cols{"visibility": "public"}))
	ownerless := util.MustParseUUID(fx.Runtime(t, "Ownerless runtime", testutil.Cols{"owner_id": nil}))
	lookup := RuntimeLookup{Queries: db.New(pool)}

	for _, tc := range []struct {
		name      string
		runtimeID pgtype.UUID
		userID    pgtype.UUID
		reason    dispatch.ReasonCode
	}{
		{"runtime owner", privateRuntime, util.MustParseUUID(owner), ""},
		{"private mismatch", privateRuntime, util.MustParseUUID(other), dispatch.ReasonRuntimeAccessDenied},
		{"explicit share", util.MustParseUUID(shared), util.MustParseUUID(other), ""},
		{"public mismatch", publicRuntime, util.MustParseUUID(other), ""},
		{"ownerless agent", privateRuntime, pgtype.UUID{}, dispatch.ReasonRuntimeAccessDenied},
		{"legacy ownerless runtime", ownerless, util.MustParseUUID(other), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, err := AgentReadiness(t.Context(), lookup, db.Agent{
				WorkspaceID: util.MustParseUUID(workspace), RuntimeID: tc.runtimeID, OwnerID: tc.userID,
			})
			if err != nil || verdict.Reason != tc.reason || verdict.Ready() != (tc.reason == "") {
				t.Fatalf("readiness=%+v, error=%v, want reason=%q", verdict, err, tc.reason)
			}
		})
	}
}
