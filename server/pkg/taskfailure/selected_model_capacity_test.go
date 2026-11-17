package taskfailure

import "testing"

func TestClassifySelectedModelCapacity(t *testing.T) {
	const witness = "Selected model is at capacity. Please try a different model."
	if !SelectedModelAtCapacity(witness) || Classify(witness) != ReasonAgentProviderCapacityOrRateLimit {
		t.Fatal("exact capacity witness must classify as transient capacity")
	}
	for _, old := range []string{"agent_error", string(ReasonAgentUnknown), string(ReasonAgentModelNotFoundOrUnavailable), string(ReasonAgentProviderCapacityOrRateLimit)} {
		if got := NormalizeDaemonReason(old, witness); got != ReasonAgentProviderCapacityOrRateLimit {
			t.Errorf("normalize %s = %s", old, got)
		}
	}
	for _, reason := range []Reason{ReasonAgentProviderAuthOrAccess, ReasonAgentProviderQuotaLimit, ReasonAgentContextOverflow, Reason("cancelled")} {
		if got := NormalizeDaemonReason(string(reason), witness); got != reason {
			t.Errorf("lost stronger reason %s: %s", reason, got)
		}
	}
	for _, raw := range []string{
		"Selected model is unavailable", "model X not found", "unknown model", "HTTP 429",
		"Selected model is not at capacity. Please try a different model.",
		"Selected model is at capacity.", "diagnostic: " + witness,
		witness + " 401 unauthorized", witness + " quota exhausted",
		witness + " maximum context length exceeded", witness + " execution cancelled",
	} {
		if SelectedModelAtCapacity(raw) {
			t.Errorf("non-exclusive witness accepted: %q", raw)
		}
	}
}
