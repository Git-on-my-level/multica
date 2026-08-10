package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func semanticTestUUID(t *testing.T, raw string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := id.Scan(raw); err != nil {
		t.Fatalf("parse UUID: %v", err)
	}
	return id
}

func TestIssueCreateClientKeyFailsClosedBeforeImmediateDispatch(t *testing.T) {
	key := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, assigneeType := range []string{"agent", "squad"} {
		_, err := (&IssueService{}).Create(context.Background(), IssueCreateParams{
			ClientKey: key, Status: "todo",
			AssigneeType: pgtype.Text{String: assigneeType, Valid: true},
		}, IssueCreateOpts{})
		if !errors.Is(err, ErrIssueClientKeyAssignedDispatchUnsupported) {
			t.Errorf("assignee %q error = %v, want ErrIssueClientKeyAssignedDispatchUnsupported", assigneeType, err)
		}
	}
}

func TestIssueCreateSemanticDigestNormalizesSetOrderAndDuplicates(t *testing.T) {
	a := semanticTestUUID(t, "11111111-1111-4111-8111-111111111111")
	b := semanticTestUUID(t, "22222222-2222-4222-8222-222222222222")
	base := IssueCreateParams{
		Title:         "promoted work",
		Description:   pgtype.Text{String: "private brief", Valid: true},
		Status:        "todo",
		Priority:      "none",
		AttachmentIDs: []pgtype.UUID{a, b},
		LabelIDs:      []pgtype.UUID{b, a},
	}
	reordered := base
	reordered.AttachmentIDs = []pgtype.UUID{b, a, a}
	reordered.LabelIDs = []pgtype.UUID{a, b, b}

	first, err := issueCreateSemanticDigest(base)
	if err != nil {
		t.Fatalf("digest base: %v", err)
	}
	second, err := issueCreateSemanticDigest(reordered)
	if err != nil {
		t.Fatalf("digest reordered: %v", err)
	}
	if first != second {
		t.Fatalf("unordered sets produced different digests: %s != %s", first, second)
	}

	changed := base
	changed.Title = "different work"
	third, err := issueCreateSemanticDigest(changed)
	if err != nil {
		t.Fatalf("digest changed: %v", err)
	}
	if third == first {
		t.Fatal("changed semantic input produced the same digest")
	}
}
