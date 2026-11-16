package handler

import "strings"

// Fork contract (R24, 2026-11-16): a custom runtime profile is provisioned on
// the hosts that carry its command and nowhere else. Every daemon reports each
// profile it could not resolve, and upstream records an offline runtime row
// for each one, so a profile meant for one host left an offline copy on every
// other host in the workspace (five "Claude (account pool)" rows), and the
// copies came back after every daemon restart.
//
// The daemon itself treats a missing command as the expected state on a host
// that is not provisioned for the profile ("skip without failing"). The server
// follows that here: a failure whose only cause is that the command is absent
// is not persisted. Failures that point at a real misconfiguration still are:
// a configured per-machine path override that is not executable (that reason
// starts with "Configured path override ...") and an unsupported runtime type.
//
// Both daemon wordings are covered: the fork/upstream "command not found on
// PATH or provider discovery: <name>" and the older "command not found on
// PATH: <name>".
const unprovisionedProfileReasonPrefix = "command not found on PATH"

// isUnprovisionedProfileFailure reports whether a daemon's failed-profile
// reason means only "this host does not have the profile's command".
func isUnprovisionedProfileFailure(reason string) bool {
	return strings.HasPrefix(strings.TrimSpace(reason), unprovisionedProfileReasonPrefix)
}
