package handler

import (
	"regexp"
	"strings"
)

var (
	flowWatchdogIDPattern = regexp.MustCompile(`(?m)(^\[flow-watchdog\]\s+)\[[0-9a-fA-F]{16,64}\]`)
	watchdogObservedAt    = regexp.MustCompile(`(?i)(observed\s+)\d{4}-\d{2}-\d{2}T[0-9:.+Z-]+`)
	flowOwnerHoldExpires  = regexp.MustCompile(`(?i)(hold expires(?: soon)?\s+)\d{4}-\d{2}-\d{2}T[0-9:.+Z-]+`)
	flowOwnerHoldExpired  = regexp.MustCompile(`(?i)(hold[^\n]*?expired\s*\()\d{4}-\d{2}-\d{2}T[0-9:.+Z-]+(\))`)
)

// foldRepetitiveSignalRoots is a read projection only. It keeps the newest
// root for each exact generated watchdog/hold message after removing only its
// per-run ID and observation/expiry timestamp. Distinct event details remain
// distinct, and threads with replies are left intact so their discussion is
// never hidden. The original rows remain queryable with fold_repetitive=false.
func foldRepetitiveSignalRoots(comments []CommentResponse) []CommentResponse {
	groups := make(map[string][]int)
	for i, comment := range comments {
		if comment.ParentID != nil || comment.ReplyCount == nil || *comment.ReplyCount != 0 {
			continue
		}
		if key := repetitiveSignalSignature(comment.Content); key != "" {
			groups[key] = append(groups[key], i)
		}
	}

	latest := make(map[string]bool, len(groups))
	annotations := make(map[int]struct {
		count   int
		firstAt string
	})
	for _, indices := range groups {
		if len(indices) < 2 {
			continue
		}
		last := indices[len(indices)-1]
		latest[comments[last].ID] = true
		annotations[last] = struct {
			count   int
			firstAt string
		}{count: len(indices) - 1, firstAt: comments[indices[0]].CreatedAt}
	}

	out := make([]CommentResponse, 0, len(comments))
	for i, comment := range comments {
		key := repetitiveSignalSignature(comment.Content)
		if key != "" && comment.ParentID == nil && comment.ReplyCount != nil && *comment.ReplyCount == 0 {
			indices := groups[key]
			if len(indices) > 1 {
				if !latest[comment.ID] {
					continue
				}
				annotation := annotations[i]
				count := annotation.count
				firstAt := annotation.firstAt
				comment.RepetitiveFoldedCount = &count
				comment.RepetitiveFirstAt = &firstAt
			}
		}
		out = append(out, comment)
	}
	return out
}

func repetitiveSignalSignature(content string) string {
	switch {
	case strings.HasPrefix(content, "[flow-watchdog]"):
		content = flowWatchdogIDPattern.ReplaceAllString(content, `${1}[signal-id]`)
		content = watchdogObservedAt.ReplaceAllString(content, `${1}<observed-at>`)
		return "watchdog\x00" + content
	case strings.HasPrefix(content, "[flow-owner]") && strings.Contains(strings.ToLower(content), "hold"):
		content = flowOwnerHoldExpires.ReplaceAllString(content, `${1}<expiry>`)
		content = flowOwnerHoldExpired.ReplaceAllString(content, `${1}<expiry>${2}`)
		return "hold\x00" + content
	default:
		return ""
	}
}
