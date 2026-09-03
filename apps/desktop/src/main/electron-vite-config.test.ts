import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

describe("electron.vite.config main bundling", () => {
  it("excludes @multica/core from main externalizeDeps so packaged main does not require raw .ts", () => {
    const src = readFileSync(
      join(dirname(fileURLToPath(import.meta.url)), "../../electron.vite.config.ts"),
      "utf8",
    );
    const mainBlock = src.slice(src.indexOf("main:"), src.indexOf("preload:"));
    expect(mainBlock).toMatch(/externalizeDepsPlugin\(\{\s*exclude:\s*\[[^\]]*["']@multica\/core["']/);
  });
});
