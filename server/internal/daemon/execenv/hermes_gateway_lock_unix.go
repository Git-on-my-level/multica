//go:build !windows

package execenv

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openHermesGatewayLock(home *os.Root) (*os.File, error) {
	return home.OpenFile("gateway.lock", os.O_RDWR, 0)
}

func tryLockHermesGateway(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return false, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return true, nil
	}
	return false, err
}

func unlockHermesGateway(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
