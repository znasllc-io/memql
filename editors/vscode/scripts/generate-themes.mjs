#!/usr/bin/env node
// Write themes/memql-{dark,light}-color-theme.json from src/webview/palette.ts.
//
//   node scripts/generate-themes.mjs           write the files
//   node scripts/generate-themes.mjs --check   exit 1 if they are stale
//
// RUN THIS BY HAND when the palette changes, and commit both the palette and
// the generated files. A VSIX packs FILES, not build steps, so the JSON has to
// be in the tree; test/themes.test.ts is the gate that keeps the committed
// bytes equal to what this script would write, and it runs in the ordinary
// `npm test` lane. `--check` is the same comparison from the command line, for
// anyone who would rather ask this script than read a test failure.
//
// WHY IT TRANSPILES RATHER THAN REIMPLEMENTS. The theme mapping lives in
// src/theme/editorThemes.ts, in TypeScript, beside the palette it reads. A
// generator that mapped the palette itself would be a SECOND implementation of
// the thing the drift gate exists to keep single -- the gate would compare the
// committed file against the test's copy of the mapping and never notice the
// generator's copy disagreeing. So this script imports the one real module,
// transpiled through esbuild, which is already this package's devDependency
// and already how every other .ts here becomes runnable JS.

import { build } from "esbuild";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

/**
 * Load editorThemes.ts as a real module.
 *
 * esbuild bundles it (palette.ts comes along) to an in-memory ESM string,
 * which is imported through a data: URL. Nothing is written to disk, so a
 * failed run leaves no stale artifact for the next one to pick up.
 */
async function loadThemeModule() {
  const result = await build({
    entryPoints: [path.join(ROOT, "src", "theme", "editorThemes.ts")],
    bundle: true,
    write: false,
    platform: "node",
    format: "esm",
    target: "node20",
    logLevel: "error",
  });
  const source = result.outputFiles[0].text;
  return import(`data:text/javascript;base64,${Buffer.from(source).toString("base64")}`);
}

async function main() {
  const check = process.argv.includes("--check");
  const { THEME_FILES, buildEditorTheme, serializeTheme } = await loadThemeModule();

  const stale = [];
  for (const variant of ["dark", "light"]) {
    const relative = THEME_FILES[variant];
    const file = path.join(ROOT, relative);
    const wanted = serializeTheme(buildEditorTheme(variant));

    if (check) {
      const current = existsSync(file) ? await readFile(file, "utf8") : "";
      if (current !== wanted) stale.push(relative);
      continue;
    }

    await mkdir(path.dirname(file), { recursive: true });
    await writeFile(file, wanted, "utf8");
    console.error(`INFO: wrote ${relative}`);
  }

  if (check) {
    if (stale.length > 0) {
      console.error(
        `ERROR: stale, run \`node scripts/generate-themes.mjs\` and commit: ${stale.join(", ")}`,
      );
      process.exit(1);
    }
    console.error("INFO: both theme files match palette.ts");
  }
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
