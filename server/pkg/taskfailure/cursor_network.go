package taskfailure

import "strings"

// CursorTransientNetworkError identifies the explicit launch/connection witnesses
// whose recovery requires a proven pre-action boundary, unlike older stream cuts.
func CursorTransientNetworkError(raw string) bool {
	lower := strings.ToLower(strings.TrimSpace(raw))
	if stderr, wrapped := cursorProviderStderr(lower); wrapped {
		lower = stderr
	}
	first, _, _ := strings.Cut(lower, "\n")
	switch strings.TrimSpace(first) {
	case "retriableerror: [unavailable] error",
		"getaddrinfo enotfound api2.cursor.sh",
		"error: getaddrinfo enotfound api2.cursor.sh":
		return true
	}
	return false
}

// CursorPreActionNetworkError requires the adapter's complete diagnostics.
// No result alone is insufficient: even a connection event leaves work ambiguous.
func CursorPreActionNetworkError(raw string) bool {
	if !CursorTransientNetworkError(raw) || Classify(raw) != ReasonAgentProviderNetwork {
		return false
	}
	header, _, ok := strings.Cut(strings.ToLower(strings.TrimSpace(raw)), "; cursor stderr: ")
	if !ok {
		return false
	}
	const diagnostic = " (result_seen=false, exit_code=1, scanner_error=false, event_count=0, invalid_event_count=0, last_event_type=none); actions completed before finalization may already have taken effect"
	// A child that closes stdin before accepting the prompt can exit cleanly.
	// Keep the same no-event boundary and require the prompt-write witness.
	if strings.HasPrefix(header, "cursor-agent prompt write failed: ") &&
		strings.HasSuffix(header, strings.Replace(diagnostic, "exit_code=1", "exit_code=0", 1)) {
		return true
	}
	if !strings.HasSuffix(header, diagnostic) {
		return false
	}
	message := strings.TrimSuffix(header, diagnostic)
	return message == "cursor-agent exited with error: exit status 1" ||
		strings.HasPrefix(message, "cursor-agent prompt write failed: ")
}
