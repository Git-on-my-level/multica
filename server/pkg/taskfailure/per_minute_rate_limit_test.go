package taskfailure

import (
	"strings"
	"testing"
)

func TestClassifyPerMinuteRateLimit(t *testing.T) {
	const witness = "provider.rate-limit: Rate limit exceeded. Please slow down your requests. Limit: 24290 uncached input tokens/minute."
	for _, raw := range []string{witness, strings.ToUpper(witness), strings.ReplaceAll(witness, "24290", "1000")} {
		if got := Classify(raw); got != ReasonAgentProviderCapacityOrRateLimit {
			t.Errorf("Classify(%q) = %s", raw, got)
		}
		for _, old := range []string{"agent_error.context_overflow", "agent_error.unknown", "agent_error"} {
			if got := NormalizeDaemonReason(old, raw); got != ReasonAgentProviderCapacityOrRateLimit {
				t.Errorf("NormalizeDaemonReason(%q) = %s", old, got)
			}
		}
		for _, specific := range []Reason{ReasonAgentProviderAuthOrAccess, ReasonAgentProviderQuotaLimit} {
			if got := NormalizeDaemonReason(string(specific), raw); got != specific {
				t.Errorf("lost specific reason %s: %s", specific, got)
			}
		}
	}
	for _, tc := range []struct {
		raw  string
		want Reason
	}{
		{"maximum context length exceeded: token limit", ReasonAgentContextOverflow},
		{"monthly usage limit reached", ReasonAgentProviderQuotaLimit},
		{"You've hit your session limit", ReasonAgentProviderQuotaLimit},
		{"401 unauthorized", ReasonAgentProviderAuthOrAccess},
		{"HTTP 429", ReasonAgentProviderCapacityOrRateLimit},
		{"Rate limit exceeded. Please slow down your requests.", ReasonAgentProviderCapacityOrRateLimit},
	} {
		if PerMinuteRateLimit(tc.raw) {
			t.Errorf("false transient witness: %q", tc.raw)
		}
		if got := Classify(tc.raw); got != tc.want {
			t.Errorf("Classify(%q) = %s, want %s", tc.raw, got, tc.want)
		}
		if got := NormalizeDaemonReason(string(tc.want), tc.raw); got != tc.want {
			t.Errorf("changed unrelated reason: %s", got)
		}
	}
}
