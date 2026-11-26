package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

type hermesPrepGCStats struct {
	found        int
	aged         int
	unproven     int
	removed      int
	dirsRemoved  int
	bytes        int64
	bytesRemoved int64
}

// gcHermesPrepOrphan handles the narrow ownerless Hermes layout separately
// from the normal whole-root cleanup, which requires an owner marker.
func (d *Daemon) gcHermesPrepOrphan(ctx context.Context, taskDir string, stats *gcStats) bool {
	now := time.Now()
	orphan := execenv.OpenHermesPrepOrphan(d.cfg.WorkspacesRoot, taskDir, now)
	if orphan == nil {
		return false
	}
	defer orphan.Close()

	census := &stats.hermesPrepOrphans
	census.found++
	census.bytes += orphan.Bytes
	expired := orphan.Age > d.cfg.GCOrphanTTL
	if expired {
		census.aged++
	}
	if !orphan.Proven {
		census.unproven++
		return false
	}
	if !expired || ctx.Err() != nil {
		stats.skipped++
		return true
	}
	release, reserved := d.reserveEnvRootForGC(taskDir)
	if !reserved {
		stats.skipped++
		return true
	}
	defer release()
	if d.environments.servesFrom(taskDir) {
		stats.skipped++
		return true
	}

	_, indexErr := execenv.IndexedEnvRootOwner(d.cfg.WorkspacesRoot, taskDir)
	retainRoot := !errors.Is(indexErr, execenv.ErrEnvRootNotIndexed)
	if ctx.Err() != nil {
		stats.skipped++
		return true
	}
	bytes, removed, err := orphan.Remove(now, d.cfg.GCOrphanTTL, retainRoot)
	if err != nil {
		d.logger.Warn("gc: remove abandoned Hermes preparation failed", "dir", taskDir, "error", err)
	}
	if !removed {
		stats.skipped++
		return true
	}
	census.removed++
	census.bytesRemoved += bytes
	if !retainRoot && err == nil {
		stats.orphaned++
		census.dirsRemoved++
	}
	stats.bytesReclaimed += bytes
	d.gcUnownedWarnings.Delete(taskDir)
	d.logger.Info("gc: removed", "dir", taskDir, "reason", "abandoned Hermes preparation",
		"bytes_reclaimed", bytes, "retention", d.cfg.GCOrphanTTL, "age", orphan.Age, "retained_index_anchor", retainRoot)
	return true
}
