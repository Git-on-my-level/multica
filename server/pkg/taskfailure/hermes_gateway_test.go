package taskfailure

import (
	"strings"
	"testing"
)

func TestHermesGatewayStartingDeadline(t *testing.T) {
	const raw = `hermes session/resume failed: session/resume: Internal error (code=-32603, data={"details":"Gateway starting: deadline"})`
	for name, text := range map[string]string{
		"exact":              raw,
		"new session":        strings.ReplaceAll(raw, "session/resume", "session/new"),
		"other backend":      strings.ReplaceAll(raw, "hermes", "kiro"),
		"other error":        strings.ReplaceAll(raw, "Gateway starting: deadline", "Gateway unavailable"),
		"auth":               raw + " authentication failed",
		"other code":         strings.ReplaceAll(raw, "-32603", "-32602"),
		"arbitrary internal": "hermes session/resume failed: Internal error",
	} {
		t.Run(name, func(t *testing.T) {
			if got := HermesGatewayStartingDeadline(text); got != (name == "exact") {
				t.Fatalf("match = %v", got)
			}
		})
	}
}
