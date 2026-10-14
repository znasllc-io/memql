#!/usr/bin/env node
// Build the gallery of the email pages: every scenario, in both themes, as a
// static HTML page in dist-gallery/, plus an index linking them all.
//
//   npm run gallery          -> dist-gallery/<id>.<theme>.html + index.html
//   npm run gallery:shoot    -> dist-gallery/shots/<id>.<theme>.<width>.png
//
// The emails in the pages are the engine's own renders: the build first has
// TestWriteEditorGalleryFixtures (component/compose/emaildoc) write them into
// dist-gallery/fixtures, so it needs Go. The two view scripts are bundled into
// dist-gallery/ as the pages load them; gallery/index.ts is bundled the way
// esbuild.test.js bundles a test (vscode aliased to the test stub), loaded in
// this process and asked for its pages. Nothing here is committed: dist-gallery/
// is git-ignored.

import { spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

import * as esbuild from "esbuild";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const REPO = path.resolve(ROOT, "..", "..");
const GALLERY = path.join(ROOT, "gallery");
const OUT = path.join(ROOT, "dist-gallery");
const FIXTURES = path.join(OUT, "fixtures");
const BUNDLE = path.join(OUT, ".build", "gallery.cjs");
const VIEWS = { "email-view": "src/emailView.ts", "new-email-view": "src/newEmailView.ts" };

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

/** The engine renders the emails the pages show, into dist-gallery/fixtures. */
function renderFixtures() {
  fs.rmSync(FIXTURES, { recursive: true, force: true });
  const run = spawnSync("go", ["test", "./component/compose/emaildoc/", "-run", "^TestWriteEditorGalleryFixtures$", "-count=1"], {
    cwd: REPO,
    env: { ...process.env, MEMQL_EMAIL_GALLERY_DIR: FIXTURES },
    stdio: ["ignore", "inherit", "inherit"],
  });
  if (run.error?.code === "ENOENT") fail("go is required to render the email fixtures");
  if (run.error) fail(`the email fixtures could not be rendered: ${run.error.message}`);
  if (run.status !== 0) fail(`the email fixtures could not be rendered (go test exit ${run.status})`);
  if (!fs.existsSync(FIXTURES) || fs.readdirSync(FIXTURES).length === 0) fail("go test wrote no email fixtures");
}

async function bundle() {
  await esbuild.build({
    entryPoints: Object.fromEntries(Object.entries(VIEWS).map(([name, file]) => [name, path.join(ROOT, file)])),
    outdir: OUT,
    bundle: true,
    platform: "browser",
    format: "iife",
    target: "es2022",
    logLevel: "warning",
  });
  await esbuild.build({
    entryPoints: [path.join(GALLERY, "index.ts")],
    bundle: true,
    outfile: BUNDLE,
    platform: "node",
    format: "cjs",
    target: "node20",
    alias: { vscode: path.join(ROOT, "test", "support", "vscode.ts") },
    logLevel: "warning",
  });
}

function escapeHtml(text) {
  return String(text).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

function indexPage(pages) {
  const groups = new Map();
  for (const page of pages) {
    if (!groups.has(page.group)) groups.set(page.group, new Map());
    const byId = groups.get(page.group);
    if (!byId.has(page.id)) byId.set(page.id, { title: page.title, id: page.id, files: [] });
    byId.get(page.id).files.push(page);
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
<title>MemQL Productivity Tools gallery</title>
<style>
  body { font: 14px -apple-system, BlinkMacSystemFont, sans-serif; margin: 32px; color: #191d1a; }
  h1 { font-size: 20px; } h2 { font-size: 15px; margin-top: 28px; }
  ul { list-style: none; padding: 0; } li { padding: 4px 0; }
  .t { display: inline-block; min-width: 34em; } code { color: #5b615c; margin-right: 12px; }
  a { color: #047d5a; margin-right: 8px; }
</style>
</head>
<body>
<h1>MemQL Productivity Tools gallery</h1>
<p>The email pages, each the document its panel would assign, inside VS Code's default webview styles, with emails the engine rendered. <code>npm run gallery:shoot</code> captures them into shots/.</p>
${sections}
</body>
</html>
`;
}

async function main() {
  checkScenarioImports();
  fs.mkdirSync(OUT, { recursive: true });
  renderFixtures();
  await bundle();
  const require = createRequire(import.meta.url);
  const { buildGallery } = require(BUNDLE);
  const pages = buildGallery(FIXTURES);
  for (const name of fs.readdirSync(OUT)) {
    if (name.endsWith(".html")) fs.rmSync(path.join(OUT, name));
  }
  for (const page of pages) fs.writeFileSync(path.join(OUT, page.file), page.html);
  fs.writeFileSync(path.join(OUT, "index.html"), indexPage(pages));
  console.error(`INFO: wrote ${pages.length} pages to ${path.relative(process.cwd(), OUT) || "."}/ (open index.html)`);
}

main().catch((err) => fail(err instanceof Error ? err.stack ?? err.message : String(err)));
