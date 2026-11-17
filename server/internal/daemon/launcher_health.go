package daemon

import (
	"context"
	"errors"
	"os/exec"
	"sort"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

const launcherUnavailableDetail = protocol.LauncherUnavailableDetail

var errLauncherUnavailable = errors.New(launcherUnavailableDetail)

func launcherKey(rt Runtime) string {
	if rt.ProfileID != "" {
		return "profile:" + rt.ProfileID
	}
	return rt.Provider
}

func (d *Daemon) runtimeDemotedLocked(rt Runtime) bool {
	return d.providerDemotedLocked(launcherKey(rt))
}

func (d *Daemon) holdLauncher(key string) {
	d.mu.Lock()
	d.markProvidersDemotedLocked(map[string]runtimeVerdict{
		key: newRuntimeVerdict(builtinProbeLauncherUnavailable, launcherUnavailableDetail, "", false),
	})
	d.mu.Unlock()
	d.logger.Warn("launcher health check failed; suppressing new launches", "launcher", key, "detail", launcherUnavailableDetail)
	d.notifyRuntimeSetChanged()
}

func (d *Daemon) probeOpenCodeLauncherCommand(ctx context.Context, command agent.Command) (string, error) {
	// POSIX-style exit 127 only; Windows cmd's 9009 is not handled.
	for attempt := 0; attempt < runtimeVersionProbeAttempts; attempt++ {
		started := time.Now()
		version, err := detectAgentVersion(ctx, command)
		if err == nil {
			return version, nil
		}
		var exitErr *exec.ExitError
		if ctx.Err() != nil || !errors.As(err, &exitErr) || exitErr.ExitCode() != 127 {
			return "", err
		}
		if time.Since(started) >= runtimeVersionProbeRetryWindow || attempt+1 == runtimeVersionProbeAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(runtimeVersionProbeRetryDelay):
		}
	}
	return "", errLauncherUnavailable
}

// A task's exit code cannot establish whether it performed work. A separate,
// task-free probe can establish launcher health, without replaying that task.
func (d *Daemon) checkFailedLauncher(ctx context.Context, rt Runtime, reason string) {
	if reason != taskfailure.ReasonAgentProcessFailure.String() && reason != taskfailure.ReasonAgentRuntimeMissingExecutable.String() {
		return
	}
	d.checkLauncherHealth(ctx, rt)
}

func (d *Daemon) checkLauncherHealth(ctx context.Context, rt Runtime) {
	if ctx.Err() != nil || rt.Provider != "opencode" {
		return
	}
	// Keep health discovery well inside the 45-second task prepare lease.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, _, _ = d.launcherProbeGroup.Do(launcherKey(rt), func() (any, error) {
		d.probeLauncherHealth(ctx, rt)
		return nil, nil
	})
}

func (d *Daemon) probeLauncherHealth(ctx context.Context, rt Runtime) {
	d.mu.Lock()
	held := d.runtimeDemotedLocked(rt)
	spec, hasSpec := d.profileLaunchSpecs[rt.ProfileID]
	d.mu.Unlock()
	if held {
		return
	}
	if rt.ProfileID != "" {
		if !hasSpec {
			return
		}
		_, err := d.probeOpenCodeLauncherCommand(ctx, agent.NewCommand(spec.path, agent.FilterLaunchPrefix(rt.Provider, spec.fixedArgs, d.logger)))
		if errors.Is(err, errLauncherUnavailable) {
			d.holdLauncher(launcherKey(rt))
		}
		return
	}
	entry, ok := d.agents()[rt.Provider]
	if !ok {
		return
	}
	resolved, _, _ := d.resolveAgentEntryWithHeal(ctx, rt.Provider, entry)
	_, err := d.probeOpenCodeLauncherCommand(ctx, agent.Command{Path: resolved.Path})
	if errors.Is(err, errLauncherUnavailable) {
		d.holdLauncher(launcherKey(rt))
	}
}

// Reuse partial profile registration under the existing workspace register lock.
// Unrelated runtimes and tasks never participate in a launcher's health gate.
func (d *Daemon) refreshHeldProfileLaunchers(ctx context.Context) {
	d.mu.Lock()
	workspaces := make(map[string]bool)
	for profileID, spec := range d.profileLaunchSpecs {
		if spec.workspaceID != "" && d.providerDemotedLocked("profile:"+profileID) {
			workspaces[spec.workspaceID] = true
		}
	}
	d.mu.Unlock()
	ids := make([]string, 0, len(workspaces))
	for id := range workspaces {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		_ = d.withWorkspaceRegisterLock(id, func() error { return d.refreshHeldProfilesForWorkspace(ctx, id) })
	}
}

