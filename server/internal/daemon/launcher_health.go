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

// Reuse profile drift registration, under its existing register lock and the
// claim barrier, for health recovery. The normal discovery loop calls this;
// no separate timer or scheduler is introduced.
func (d *Daemon) refreshHeldProfileLaunchers(ctx context.Context) {
	d.mu.Lock()
	workspaces := make(map[string]bool)
	for profileID, spec := range d.profileLaunchSpecs {
		if spec.workspaceID != "" && d.providerDemotedLocked("profile:"+profileID) {
			workspaces[spec.workspaceID] = true
		}
	}
	d.mu.Unlock()
	if len(workspaces) == 0 || !d.trySetClaimBarrier() {
		return
	}
	defer d.releaseClaimBarrier()
	ids := make([]string, 0, len(workspaces))
	for id := range workspaces {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		_ = d.withWorkspaceRegisterLock(id, func() error { return d.applyProfileDriftRegistration(ctx, id) })
	}
}

// Demotion must wait for active tasks to settle. The hold already excludes the
// provider from claims, while the existing barrier protects sibling runs.
func (d *Daemon) demoteHeldLaunchers(ctx context.Context) {
	d.mu.Lock()
	causes := make(map[string]runtimeVerdict)
	heldProfile := false
	for _, rt := range d.runtimeIndex {
		record := d.demotedProviders[launcherKey(rt)]
		if rt.ProfileID != "" && record.launcherUnavailable {
			heldProfile = true
		}
		if rt.ProfileID == "" && record.launcherUnavailable {
			causes[rt.Provider] = runtimeVerdict{launcherUnavailable: true, reason: record.evidence, offline: record.offline}
		}
	}
	d.mu.Unlock()
	if len(causes) != 0 {
		d.demoteUnusableRuntimes(ctx, causes)
	}
	if heldProfile {
		d.refreshHeldProfileLaunchers(ctx)
	}
}
