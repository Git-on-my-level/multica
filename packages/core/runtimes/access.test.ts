// @vitest-environment node

import { describe, expect, it } from "vitest";
import type { AgentRuntime } from "../types";
import { isRuntimePlaceableForUser, isRuntimeUsableForUser } from "./access";

const OWNER = "user-owner";
const OTHER = "user-other";

function makeRuntime(overrides: Partial<AgentRuntime> = {}): AgentRuntime {
  return {
    id: "rt-1",
    workspace_id: "ws-1",
    daemon_id: "daemon-1",
    name: "Test Runtime",
    runtime_mode: "local",
    provider: "claude",
    launch_header: "",
    status: "online",
    device_info: "",
    metadata: {},
    owner_id: OWNER,
    visibility: "private",
    last_seen_at: null,
    created_at: "2026-04-01T00:00:00Z",
    updated_at: "2026-04-01T00:00:00Z",
    ...overrides,
  };
}

describe("isRuntimeUsableForUser", () => {
  it("lets the owner use their own private runtime", () => {
    expect(isRuntimeUsableForUser(makeRuntime(), OWNER)).toBe(true);
  });

  it("refuses another member's private runtime", () => {
    expect(isRuntimeUsableForUser(makeRuntime(), OTHER)).toBe(false);
  });

  it("allows only selected members on a shared private runtime", () => {
    const runtime = makeRuntime({ shared_with: [OTHER] });
    expect(isRuntimeUsableForUser(runtime, OTHER)).toBe(true);
    expect(isRuntimeUsableForUser(runtime, OWNER)).toBe(true);
    expect(isRuntimeUsableForUser(runtime, "not-selected")).toBe(false);
    expect(isRuntimeUsableForUser({ ...runtime, owner_id: null }, OTHER)).toBe(false);
  });

  it("lets anyone use a public runtime", () => {
    expect(
      isRuntimeUsableForUser(makeRuntime({ visibility: "public" }), OTHER),
    ).toBe(true);
  });

  it("refuses an ownerless runtime whatever its visibility", () => {
    // No owner means no task token at claim time (MUL-3292), so a bind here
    // could only produce agents that fail the moment they run.
    expect(isRuntimeUsableForUser(makeRuntime({ owner_id: null }), OTHER)).toBe(
      false,
    );
    expect(
      isRuntimeUsableForUser(
        makeRuntime({ owner_id: null, visibility: "public" }),
        OTHER,
      ),
    ).toBe(false);
  });

  it("allows everything while the viewer is unknown, so a loading session never hides the user's own runtime", () => {
    expect(isRuntimeUsableForUser(makeRuntime(), null)).toBe(true);
  });
});

describe("isRuntimePlaceableForUser", () => {
  it("honors an operator peer grant for placement and model discovery", () => {
    const runtime = makeRuntime({ policy_peer_id: OTHER });
    expect(isRuntimePlaceableForUser(runtime, OTHER)).toBe(true);
    expect(isRuntimePlaceableForUser(runtime, OWNER)).toBe(true);
    expect(isRuntimePlaceableForUser(runtime, "third-member")).toBe(false);
    expect(isRuntimePlaceableForUser({ ...runtime, owner_id: null }, OTHER)).toBe(false);
    expect(isRuntimeUsableForUser(runtime, OTHER)).toBe(false);
  });
});
