#!/usr/bin/env node
/**
 * Serialize fumadocs-mdx generation for @multica/web.
 *
 * turbo runs `build` and `typecheck` concurrently; both used to invoke
 * `fumadocs-mdx` and raced on the shared `.source/` tree (CI ENOENT on
 * `.source/index.ts` / `lstat('.source')`). Hold an exclusive lock around
 * generation so concurrent scripts cannot clobber each other mid-write.
 */
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const sourceDir = path.join(root, ".source");
const exclusiveLock = path.join(sourceDir, ".generate.exclusive");
const timeoutMs = 120_000;

fs.mkdirSync(sourceDir, { recursive: true });

function sleep(ms) {
  spawnSync(process.execPath, ["-e", `Atomics.wait(new Int32Array(new SharedArrayBuffer(4)),0,0,${ms})`], {
    stdio: "ignore",
  });
}

function withExclusiveLock(fn) {
  const start = Date.now();
  while (true) {
    try {
      const fd = fs.openSync(exclusiveLock, "wx");
      try {
        return fn();
      } finally {
        fs.closeSync(fd);
        try {
          fs.unlinkSync(exclusiveLock);
        } catch {
          // ignore
        }
      }
    } catch (err) {
      if (err && typeof err === "object" && "code" in err && err.code === "EEXIST") {
        if (Date.now() - start > timeoutMs) {
          throw new Error(`timed out waiting for fumadocs-mdx lock at ${exclusiveLock}`);
        }
        sleep(50);
        continue;
      }
      throw err;
    }
  }
}

const result = withExclusiveLock(() =>
  spawnSync("pnpm", ["exec", "fumadocs-mdx"], {
    cwd: root,
    stdio: "inherit",
    shell: process.platform === "win32",
  }),
);

if ((result.status ?? 1) !== 0) {
  process.exit(result.status ?? 1);
}