func (d *Daemon) refreshHeldProfilesForWorkspace(ctx context.Context, workspaceID string) error {
	snapshot, err := d.client.GetRuntimeProfiles(ctx, workspaceID)
	if err != nil {
		return err
	}
	enabled := make(map[string]RuntimeProfile)
	if snapshot != nil {
		for _, profile := range snapshot.RuntimeProfiles {
			if profile.Enabled {
				enabled[profile.ID] = profile
			}
		}
	}
	d.mu.Lock()
	ws := d.workspaces[workspaceID]
	if ws == nil {
		d.mu.Unlock()
		return nil
	}
	previous := make(map[string]demotionRecord)
	var selected []RuntimeProfile
	removed := make(map[string]RuntimeOfflineReason)
	for profileID, spec := range d.profileLaunchSpecs {
		key := "profile:" + profileID
		record, held := d.demotedProviders[key]
		if spec.workspaceID != workspaceID || !held {
			continue
		}
		if profile, exists := enabled[profileID]; exists {
			selected = append(selected, profile)
			previous[key] = record
			continue
		}
		if d.launcherActive[key] != 0 {
			continue
		}
		kept := ws.runtimeIDs[:0:0]
		for _, id := range ws.runtimeIDs {
			if rt := d.runtimeIndex[id]; rt.ProfileID == profileID {
				delete(d.runtimeIndex, id)
				if record.offline != nil {
					removed[id] = *record.offline
				}
			} else {
				kept = append(kept, id)
			}
		}
		ws.runtimeIDs = kept
		delete(d.profileLaunchSpecs, profileID)
		delete(d.demotedProviders, key)
	}
	d.mu.Unlock()
	if len(removed) != 0 {
		ids := make([]string, 0, len(removed))
		for id := range removed {
			ids = append(ids, id)
		}
		d.deregisterDroppedRuntimes(ctx, workspaceID, ids, "held profile removed", removed)
		d.notifyRuntimeSetChanged()
	}
	if len(selected) == 0 {
		return nil
	}
	var runtimes, failures []map[string]string
	d.appendProfileRuntimeEntries(ctx, workspaceID, selected, &runtimes, &failures)
	// Keep an established launch diagnostic sanitized while resolution is
	// unavailable; only a successful probe may replace it with recovery.
	for _, failure := range failures {
		failure["failure_code"] = protocol.LauncherUnavailableCode
		failure["reason"] = launcherUnavailableDetail
		delete(failure, "command_name")
	}
	if len(runtimes) == 0 && len(failures) == 0 {
		return nil
	}
	resp, err := d.client.Register(ctx, map[string]any{
		"workspace_id": workspaceID, "daemon_id": d.cfg.DaemonID,
		"legacy_daemon_ids": d.cfg.LegacyDaemonIDs, "device_name": d.cfg.DeviceName,
		"cli_version": d.cfg.CLIVersion, "launched_by": d.cfg.LaunchedBy,
		"runtimes": runtimes, "failed_profiles": failures,
	})
	if err != nil {
		d.mu.Lock()
		for key, record := range previous {
			if !d.providerDemotedLocked(key) {
				d.demotedProviders[key] = record
			}
		}
		d.mu.Unlock()
		return err
	}
	failed := make(map[string]bool)
	for _, failure := range failures {
		failed[failure["profile_id"]] = true
	}
	d.mu.Lock()
	ws = d.workspaces[workspaceID]
	if ws == nil {
		d.mu.Unlock()
		return nil
	}
	kept := ws.runtimeIDs[:0:0]
	present := make(map[string]bool)
	for _, id := range ws.runtimeIDs {
		rt := d.runtimeIndex[id]
		if failed[rt.ProfileID] && d.launcherActive[launcherKey(rt)] == 0 {
			delete(d.runtimeIndex, id)
			continue
		}
		kept = append(kept, id)
		present[id] = true
	}
	for _, rt := range resp.Runtimes {
		if d.runtimeDemotedLocked(rt) {
			continue
		}
		d.runtimeIndex[rt.ID] = rt
		if !present[rt.ID] {
			kept = append(kept, rt.ID)
			present[rt.ID] = true
		}
	}
	ws.runtimeIDs = kept
	d.mu.Unlock()
	d.notifyRuntimeSetChanged()
	return nil
}

// The hold fences claims immediately. Cleanup waits only for tasks using that
// launcher; profile reporting/recovery runs on the existing discovery tick.
func (d *Daemon) demoteHeldLaunchers(ctx context.Context) {
	d.mu.Lock()
	causes := make(map[string]runtimeVerdict)
	for _, rt := range d.runtimeIndex {
		record := d.demotedProviders[launcherKey(rt)]
		if rt.ProfileID == "" && record.launcherUnavailable {
			causes[rt.Provider] = runtimeVerdict{launcherUnavailable: true, reason: record.evidence, offline: record.offline}
		}
	}
	d.mu.Unlock()
	if len(causes) != 0 {
		d.demoteUnusableRuntimes(ctx, causes)
	}
}
