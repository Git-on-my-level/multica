package service

import (
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"testing"
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
