// Gate on package.json's `exports` map (memql#4110).
//
// WHY THIS TEST EXISTS. Every other test in this directory imports the
// SDK by RELATIVE source path (`../src/ai/chat.js`), which is exactly the
// blind spot that let a broken subpath ship: commit 5562a8a8 renamed
// src/si/ -> src/ai/ and every symbol in it, but never touched the
// exports map. `./si` kept pointing at dist/si/index.js -- a path the
// build (bare `tsc`, rootDir src, outDir dist, no postbuild step) can
// never produce -- and the renamed module got no subpath at all, so it
// was unreachable from outside the package. Node's exports resolution
// does not fall back to the real directory when a declared target is
// missing, so a consumer's import fails at module-resolution time while
// `npm test` stays 62/62 green.
//
// The invariant: the build mirrors src/ to dist/ 1:1, so a declared
// subpath is resolvable if and only if its source module exists.

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync, existsSync, readdirSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// Compiled to dist-test/test/exports.test.js, so the package root is two
// levels up from this file at runtime.
const packageRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");

interface ExportEntry {
  types: string;
  import: string;
}

function exportsMap(): Record<string, ExportEntry> {
  const pkg = JSON.parse(
    readFileSync(join(packageRoot, "package.json"), "utf8"),
  ) as { exports: Record<string, ExportEntry> };
  return pkg.exports;
}

// dist/<x>/index.js is emitted from src/<x>/index.ts, and dist/index.js
// from src/index.ts. Map a declared target back to the source that must
// exist for the build to produce it.
function sourceFor(distPath: string): string {
  assert.ok(
    distPath.startsWith("./dist/"),
    `export target must live under ./dist/, got ${distPath}`,
  );
  return join(
    packageRoot,
    distPath.replace("./dist/", "src/").replace(/\.d\.ts$/, ".ts").replace(/\.js$/, ".ts"),
  );
}

test("every declared exports subpath has a source module the build will emit", () => {
  for (const [subpath, entry] of Object.entries(exportsMap())) {
    for (const target of [entry.types, entry.import]) {
      const src = sourceFor(target);
      assert.ok(
        existsSync(src),
        `exports["${subpath}"] declares ${target}, but ${src} does not exist -- ` +
          `the build cannot emit it, so the subpath is dead`,
      );
    }
  }
});

test("every public src/ directory is reachable through a subpath export", () => {
  const declared = new Set(
    Object.values(exportsMap()).map((e) => e.import),
  );
  const missing: string[] = [];
  for (const dirent of readdirSync(join(packageRoot, "src"), { withFileTypes: true })) {
    if (!dirent.isDirectory()) continue;
    if (!existsSync(join(packageRoot, "src", dirent.name, "index.ts"))) continue;
    if (!declared.has(`./dist/${dirent.name}/index.js`)) {
      missing.push(dirent.name);
    }
  }
  assert.deepEqual(
    missing,
    [],
    `src/ directories with an index.ts but no exports subpath -- unreachable ` +
      `from outside the package: ${missing.join(", ")}`,
  );
});
