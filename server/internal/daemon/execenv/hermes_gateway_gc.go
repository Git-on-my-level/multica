package execenv

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const hermesGatewayHome = "hermes-home"

// HermesGatewayOrphan pins a root that contains only a physical Hermes home.
// Proven requires a stopped gateway record belonging to this exact home.
type HermesGatewayOrphan struct {
	claim  *EnvRootGCClaim
	Bytes  int64
	Age    time.Duration
	Proven bool
}

// OpenHermesGatewayOrphan inspects only the unnamed task layout produced by
// PredictRootDir. The workspace component can use either layout used by the daemon.
func OpenHermesGatewayOrphan(workspacesRoot, taskDir string, now time.Time) *HermesGatewayOrphan {
	rel, err := filepath.Rel(workspacesRoot, taskDir)
	if err != nil || !filepath.IsLocal(rel) {
		return nil
	}
	workspace, name, ok := strings.Cut(rel, string(filepath.Separator))
	if !ok || !strings.HasPrefix(name, "task-") || !hasTaskKey(name[len("task-"):]) {
		return nil
	}
	workspaceID, uuidErr := uuid.Parse(workspace)
	if uuidErr != nil || workspaceID.String() != workspace {
		split := strings.LastIndex(workspace, "-")
		if split < 1 || !hasTaskKey(workspace[split+1:]) ||
			readablePathSegment(workspace[:split], "workspace", workspace[split+1:]) != workspace {
			return nil
		}
	}

	root, err := os.OpenRoot(workspacesRoot)
	if err != nil {
		return nil
	}
	defer root.Close()
	parentInfo, err := root.Lstat(workspace)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeIrregular != 0 {
		return nil
	}
	claim, err := openEnvRootForGC(root, rel, taskDir)
	if err != nil {
		return nil
	}
	actualParent, err := claim.parent.Stat(".")
	if err != nil || !os.SameFile(parentInfo, actualParent) {
		claim.Release()
		return nil
	}
	bytes, newest, proven, matches := inspectHermesGateway(claim.root, false, claim.info.ModTime(), false)
	if !matches {
		claim.Release()
		return nil
	}
	return &HermesGatewayOrphan{claim: claim, Bytes: bytes, Age: now.Sub(newest), Proven: proven}
}

