import { describe, expect, it } from "vitest";
import { render, waitFor } from "@testing-library/react";
import {
  formatEntityPageTitle,
  formatIssuePageTitle,
  truncatePageTitle,
} from "./page-title";
import { dashboardRouteTitle } from "@/components/dashboard-page-title";
import { PageTitle } from "@/components/page-title";

describe("browser page title formatting", () => {
  it("keeps the issue identifier first and truncates only the issue title", () => {
    const title = formatIssuePageTitle(
      "SCA-240",
      "Fix browser tab titles so every open issue remains easy to distinguish at a glance",
    );

    expect(title).toMatch(/^SCA-240 /);
    expect(title).toContain("…");
    expect(title).not.toMatch(/^Multica\s[-—]/);
  });

  it("uses concise non-issue route titles without a brand prefix", () => {
    expect(formatEntityPageTitle("Settings", "Repositories")).toBe("Settings · Repositories");
    expect(formatEntityPageTitle("Issues")).toBe("Issues");
    expect(dashboardRouteTitle("/acme/settings", "repositories").fallback).toBe(
      "Settings · Repositories",
    );
  });

  it("normalizes whitespace before truncating", () => {
    expect(truncatePageTitle("  One\n  title  ")).toBe("One title");
  });

  it("classifies an issue detail path so the first paint can leave Project workspace", () => {
    const route = dashboardRouteTitle(
      "/scaling-forever/issues/SCA-286",
      null,
    );
    expect(route).toEqual({
      fallback: "SCA-286",
      detail: { kind: "issue", id: "SCA-286" },
    });
    expect(formatIssuePageTitle(route.detail?.id, undefined)).toBe("SCA-286");
    expect(
      formatIssuePageTitle(
        "SCA-286",
        "fix(desktop): eliminate recurring Gemini timeout blind spots",
      ),
    ).toMatch(/^SCA-286 /);
  });
});

describe("PageTitle", () => {
  it("renders a title element and syncs document.title on first paint", () => {
    document.title = "Project workspace";
    const label = "SCA-286 fix(desktop): eliminate recurring Gemini…";
    render(<PageTitle title={label} />);
    expect(document.title).toBe(label);
  });

  it("restores its title when another owner resets the layout default", async () => {
    const label = "SCA-286 fix(desktop): eliminate recurring Gemini…";
    render(<PageTitle title={label} />);

    document.title = "Project workspace";

    await waitFor(() => expect(document.title).toBe(label));
  });
});
