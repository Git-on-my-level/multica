import { afterEach, describe, expect, it } from "vitest";

import {
  githubLatestDownloadBase,
  githubRepoFromAppUpdateYaml,
  githubReleasesLatestPageUrl,
  normalizeDesktopGithubRepo,
  resolveGithubRepo,
} from "./github-release-base";

describe("githubRepoFromAppUpdateYaml", () => {
  it("parses owner and repo from app-update.yml", () => {
    expect(
      githubRepoFromAppUpdateYaml("owner: acme\nrepo: multica\nprovider: github\n"),
    ).toBe("acme/multica");
  });

  it("returns null when owner or repo is missing", () => {
    expect(githubRepoFromAppUpdateYaml("provider: github\n")).toBeNull();
  });
});

describe("normalizeDesktopGithubRepo", () => {
  it("remaps a wrongly baked upstream feed to this fork", () => {
    expect(normalizeDesktopGithubRepo("multica-ai/multica")).toBe(
      "Git-on-my-level/multica",
    );
  });

  it("preserves non-upstream repos", () => {
    expect(normalizeDesktopGithubRepo("acme/multica")).toBe("acme/multica");
    expect(normalizeDesktopGithubRepo("Git-on-my-level/multica")).toBe(
      "Git-on-my-level/multica",
    );
  });
});

describe("resolveGithubRepo", () => {
  afterEach(() => {
    delete process.env.MULTICA_GITHUB_REPO;
  });

  it("prefers MULTICA_GITHUB_REPO when set", () => {
    process.env.MULTICA_GITHUB_REPO = "acme/multica";
    expect(resolveGithubRepo()).toBe("acme/multica");
  });

  it("falls back to this fork when env is unset", () => {
    expect(resolveGithubRepo()).toBe("Git-on-my-level/multica");
  });
});

describe("githubLatestDownloadBase", () => {
  afterEach(() => {
    delete process.env.MULTICA_GITHUB_REPO;
  });

  it("uses this fork when MULTICA_GITHUB_REPO is unset", () => {
    expect(githubLatestDownloadBase()).toBe(
      "https://github.com/Git-on-my-level/multica/releases/latest/download",
    );
  });

  it("honors MULTICA_GITHUB_REPO for fork/self-host overrides", () => {
    process.env.MULTICA_GITHUB_REPO = "acme/multica";
    expect(githubLatestDownloadBase()).toBe(
      "https://github.com/acme/multica/releases/latest/download",
    );
  });
});

describe("githubReleasesLatestPageUrl", () => {
  afterEach(() => {
    delete process.env.MULTICA_GITHUB_REPO;
  });

  it("points at the latest release page for the resolved repo", () => {
    process.env.MULTICA_GITHUB_REPO = "acme/multica";
    expect(githubReleasesLatestPageUrl()).toBe(
      "https://github.com/acme/multica/releases/latest",
    );
  });
});
