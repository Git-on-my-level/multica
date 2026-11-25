package taskfailure

import (
	"strings"
	"testing"
)

func TestClassifyPerMinuteRateLimit(t *testing.T) {
	const witness = "provider.rate-limit: Rate limit exceeded. Please slow down your requests. Limit: 24290 uncached input tokens/minute."
	for _, tc := range []struct {
		signal string
		want   Reason
	}{
		{"An api key is required", ReasonAgentMissingConfig},
		{"401 unauthorized", ReasonAgentProviderAuthOrAccess},
		{"403 forbidden", ReasonAgentProviderAuthOrAccess},
		{"invalid API key", ReasonAgentProviderAuthOrAccess},
		{"refresh token expired", ReasonAgentProviderAuthOrAccess},
		{"HTTP 402", ReasonAgentProviderQuotaLimit},
		{"monthly usage limit reached", ReasonAgentProviderQuotaLimit},
		{"You've hit your session limit", ReasonAgentProviderQuotaLimit},
		{"You've hit your weekly limit", ReasonAgentProviderQuotaLimit},
		{"insufficient_balance", ReasonAgentProviderQuotaLimit},
	} {
		for _, mixed := range []string{tc.signal + ": " + witness, witness + " " + tc.signal} {
			if got := Classify(mixed); got != tc.want {
				t.Errorf("mixed Classify(%q) = %s, want %s", mixed, got, tc.want)
			}
			for _, reported := range []string{"agent_error.context_overflow", "agent_error.unknown", "agent_error", string(ReasonAgentProviderCapacityOrRateLimit)} {
				if got := NormalizeDaemonReason(reported, mixed); got != tc.want {
					t.Errorf("mixed NormalizeDaemonReason(%q, %q) = %s, want %s", reported, mixed, got, tc.want)
				}
			}
		}
	}
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
	for _, old := range []string{"agent_error.context_overflow", "agent_error.unknown", "agent_error"} {
		if got := NormalizeDaemonReason(old, "prepare execution environment: "+witness); got != ReasonEnvironmentPrepareFailed {
			t.Errorf("preparation failure lost precedence: %s", got)
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
