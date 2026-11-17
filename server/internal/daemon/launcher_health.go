package daemon

import (
	"context"

	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

const launcherUnavailableDetail = "OpenCode launcher health check exited 127; repair the launcher, then verify opencode --version succeeds"

// A task's exit code cannot establish whether it performed work. A separate,
// task-free probe can establish launcher health, without replaying that task.
func (d *Daemon) checkFailedLauncher(ctx context.Context, rt Runtime, reason string) {
	if reason != taskfailure.ReasonAgentProcessFailure.String() {
		return
	}
	d.checkLauncherHealth(ctx, rt)
}

func (d *Daemon) checkLauncherHealth(ctx context.Context, rt Runtime) {
	if ctx.Err() != nil || rt.Provider != "opencode" || rt.ProfileID != "" {
		return
	}
	_, _, _ = d.launcherProbeGroup.Do(rt.Provider, func() (any, error) {
		d.probeLauncherHealth(ctx, rt)
		return nil, nil
	})
}

func (d *Daemon) probeLauncherHealth(ctx context.Context, rt Runtime) {
	d.mu.Lock()
	held := d.providerDemotedLocked(rt.Provider)
	d.mu.Unlock()
	if held {
		return
	}
	entry, ok := d.agents()[rt.Provider]
	if !ok {
		return
	}
	_, detail, verdict := d.probeBuiltinRuntime(ctx, rt.Provider, entry)
	if verdict != builtinProbeLauncherUnavailable {
		return
	}
	d.mu.Lock()
	d.markProvidersDemotedLocked(map[string]runtimeVerdict{
		rt.Provider: newRuntimeVerdict(verdict, detail, "", false),
	})
	d.mu.Unlock()
	d.notifyRuntimeSetChanged()
}

// Demotion must wait for active tasks to settle. The hold already excludes the
// provider from claims, while the existing barrier protects sibling runs.
func (d *Daemon) demoteHeldLaunchers(ctx context.Context) {
	d.mu.Lock()
	causes := make(map[string]runtimeVerdict)
	for _, rt := range d.runtimeIndex {
		record := d.demotedProviders[rt.Provider]
		if rt.ProfileID == "" && record.launcherUnavailable {
			causes[rt.Provider] = runtimeVerdict{launcherUnavailable: true, reason: record.evidence, offline: record.offline}
		}
	}
	d.mu.Unlock()
	if len(causes) != 0 {
		d.demoteUnusableRuntimes(ctx, causes)
	}
}
