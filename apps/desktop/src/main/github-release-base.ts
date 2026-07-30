import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
  DEFAULT_GITHUB_REPO,
  githubReleasesLatestDownloadUrl,
  githubReleasesUrl,
} from "@multica/core/constants/github";

/** This fork's Desktop default — never fall through to upstream releases. */
export const DESKTOP_DEFAULT_GITHUB_REPO = "Git-on-my-level/multica";

/** Parse owner/repo from electron-builder's packaged app-update.yml. */
export function githubRepoFromAppUpdateYaml(raw: string): string | null {
  const owner = /^owner:\s*(\S+)/m.exec(raw)?.[1];
  const repo = /^repo:\s*(\S+)/m.exec(raw)?.[1];
  return owner && repo ? `${owner}/${repo}` : null;
}

/**
 * Older fork DMGs incorrectly baked upstream into app-update.yml. Remap that
 * single upstream slug to this fork so installed clients stop offering
 * unsigned upstream updates. Any other owner/repo (including other forks) is
 * preserved; MULTICA_GITHUB_REPO still wins first.
 */
export function normalizeDesktopGithubRepo(repo: string): string {
  return repo === DEFAULT_GITHUB_REPO ? DESKTOP_DEFAULT_GITHUB_REPO : repo;
}

/** Resolve the GitHub owner/repo slug for release links and the updater feed. */
export function resolveGithubRepo(): string {
  const fromEnv = process.env.MULTICA_GITHUB_REPO?.trim();
  if (fromEnv) return fromEnv;

  try {
    const raw = readFileSync(join(process.resourcesPath, "app-update.yml"), "utf8");
    const fromUpdateConfig = githubRepoFromAppUpdateYaml(raw);
    if (fromUpdateConfig) return normalizeDesktopGithubRepo(fromUpdateConfig);
  } catch {
    // Dev builds may not ship app-update.yml next to the asar.
  }

  return DESKTOP_DEFAULT_GITHUB_REPO;
}

/** Base URL for GitHub release asset downloads (checksums + CLI archives). */
export function githubLatestDownloadBase(): string {
  return githubReleasesLatestDownloadUrl(resolveGithubRepo());
}

/** Human-facing releases page for the newest GitHub Release. */
export function githubReleasesLatestPageUrl(): string {
  return `${githubReleasesUrl(resolveGithubRepo())}/latest`;
}
