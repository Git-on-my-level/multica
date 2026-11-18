// @vitest-environment node
import { describe, expect, it } from "vitest";
import { RuntimeRateLimitsResponseSchema } from "./schemas";

// Fork account pools: a pooled runtime reports the account it last ran under
// plus the other accounts' latest snapshots.
describe("RuntimeRateLimitsResponseSchema account pools", () => {
  it("defaults account and accounts on a runtime without a pool", () => {
    const parsed = RuntimeRateLimitsResponseSchema.parse({
      rate_limits: { windows: [], observed_at: "2026-11-19T10:00:00Z" },
    });
    expect(parsed.rate_limits?.account).toBe("");
    expect(parsed.rate_limits?.accounts).toEqual([]);
  });

  it("keeps valid siblings and drops malformed or unnamed ones", () => {
    const parsed = RuntimeRateLimitsResponseSchema.parse({
      rate_limits: {
        account: "default",
        windows: [{ kind: "weekly", used_percent: 91 }],
        observed_at: "2026-11-19T10:00:00Z",
        accounts: [
          { account: "claude-2", windows: [{ kind: "weekly", used_percent: 12 }], observed_at: "2026-11-19T09:00:00Z" },
          { windows: [], observed_at: "2026-11-19T09:00:00Z" },
          { account: "claude-3" },
          "garbage",
        ],
      },
    });
    const rl = parsed.rate_limits;
    expect(rl?.account).toBe("default");
    expect(rl?.accounts).toHaveLength(1);
    expect(rl?.accounts?.[0]).toMatchObject({ account: "claude-2", windows: [{ kind: "weekly", used_percent: 12 }] });
  });

  it("survives a non-array accounts field", () => {
    const parsed = RuntimeRateLimitsResponseSchema.parse({
      rate_limits: { windows: [], observed_at: "2026-11-19T10:00:00Z", accounts: { nope: true } },
    });
    expect(parsed.rate_limits?.accounts).toEqual([]);
  });
});
