// @vitest-environment node
import { describe, expect, it } from "vitest";
import { WakeupConditionSchema } from "./schemas";

describe("merge return custody boundary", () => {
  const custody = { revision: 9, agent_id: "original-agent", project_id: "project", pr: "owner/repo#7", head: "reviewed-head", branch: "existing-branch", observed_at: "2026-11-17T18:00:00Z", merger_id: "merger" };

  it("preserves the runtime guard when reading a wakeup", () => {
    expect(WakeupConditionSchema.parse({ type: "merge_return", value: custody })).toEqual({ type: "merge_return", value: custody });
  });

  it("rejects missing or malformed custody rather than weakening its predicate", () => {
    expect(WakeupConditionSchema.safeParse({ type: "merge_return", value: { agent_id: "original-agent" } }).success).toBe(false);
    expect(WakeupConditionSchema.safeParse({ type: "merge_return", value: { ...custody, revision: 0 } }).success).toBe(false);
  });
});
