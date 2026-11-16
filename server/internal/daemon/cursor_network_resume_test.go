package daemon

import (
	"testing"

	"github.com/multica-ai/multica/server/pkg/agent"
)

func TestCursorConnectionStalledDoesNotStartFreshSession(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"cursor-agent exited with error: exit status 1 (result_seen=false, exit_code=1, scanner_error=false, event_count=14, invalid_event_count=0, last_event_type=connection); actions completed before finalization may already have taken effect; cursor stderr: RetriableError: [unavailable] Error",
		"cursor-agent exited with error: exit status 1 (result_seen=false, exit_code=1, scanner_error=false, event_count=0, invalid_event_count=0, last_event_type=none); actions completed before finalization may already have taken effect; cursor stderr: Error: getaddrinfo ENOTFOUND api2.cursor.sh",
		"cursor-agent exited with error: exit status 1; cursor stderr: RetriableError: Connection stalled",
		"cursor-agent prompt write failed: write |1: broken pipe; cursor stderr: RetriableError: Connection stalled repeatedly",
	} {
		for _, tools := range []int32{0, 41} {
			result := agent.Result{Status: "failed", Error: raw}
			if shouldRetryWithFreshSession(result, "existing-cursor-session", tools, "cursor") {
				t.Errorf("transport failure must not discard the prior session (tools=%d, error=%q)", tools, raw)
			}
		}
	}
}
