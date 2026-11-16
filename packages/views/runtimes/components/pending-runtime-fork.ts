import type { RuntimeProfile } from "@multica/core/types";
import type { PendingRuntimeProfile } from "./pending-runtime";

// Fork contract (R24, 2026-11-16): a custom runtime profile registers only on
// the hosts that carry its command, and the server no longer stores an offline
// row for the others (server/internal/handler/daemon_profile_fork.go). A
// missing row is therefore the normal state on an unprovisioned machine, not a
// sync in progress. Show the "waiting for daemon sync" placeholder only while a
// daemon could still be picking up a profile that was just created or edited.
export const PENDING_PROFILE_SYNC_WINDOW_MS = 10 * 60_000;

function lastChangedAt(profile: RuntimeProfile): number {
  const created = Date.parse(profile.created_at);
  const updated = Date.parse(profile.updated_at);
  return Math.max(
    Number.isFinite(created) ? created : 0,
    Number.isFinite(updated) ? updated : 0,
  );
}

// profilesAwaitingFirstSync keeps disabled profiles (shown as disabled, never
// as registering) and enabled profiles changed within the sync window.
export function profilesAwaitingFirstSync(
  rows: PendingRuntimeProfile[],
  now: number,
): PendingRuntimeProfile[] {
  return rows.filter(
    ({ profile }) =>
      !profile.enabled ||
      now - lastChangedAt(profile) < PENDING_PROFILE_SYNC_WINDOW_MS,
  );
}
