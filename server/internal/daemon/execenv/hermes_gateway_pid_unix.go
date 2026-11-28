//go:build !windows

package execenv

import (
	"errors"
	"syscall"
)

// Signal zero probes the PID without changing the process. Permission errors
// and any other inconclusive result preserve the workspace.
func hermesGatewayPIDGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
