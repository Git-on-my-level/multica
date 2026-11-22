package taskfailure

// HermesGatewayStartingDeadline matches only Hermes' explicit transient failure
// before session/resume has landed. Extra error evidence must stay fail-fast.
func HermesGatewayStartingDeadline(raw string) bool {
	return raw == `hermes session/resume failed: session/resume: Internal error (code=-32603, data={"details":"Gateway starting: deadline"})`
}
