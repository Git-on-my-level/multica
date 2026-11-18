// @vitest-environment jsdom

import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { AgentRuntime, RuntimeRateLimitsResponse } from "@multica/core/types";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enRuntimes from "../../locales/en/runtimes.json";

const TEST_RESOURCES = { en: { common: enCommon, runtimes: enRuntimes } };

const getRuntimeRateLimits = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/api", () => ({
  api: { getRuntimeRateLimits },
}));

vi.mock("../../common/use-viewing-timezone", () => ({
  useViewingTimezone: () => "UTC",
}));

import { RateLimitSection } from "./rate-limit-section";

// Mon 2026-11-16 11:46 UTC: 2h46m into the 09:00-14:00 session window.
const NOW = Date.parse("2026-11-16T11:46:00Z");

const RUNTIME: AgentRuntime = {
  id: "r-1",
  workspace_id: "ws-1",
  daemon_id: null,
  name: "Claude Code",
  runtime_mode: "local",
  provider: "claude",
  launch_header: "",
  status: "online",
  device_info: "",
  metadata: {},
  owner_id: null,
  visibility: "private",
  last_seen_at: null,
  created_at: "2026-05-01T00:00:00Z",
  updated_at: "2026-05-01T00:00:00Z",
};

// Fork account pools: the runtime last ran under "default"; "claude-2" is the
// pool's other account.
const SNAPSHOT: RuntimeRateLimitsResponse = {
  rate_limits: {
    account: "default",
    accounts: [
      {
        account: "claude-2",
        plan_type: "max",
        status: "allowed",
        windows: [{ kind: "weekly", used_percent: 12, window_minutes: 10080, resets_at: "2026-11-21T09:00:00Z", status: "allowed" }],
        observed_at: "2026-11-16T10:00:00Z",
      },
      { account: "claude-3", plan_type: "", status: "", windows: [], observed_at: "2026-11-16T10:00:00Z" },
    ],
    plan_type: "pro",
    status: "warning",
    windows: [
      { kind: "session", used_percent: 72, window_minutes: 300, resets_at: "2026-11-16T14:00:00Z", status: "warning" },
      { kind: "weekly", used_percent: 41, window_minutes: 10080, resets_at: "2026-11-19T09:00:00Z", status: "allowed" },
    ],
    credits: { has_credits: true, unlimited: false, balance: "240" },
    observed_at: "2026-11-16T11:44:00Z",
  },
};

function Wrapper({ children }: { children: ReactNode }) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return (
    <QueryClientProvider client={client}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        {children}
      </I18nProvider>
    </QueryClientProvider>
  );
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(NOW);
  getRuntimeRateLimits.mockReset();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("RateLimitSection account pools", () => {
  it("shows the current account first, then each other account with data", async () => {
    getRuntimeRateLimits.mockResolvedValue(SNAPSHOT);
    render(<RateLimitSection runtime={RUNTIME} />, { wrapper: Wrapper });

    const accounts = await screen.findAllByText(/^(default|claude-2|claude-3)$/);
    expect(accounts.map((el) => el.textContent)).toEqual(["default", "claude-2"]);
    // The CLI caption belongs to the current account's block only.
    expect(screen.getAllByText("Reported by the CLI while tasks run")).toHaveLength(1);
    expect(screen.getByText("12% used")).toBeTruthy();
  });

  it("renders without account chips when the runtime has no pool", async () => {
    const { account: _a, accounts: _s, ...plain } = SNAPSHOT.rate_limits!;
    getRuntimeRateLimits.mockResolvedValue({ rate_limits: plain });
    const { container } = render(<RateLimitSection runtime={RUNTIME} />, { wrapper: Wrapper });

    await screen.findByText("Reported by the CLI while tasks run");
    expect(container.querySelector('[data-slot="rate-limit-account"]')).toBeNull();
  });
});