func hasTaskKey(s string) bool {
	if len(s) != taskKeyLen || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func inspectHermesGateway(root *os.Root, locked bool, newest time.Time, gatewayLockHeld bool) (bytes int64, lastWrite time.Time, proven, matches bool) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return 0, newest, false, false
	}
	hasHome := false
	for _, entry := range entries {
		switch {
		case entry.Name() == hermesGatewayHome && entry.IsDir() && entry.Type()&os.ModeIrregular == 0:
			hasHome = true
		case locked && entry.Name() == envRootLockFile:
		default:
			return 0, newest, false, false
		}
	}
	if !hasHome {
		return 0, newest, false, false
	}
	// WalkDir does not follow symlinks. Refuse junctions and unreadable entries.
	err = fs.WalkDir(root.FS(), hermesGatewayHome, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeIrregular != 0 {
			return fs.ErrInvalid
		}
		if info.Mode().IsRegular() {
			bytes += info.Size()
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	if err != nil {
		return 0, newest, false, false
	}
	return bytes, newest, inspectHermesGatewayStatus(root, gatewayLockHeld) == hermesGatewayStopped, true
}

func (o *HermesGatewayOrphan) Close() {
	o.claim.Release()
}

// Remove takes the same execution lock as Prepare. An existing lock entry is
// outside the allowed shape even if no process is currently holding it.
// Keep the empty directory when a stable index points to it, so normal index
// maintenance does not treat the published record as stranded.
func (o *HermesGatewayOrphan) Remove(now time.Time, ttl time.Duration, retainRoot bool) (bytes int64, removed bool, err error) {
	if !o.Proven || o.Age <= ttl {
		return 0, false, nil
	}
	c := o.claim
	if err := c.checkEntry(); err != nil {
		return 0, false, err
	}
	info, err := c.root.Stat(".")
	if err != nil || now.Sub(info.ModTime()) <= ttl {
		return 0, false, err
	}
	lock, err := c.root.OpenFile(envRootLockFile, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if errors.Is(err, os.ErrExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	locked, err := lockFileExclusiveNonBlocking(lock)
	if err != nil || !locked {
		lock.Close()
		return 0, false, err
	}
	c.claim = &EnvRootClaim{lock: lock}
	defer func() {
		// Keep all extra entries, including ones that appeared after the scan.
		// Only unlink the execution lock created by this call.
		if lockErr := c.root.Remove(envRootLockFile); lockErr != nil {
			err = errors.Join(err, lockErr)
		}
		c.claim.Release()
		if err == nil && removed && !retainRoot {
			c.root.Close()
			c.root = nil
			if err = c.checkEntry(); err == nil {
				err = c.parent.Remove(c.name)
			}
		}
	}()
	home, err := c.root.OpenRoot(hermesGatewayHome)
	if err != nil {
		return 0, false, err
	}
	hermesLock, reserved, lockErr := acquireHermesGatewayLock(home)
	closeErr := home.Close()
	if lockErr != nil || closeErr != nil || reserved {
		if hermesLock != nil {
			lockErr = errors.Join(lockErr, releaseHermesGatewayLock(hermesLock))
		}
		return 0, false, errors.Join(lockErr, closeErr)
	}
	if hermesLock != nil {
		defer func() {
			err = errors.Join(err, releaseHermesGatewayLock(hermesLock))
		}()
	}
	bytes, newest, proven, matches := inspectHermesGateway(c.root, true, info.ModTime(), hermesLock != nil)
	if !matches || !proven || now.Sub(newest) <= ttl {
		return 0, false, nil
	}
	if err := c.root.RemoveAll(hermesGatewayHome); err != nil {
		return 0, false, err
	}
	return bytes, true, nil
}

type hermesGatewayStatus int

const (
	hermesGatewayUnknown hermesGatewayStatus = iota
	hermesGatewayAbsent
	hermesGatewayStopped
)

// HermesGatewayMayWrite reports whether cleanup needs to wait for a task's
// detached gateway. Legacy in-process ACP leaves no gateway state or lock.
func HermesGatewayMayWrite(taskDir string) bool {
	root, err := os.OpenRoot(taskDir)
	if err != nil {
		return true
	}
	defer root.Close()

	status := inspectHermesGatewayStatus(root, false)
	return status != hermesGatewayAbsent && status != hermesGatewayStopped
}

func inspectHermesGatewayStatus(root *os.Root, gatewayLockHeld bool) (status hermesGatewayStatus) {
	homeInfo, err := root.Lstat(hermesGatewayHome)
	if errors.Is(err, os.ErrNotExist) {
		return hermesGatewayAbsent
	}
	if err != nil || !homeInfo.IsDir() || homeInfo.Mode()&os.ModeIrregular != 0 {
		return hermesGatewayUnknown
	}
	home, err := root.OpenRoot(hermesGatewayHome)
	if err != nil {
		return hermesGatewayUnknown
	}
	defer home.Close()
	if info, err := home.Stat("."); err != nil || !os.SameFile(info, homeInfo) {
		return hermesGatewayUnknown
	}
	// Probe the lock itself: Hermes intentionally leaves the lock file behind
	// after shutdown, so file existence is not evidence of a live gateway.
	if !gatewayLockHeld {
		lock, reserved, err := acquireHermesGatewayLock(home)
		if err != nil || reserved {
			return hermesGatewayUnknown
		}
		if lock != nil {
			defer func() {
				if err := releaseHermesGatewayLock(lock); err != nil {
					status = hermesGatewayUnknown
				}
			}()
		}
	}
	pidFile, pidFilePresent, err := readHermesGatewayPID(home)
	if err != nil || (pidFilePresent && !hermesGatewayPIDGone(pidFile)) {
		return hermesGatewayUnknown
	}

	const stateFile = "gateway_state.json"
	info, err := home.Lstat(stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return hermesGatewayAbsent
	}
	if err != nil || !info.Mode().IsRegular() {
		return hermesGatewayUnknown
	}
	f, err := home.Open(stateFile)
	if err != nil {
		return hermesGatewayUnknown
	}
	defer f.Close()
	const maxStateBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil || len(data) > maxStateBytes {
		return hermesGatewayUnknown
	}
	var state struct {
		Kind  string `json:"kind"`
		State string `json:"gateway_state"`
		Home  string `json:"hermes_home"`
		PID   *int   `json:"pid"`
	}
	wantHome, err := filepath.Abs(home.Name())
	if err != nil || json.Unmarshal(data, &state) != nil ||
		state.Kind != "hermes-gateway" || state.State != "stopped" ||
		!filepath.IsAbs(state.Home) || filepath.Clean(state.Home) != wantHome {
		return hermesGatewayUnknown
	}
	if state.PID != nil && (*state.PID <= 0 || *state.PID > math.MaxInt32 || !hermesGatewayPIDGone(*state.PID)) {
		return hermesGatewayUnknown
	}
	return hermesGatewayStopped
}

func readHermesGatewayPID(home *os.Root) (int, bool, error) {
	info, err := home.Lstat("gateway.pid")
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !info.Mode().IsRegular() {
		return 0, false, fs.ErrInvalid
	}
	f, err := home.Open("gateway.pid")
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return 0, false, errors.Join(err, fs.ErrInvalid)
	}
	const maxPIDBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxPIDBytes+1))
	if err != nil || len(data) > maxPIDBytes {
		return 0, false, errors.Join(err, fs.ErrInvalid)
	}
	var record struct {
		PID json.RawMessage `json:"pid"`
	}
	var pid int
	if json.Unmarshal(data, &record) == nil && len(record.PID) > 0 {
		if err := json.Unmarshal(record.PID, &pid); err != nil {
			return 0, false, err
		}
	} else {
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return 0, false, err
		}
	}
	if pid <= 0 || pid > math.MaxInt32 {
		return 0, false, fs.ErrInvalid
	}
	return pid, true, nil
}

func acquireHermesGatewayLock(home *os.Root) (*os.File, bool, error) {
	info, err := home.Lstat("gateway.lock")
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fs.ErrInvalid
	}
	f, err := openHermesGatewayLock(home)
	if err != nil {
		return nil, false, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		f.Close()
		return nil, false, errors.Join(err, fs.ErrInvalid)
	}
	reserved, err := tryLockHermesGateway(f)
	if err != nil || reserved {
		return nil, reserved, errors.Join(err, f.Close())
	}
	return f, false, nil
}

func releaseHermesGatewayLock(f *os.File) error {
	return errors.Join(unlockHermesGateway(f), f.Close())
}
