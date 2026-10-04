#!/usr/bin/env node
// Build the gallery: every scenario, in both themes, as a static HTML page in
// dist-gallery/, plus an index linking them all.
//
//   npm run gallery          -> dist-gallery/<id>.<theme>.html + index.html
//   npm run gallery:shoot    -> dist-gallery/shots/<id>.<theme>.<width>.png
//
// gallery/index.ts is bundled the way esbuild.test.js bundles a test --
// `vscode` aliased to the test stub, so a scenario may drive a real panel
// class -- then loaded in this process and asked for its pages.

import { createRequire } from "node:module";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

import * as esbuild from "esbuild";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const GALLERY = path.join(ROOT, "gallery");
const OUT = path.join(ROOT, "dist-gallery");
const BUNDLE = path.join(OUT, ".build", "gallery.cjs");

function fail(message) {
  console.error(`ERROR: ${message}`);
  process.exit(1);
}

/** Every scenarios/*.ts must be imported by index.ts, or it would never render. */
function checkScenarioImports() {
  const index = fs.readFileSync(path.join(GALLERY, "index.ts"), "utf8");
  const missing = fs
    .readdirSync(path.join(GALLERY, "scenarios"))
    .filter((name) => name.endsWith(".ts"))
    .map((name) => name.replace(/\.ts$/, ""))
    .filter((name) => !index.includes(`./scenarios/${name}.js`));
  if (missing.length > 0) fail(`gallery/index.ts does not import scenarios: ${missing.join(", ")}`);
}

async function bundle() {
  await esbuild.build({
    entryPoints: [path.join(GALLERY, "index.ts")],
    bundle: true,
    outfile: BUNDLE,
    platform: "node",
    format: "cjs",
    target: "node20",
    alias: {
      vscode: path.join(ROOT, "test", "support", "vscodeStub.ts"),
      "vscode-languageclient/node": path.join(ROOT, "test", "support", "languageClientStub.ts"),
    },
    logLevel: "warning",
  });
}

function escapeHtml(text) {
  return String(text).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

function indexPage(pages) {
  const groups = new Map();
  for (const page of pages) {
    const key = `${page.group}\u0000${page.id}`;
    if (!groups.has(page.group)) groups.set(page.group, new Map());
    const byId = groups.get(page.group);
    if (!byId.has(key)) byId.set(key, { title: page.title, id: page.id, files: [] });
    byId.get(key).files.push(page);
  }
  const sections = [...groups.entries()]
    .map(([group, byId]) => {
      const rows = [...byId.values()]
        .map(
          (s) =>
            `<li><span class="t">${escapeHtml(s.title)}</span> <code>${escapeHtml(s.id)}</code> ` +
            s.files.map((f) => `<a href="${escapeHtml(f.file)}">${escapeHtml(f.theme)}</a>`).join(" ") +
            `</li>`,
        )
        .join("\n");
      return `<h2>${escapeHtml(group)}</h2>\n<ul>\n${rows}\n</ul>`;
    })
    .join("\n");
  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>MemQL extension gallery</title>
<style>
  body { font: 14px -apple-system, BlinkMacSystemFont, sans-serif; margin: 32px; color: #191d1a; }
  h1 { font-size: 20px; } h2 { font-size: 15px; margin-top: 28px; }
  ul { list-style: none; padding: 0; } li { padding: 4px 0; }
  .t { display: inline-block; min-width: 22em; } code { color: #5b615c; margin-right: 12px; }
  a { color: #047d5a; margin-right: 8px; }
</style>
</head>
<body>
<h1>MemQL extension gallery</h1>
<p>Every page is the document a panel would assign, inside VS Code's default webview styles. <code>npm run gallery:shoot</code> captures them into shots/.</p>
${sections}
</body>
</html>
`;
}

async function main() {
  checkScenarioImports();
  fs.mkdirSync(OUT, { recursive: true });
  await bundle();
  const require = createRequire(import.meta.url);
  const { buildGallery } = require(BUNDLE);
  const pages = buildGallery();
  for (const name of fs.readdirSync(OUT)) {
    if (name.endsWith(".html")) fs.rmSync(path.join(OUT, name));
  }
  for (const page of pages) fs.writeFileSync(path.join(OUT, page.file), page.html);
  fs.writeFileSync(path.join(OUT, "index.html"), indexPage(pages));
  console.error(`INFO: wrote ${pages.length} pages to ${path.relative(process.cwd(), OUT) || "."}/ (open index.html)`);
}

main().catch((err) => fail(err instanceof Error ? err.stack ?? err.message : String(err)));
