package protocol

const LauncherUnavailableCode = "launcher_unavailable"
const LauncherUnavailableDetail = "OpenCode launcher health check exited 127; repair the configured launcher and verify its version check succeeds"

// LauncherClaim identifies the exact delivery fenced before task input.
type LauncherClaim struct {
	DispatchedAt  string `json:"dispatched_at"`
	Model         string `json:"model"`
	ThinkingLevel string `json:"thinking_level,omitempty"`
	ServiceTier   string `json:"service_tier,omitempty"`
}
