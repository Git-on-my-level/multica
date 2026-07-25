#!/usr/bin/env node
// Human-gated release lane for the fork's signed macOS arm64 Desktop payload.
// It deliberately has no CI entrypoint: Apple credentials remain in the local
// Keychain and an operator invokes this only after approving a release tag.

import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const desktopRoot = resolve(here, "..");
const repoRoot = resolve(desktopRoot, "..", "..");
const keychainProfile = "multica-notary";
const teamID = "JVMXE5G542";
const exactTag = /^v(\d+)\.(\d+)\.(\d+)$/;

export const hermeticReleaseEnv = (env = process.env) => {
  const next = { ...env };
  for (const key of ["GOROOT", "GOPATH", "GOENV_ROOT", "GOENV_SHELL", "GOENV_PATH_ORDER"]) {
    delete next[key];
  }
  return {
    ...next,
    PATH: "/opt/homebrew/bin:/usr/bin:/bin",
    APPLE_KEYCHAIN_PROFILE: keychainProfile,
    APPLE_TEAM_ID: teamID,
  };
};

export function releaseTagAtHead(tags) {
  const matches = tags.filter((tag) => exactTag.test(tag));
  if (matches.length !== 1) {
    throw new Error(`expected exactly one vX.Y.Z tag at HEAD; found: ${matches.join(", ") || "none"}`);
  }
  return matches[0];
}

export function updaterAssetNames(version) {
  const stem = `multica-desktop-${version}-mac-arm64.zip`;
  return {
    dmg: `multica-desktop-${version}-mac-arm64.dmg`,
    manifest: "latest-mac.yml",
    zip: stem,
    blockmap: `${stem}.blockmap`,
  };
}

// electron-builder's update manifest is deliberately parsed narrowly here.
// These fields are its load-bearing updater contract; a malformed or hand-made
// manifest must fail the release before anything is published.
export function parseLatestMacManifest(raw) {
  const field = (name) => raw.match(new RegExp(`^${name}:\\s*([^\\r\\n]+)`, "m"))?.[1]?.trim();
  const version = field("version");
  const path = field("path");
  const sha512 = field("sha512");
  if (!version || !path || !sha512) {
    throw new Error("latest-mac.yml must contain version, path, and sha512");
  }
  return { version, path, sha512 };
}

export function assertUpdaterPayload({ version, manifest, availableNames, zipPath }) {
  const expected = updaterAssetNames(version);
  const parsed = parseLatestMacManifest(manifest);
  if (parsed.version !== version) throw new Error(`manifest version is ${parsed.version}, expected ${version}`);
  if (parsed.path !== expected.zip) throw new Error(`manifest path is ${parsed.path}, expected ${expected.zip}`);
  for (const name of [expected.manifest, expected.zip, expected.blockmap]) {
    if (!availableNames.includes(name)) throw new Error(`required updater asset is missing: ${name}`);
  }
  if (!zipPath || !existsSync(zipPath)) throw new Error(`referenced updater ZIP is missing: ${expected.zip}`);
  const actual = createHash("sha512").update(readFileSync(zipPath)).digest("base64");
  if (actual !== parsed.sha512) throw new Error("latest-mac.yml sha512 does not match its referenced ZIP");
  return expected;
}

function sha256(path) {
  return createHash("sha256").update(readFileSync(path)).digest("hex");
}

function command(command, args, { cwd = repoRoot, capture = false } = {}) {
  return execFileSync(command, args, {
    cwd,
    env: hermeticReleaseEnv(),
    encoding: capture ? "utf-8" : undefined,
    stdio: capture ? ["ignore", "pipe", "inherit"] : "inherit",
  })?.trim();
}

function required(condition, message) {
  if (!condition) throw new Error(message);
}

