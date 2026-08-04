import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";

const routeState = vi.hoisted(() => ({
  pathname: "/scaling-forever/issues/SCA-289",
}));

vi.mock("next/navigation", () => ({
  usePathname: () => routeState.pathname,
  useSearchParams: () => new URLSearchParams(),
}));

vi.mock("@tanstack/react-query", async (importOriginal) => ({
  ...await importOriginal<typeof import("@tanstack/react-query")>(),
  useQuery: () => ({ data: undefined }),
}));

vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => null,
}));

import { DashboardPageTitle } from "./dashboard-page-title";

describe("DashboardPageTitle", () => {
  afterEach(() => {
    cleanup();
    document.title = "";
  });

  it("uses an issue identifier from the path before workspace and issue data resolve", () => {
    document.title = "Project workspace";

    render(<DashboardPageTitle />);

    expect(document.title).toBe("SCA-289");
  });
});
