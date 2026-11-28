//go:build windows

package execenv

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func openHermesGatewayLock(home *os.Root) (*os.File, error) {
	path := filepath.Join(home.Name(), "gateway.lock")
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// Share delete so GC can remove the home while holding this reservation.
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func tryLockHermesGateway(f *os.File) (bool, error) {
	overlapped := windows.Overlapped{Offset: 1024 * 1024}
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &overlapped,
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return true, nil
	}
	return false, err
}

func unlockHermesGateway(f *os.File) error {
	overlapped := windows.Overlapped{Offset: 1024 * 1024}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped)
}
