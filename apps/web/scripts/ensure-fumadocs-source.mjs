#!/usr/bin/env node
/**
 * Serialize fumadocs-mdx generation for @multica/web.
 *
 * turbo runs `build` and `typecheck` concurrently. Both used to invoke
 * `fumadocs-mdx` and either raced mid-write on `.source/` or one regenerated
 * the tree while the other was reading it (ENOENT / missing
 * `.source/source.config.mjs` during `next build`).
 *
 * Under an exclusive lock: if the generated tree is already complete, no-op;
 * otherwise generate once. Concurrent callers wait and then observe the ready
 * tree instead of rewriting it.
 */
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const sourceDir = path.join(root, ".source");
const exclusiveLock = path.join(sourceDir, ".generate.exclusive");
const timeoutMs = 120_000;

const READY_FILES = ["index.ts", "source.config.mjs"];

function isReady() {
  return READY_FILES.every((name) => fs.existsSync(path.join(sourceDir, name)));
}

function sleep(ms) {
  spawnSync(process.execPath, ["-e", `Atomics.wait(new Int32Array(new SharedArrayBuffer(4)),0,0,${ms})`], {
    stdio: "ignore",
  });
}

function withExclusiveLock(fn) {
  fs.mkdirSync(sourceDir, { recursive: true });
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

const result = withExclusiveLock(() => {
  if (isReady() && process.env.FUMADOCS_FORCE !== "1") {
    return { status: 0, skipped: true };
  }
  const child = spawnSync("pnpm", ["exec", "fumadocs-mdx"], {
    cwd: root,
    stdio: "inherit",
    shell: process.platform === "win32",
  });
  if ((child.status ?? 1) !== 0) return child;
  if (!isReady()) {
    console.error("fumadocs-mdx finished but .source is incomplete:", READY_FILES);
    return { status: 1 };
  }
  return child;
});

if (result.skipped) {
  process.exit(0);
}

if ((result.status ?? 1) !== 0) {
  process.exit(result.status ?? 1);
}