function verifyDmg(dmg, version) {
  required(existsSync(dmg), `missing DMG: ${dmg}`);
  let mountPoint = "";
  try {
    const attach = command("hdiutil", ["attach", "-nobrowse", "-readonly", dmg], { capture: true });
    mountPoint = attach.split("\n").map((line) => line.split("\t").at(-1)).find((path) => path?.startsWith("/Volumes/")) ?? "";
    required(mountPoint, "could not determine mounted DMG volume");
    const app = resolve(mountPoint, "Multica.app");
    required(existsSync(app), "mounted DMG does not contain Multica.app");
    command("codesign", ["--verify", "--deep", "--strict", "--verbose=4", app]);
    command("spctl", ["-a", "-vv", "-t", "install", app]);
    command("xcrun", ["stapler", "validate", app]);
    required(command("defaults", ["read", resolve(app, "Contents/Info"), "CFBundleShortVersionString"], { capture: true }) === version, "DMG app version differs from release tag");
    required(command("file", [resolve(app, "Contents/MacOS/Multica")], { capture: true }).includes("arm64"), "DMG app is not arm64");
    required(command("file", [resolve(app, "Contents/Resources/bin/multica")], { capture: true }).includes("arm64"), "bundled CLI is not arm64");
  } finally {
    if (mountPoint) command("hdiutil", ["detach", mountPoint]);
  }
}

function preflight() {
  required(process.platform === "darwin" && process.arch === "arm64", "release lane requires an arm64 macOS host");
  required(command("git", ["status", "--porcelain"], { capture: true }) === "", "release checkout must be clean");
  command("git", ["fetch", "origin", "main", "--tags"]);
  const head = command("git", ["rev-parse", "HEAD"], { capture: true });
  const tag = releaseTagAtHead(command("git", ["tag", "--points-at", "HEAD"], { capture: true }).split("\n").filter(Boolean));
  required(command("git", ["rev-parse", "origin/main"], { capture: true }) === head, "release tag target must equal freshly fetched origin/main");
  const goVersion = command("go", ["version"], { capture: true });
  required(/go1\.(2[6-9]|[3-9]\d)\./.test(goVersion), `Go 1.26+ required; got ${goVersion}`);
  required(command("security", ["find-identity", "-v", "-p", "codesigning"], { capture: true }).includes(`Developer ID Application: DAZHENG ZHANG (${teamID})`), "Developer ID Application identity is unavailable");
  // Match electron-builder's actual credential lookup: no compensating
  // --team-id is accepted here, because that would hide a missing profile.
  command("xcrun", ["notarytool", "history", "--keychain-profile", keychainProfile, "--output-format", "json"]);
  return { head, tag, version: tag.slice(1) };
}

function localArtifacts(version) {
  const names = updaterAssetNames(version);
  const dist = resolve(desktopRoot, "dist");
  const availableNames = [names.manifest, names.zip, names.blockmap, names.dmg].filter((name) => existsSync(resolve(dist, name)));
  const manifest = readFileSync(resolve(dist, names.manifest), "utf-8");
  assertUpdaterPayload({ version, manifest, availableNames, zipPath: resolve(dist, names.zip) });
  command("unzip", ["-t", resolve(dist, names.zip)]);
  verifyDmg(resolve(dist, names.dmg), version);
  return { dist, names };
}

function publicArtifacts(tag, version, names, localDmgSha256) {
  const downloadDir = mkdtempSync(resolve(tmpdir(), "multica-macos-release-"));
  try {
    for (const name of Object.values(names)) command("gh", ["release", "download", tag, "--repo", "Git-on-my-level/multica", "--pattern", name, "--dir", downloadDir]);
    const availableNames = Object.values(names).filter((name) => existsSync(resolve(downloadDir, name)));
    const manifest = readFileSync(resolve(downloadDir, names.manifest), "utf-8");
    assertUpdaterPayload({ version, manifest, availableNames, zipPath: resolve(downloadDir, names.zip) });
    command("unzip", ["-t", resolve(downloadDir, names.zip)]);
    required(sha256(resolve(downloadDir, names.dmg)) === localDmgSha256, "downloaded public DMG hash differs from the locally verified DMG");
    verifyDmg(resolve(downloadDir, names.dmg), version);
  } finally {
    rmSync(downloadDir, { recursive: true, force: true });
  }
}

function main() {
  const { tag, version } = preflight();
  command("node", ["scripts/package.mjs", "--mac", "--arm64", "--publish", "never"], { cwd: desktopRoot });
  localArtifacts(version);
  // The normal electron-builder publisher is intentionally invoked only after
  // the signed, notarized, Gatekeeper-validated payload passed all local gates.
  command("node", ["scripts/package.mjs", "--mac", "--arm64", "--publish", "always"], { cwd: desktopRoot });
  const publishedLocal = localArtifacts(version);
  publicArtifacts(tag, version, publishedLocal.names, sha256(resolve(publishedLocal.dist, publishedLocal.names.dmg)));
  console.log(`[release-macos-arm64] verified public Desktop updater payload for ${tag}`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main();
}
