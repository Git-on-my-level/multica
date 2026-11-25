package handler

import "testing"

func TestFoldRepetitiveSignalRootsKeepsLatestSignalAndAuditCount(t *testing.T) {
	zero := 0
	withReplies := 2
	comments := []CommentResponse{
		{ID: "watchdog-1", CreatedAt: "2026-11-25T10:00:00Z", Content: "[flow-watchdog] [0123456789abcdef] issue SCA-1: waiting for owner.\nObserved 2026-11-25T09:55:00Z", ReplyCount: &zero},
		{ID: "ordinary", CreatedAt: "2026-11-25T10:01:00Z", Content: "A hold is discussed here, but this is a human update."},
		{ID: "watchdog-thread", CreatedAt: "2026-11-25T10:02:00Z", Content: "[flow-watchdog] [1123456789abcdef] issue SCA-1: waiting for owner.\nObserved 2026-11-25T09:57:00Z", ReplyCount: &withReplies},
		{ID: "watchdog-2", CreatedAt: "2026-11-25T10:03:00Z", Content: "[flow-watchdog] [2123456789abcdef] issue SCA-1: waiting for owner.\nObserved 2026-11-25T10:02:00Z", ReplyCount: &zero},
		{ID: "watchdog-distinct", CreatedAt: "2026-11-25T10:04:00Z", Content: "[flow-watchdog] [3123456789abcdef] issue SCA-1: fixed, no later owner run.", ReplyCount: &zero},
	}

	got := foldRepetitiveSignalRoots(comments)
	if len(got) != 4 {
		t.Fatalf("returned %d roots, want 4: %+v", len(got), got)
	}
	if got[0].ID != "ordinary" || got[1].ID != "watchdog-thread" || got[2].ID != "watchdog-2" || got[3].ID != "watchdog-distinct" {
		t.Fatalf("roots after folding = %v", []string{got[0].ID, got[1].ID, got[2].ID, got[3].ID})
	}
	if got[2].RepetitiveFoldedCount == nil || *got[2].RepetitiveFoldedCount != 1 || got[2].RepetitiveFirstAt == nil || *got[2].RepetitiveFirstAt != comments[0].CreatedAt {
		t.Fatalf("latest repeated root lacks audit summary: %+v", got[2])
	}
}

func TestFoldRepetitiveHoldRootsNormalizesExpiryOnly(t *testing.T) {
	zero := 0
	comments := []CommentResponse{
		{ID: "hold-1", CreatedAt: "2026-11-25T10:00:00Z", Content: "[flow-owner] The hold on this issue expires soon (hold expires 2026-11-25T11:00:00Z; owner Release). Check the lift evidence.", ReplyCount: &zero},
		{ID: "hold-2", CreatedAt: "2026-11-25T10:15:00Z", Content: "[flow-owner] The hold on this issue expires soon (hold expires 2026-11-25T11:15:00Z; owner Release). Check the lift evidence.", ReplyCount: &zero},
		{ID: "hold-different-reason", CreatedAt: "2026-11-25T10:30:00Z", Content: "[flow-owner] The hold on this issue expires soon (hold expires 2026-11-25T11:30:00Z; owner Release). Wait for the guarded merge.", ReplyCount: &zero},
	}
	got := foldRepetitiveSignalRoots(comments)
	if len(got) != 2 || got[0].ID != "hold-2" || got[1].ID != "hold-different-reason" {
		t.Fatalf("roots after folding = %+v", got)
	}
	if got[0].RepetitiveFoldedCount == nil || *got[0].RepetitiveFoldedCount != 1 {
		t.Fatalf("folded hold count = %v, want 1", got[0].RepetitiveFoldedCount)
	}
}
