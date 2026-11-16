// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { RuntimeProfile } from "@multica/core/types";
import {
  PENDING_PROFILE_SYNC_WINDOW_MS,
  profilesAwaitingFirstSync,
} from "./pending-runtime-fork";

function row(overrides: Partial<RuntimeProfile> = {}) {
  const profile: RuntimeProfile = {
    id: "profile-1",
    workspace_id: "ws-1",
    display_name: "Claude (account pool)",
    protocol_family: "claude",
    command_name: "claude-pool",
    description: null,
    fixed_args: [],
    visibility: "workspace",
    created_by: "user-1",
    enabled: true,
    created_at: "2026-11-15T13:02:57Z",
    updated_at: "2026-11-15T13:02:57Z",
    ...overrides,
  };
  return { profile, createdAt: Date.parse(profile.created_at) };
}

describe("profilesAwaitingFirstSync", () => {
  const changed = Date.parse("2026-11-15T13:02:57Z");

  it("keeps a just-created profile as a pending placeholder", () => {
    expect(profilesAwaitingFirstSync([row()], changed + 30_000)).toHaveLength(1);
  });

  it("drops an old profile with no row: the machine is not provisioned for it", () => {
    expect(
      profilesAwaitingFirstSync([row()], changed + PENDING_PROFILE_SYNC_WINDOW_MS),
    ).toHaveLength(0);
  });

  it("uses the latest edit, not only creation", () => {
    const edited = row({ updated_at: "2026-11-16T11:00:00Z" });
    expect(
      profilesAwaitingFirstSync([edited], Date.parse("2026-11-16T11:01:00Z")),
    ).toHaveLength(1);
  });

  it("keeps disabled profiles visible regardless of age", () => {
    expect(
      profilesAwaitingFirstSync([row({ enabled: false })], changed + 10 * PENDING_PROFILE_SYNC_WINDOW_MS),
    ).toHaveLength(1);
  });
});
