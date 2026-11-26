package execenv

import (
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const hermesPrepHome = "hermes-home"

// HermesPrepOrphan pins a root that contains only a physical Hermes overlay.
// Proven distinguishes daemon-generated overlays from paths of the same shape.
type HermesPrepOrphan struct {
	claim  *EnvRootGCClaim
	Bytes  int64
	Age    time.Duration
	Proven bool
}

// OpenHermesPrepOrphan inspects only the unnamed task layout produced by
// PredictRootDir. The workspace component can use either layout used by the daemon.
func OpenHermesPrepOrphan(workspacesRoot, taskDir string, now time.Time) *HermesPrepOrphan {
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
	bytes, newest, proven, matches := inspectHermesPrep(claim.root, false, claim.info.ModTime())
	if !matches {
		claim.Release()
		return nil
	}
	return &HermesPrepOrphan{claim: claim, Bytes: bytes, Age: now.Sub(newest), Proven: proven}
}

func hasTaskKey(s string) bool {
	if len(s) != taskKeyLen || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func inspectHermesPrep(root *os.Root, locked bool, newest time.Time) (bytes int64, lastWrite time.Time, proven, matches bool) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return 0, newest, false, false
	}
	hasHome := false
	for _, entry := range entries {
		switch {
		case entry.Name() == hermesPrepHome && entry.IsDir() && entry.Type()&os.ModeIrregular == 0:
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
	err = fs.WalkDir(root.FS(), hermesPrepHome, func(path string, entry fs.DirEntry, err error) error {
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
		if path == hermesPrepHome+"/"+hermesTaskLocalStateMarker &&
			info.Mode().IsRegular() && info.Size() == int64(len(hermesTaskLocalStateBody)) {
			body, err := root.ReadFile(path)
			proven = err == nil && string(body) == hermesTaskLocalStateBody
		}
		return nil
	})
	return bytes, newest, proven, err == nil
}

func (o *HermesPrepOrphan) Close() {
	o.claim.Release()
}

// Remove takes the same execution lock as Prepare. An existing lock entry is
// outside the allowed shape even if no process is currently holding it.
// Keep the empty directory when a stable index points to it, so normal index
// maintenance does not treat the published record as stranded.
func (o *HermesPrepOrphan) Remove(now time.Time, ttl time.Duration, retainRoot bool) (bytes int64, removed bool, err error) {
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
	bytes, newest, proven, matches := inspectHermesPrep(c.root, true, info.ModTime())
	if !matches || !proven || now.Sub(newest) <= ttl {
		return 0, false, nil
	}
	if err := c.root.RemoveAll(hermesPrepHome); err != nil {
		return 0, false, err
	}
	return bytes, true, nil
}
