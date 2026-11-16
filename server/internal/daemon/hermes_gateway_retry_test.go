package daemon

import (
	"github.com/multica-ai/multica/server/pkg/agent"
	"testing"
)

func TestHermesGatewayDeadlineNeverRetriesFresh(t *testing.T) {
	result := agent.Result{Status: "failed", SessionID: "ses_existing", Error: `hermes session/resume failed: session/resume: Internal error (code=-32603, data={"details":"Gateway starting: deadline"})`}
	if shouldRetryWithFreshSession(result, "ses_existing", 0, "hermes") {
		t.Fatal("startup deadline discarded the existing session")
	}
	// Older daemons/backends that have no session in their result must also
	// retain the original fail-fast behavior instead of starting a fresh session.
	result.SessionID = ""
	if shouldRetryWithFreshSession(result, "ses_existing", 0, "hermes") {
		t.Fatal("missing result session caused a fresh-session retry")
	}
}
