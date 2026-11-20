package agent

import (
	"strings"
	"testing"
)

func TestDevinModelSelection(t *testing.T) {
	const model = "fusion-gpt-6-1-sol-high-sidekick-swe-2-medium"
	for _, tc := range []struct {
		name, requested string
		ok              bool
	}{
		{"exact", model, true},
		{"dot-hyphen", "fusion-gpt-6.1-sol-high-sidekick-swe-2-medium", true},
		{"unavailable-effort", "fusion-gpt-6.1-sol-high-sidekick-swe-2-high", false},
		{"unknown", "missing-model", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, _, requests := runDevinContract(t, ExecOptions{Model: tc.requested}, map[string]string{
				"DEVIN_TEST_SESSION_RESULT": `{"sessionId":"selection-probe","configOptions":[{"id":"model","currentValue":"` + model + `","options":[{"value":"` + model + `"}]}]}`,
			})
			prompted := strings.Contains(requests, `"method":"session/prompt"`)
			if prompted != tc.ok || (result.Status == "completed") != tc.ok {
				t.Fatalf("result=%+v prompted=%v want success=%v", result, prompted, tc.ok)
			}
			if !tc.ok && (!strings.Contains(result.Error, tc.requested) || !strings.Contains(result.Error, "available model ID")) {
				t.Fatalf("missing unavailable ID guidance: %v", result.Error)
			}
		})
	}
}
