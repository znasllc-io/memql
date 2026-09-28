#!/usr/bin/env node
// Run the page runtime's host-to-page checks in headless Chrome.
//
//   npm run gallery:check   -> exit 0 when every check passes, 1 otherwise
//
// Builds dist-gallery/checks/runtime.html from gallery/checks/runtimePage.ts
// (a real kit document) and gallery/checks/runtime.checks.js (the checks),
// loads it with --dump-dom, and reads back the <li data-check> list the
// checks write. It is not part of `npm test` because it needs Chrome; run it
// after changing src/webview/ui/runtime.ts. CHROME=<path> overrides the
// binary, and Chrome runs with a throwaway profile under dist-gallery/.

import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

import * as esbuild from "esbuild";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const CHECKS = path.join(ROOT, "gallery", "checks");
const OUT = path.join(ROOT, "dist-gallery", "checks");
const BUNDLE = path.join(ROOT, "dist-gallery", ".build", "checks.cjs");
const PAGE = path.join(OUT, "runtime.html");
const PROFILE = path.join(ROOT, "dist-gallery", ".chrome-profile-checks");
const CHROME = process.env.CHROME ?? "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const TIMEOUT_MS = 60_000;

function fail(message) {
  console.error(`ERROR: ${message}`);
  process.exit(1);
}

async function buildPage() {
  await esbuild.build({
    entryPoints: [path.join(CHECKS, "runtimePage.ts")],
    bundle: true,
    outfile: BUNDLE,
    platform: "node",
    format: "cjs",
    target: "node20",
    logLevel: "warning",
  });
  const require = createRequire(import.meta.url);
  const { runtimeCheckDocument } = require(BUNDLE);
  const checksJs = fs.readFileSync(path.join(CHECKS, "runtime.checks.js"), "utf8");
  fs.mkdirSync(OUT, { recursive: true });
  fs.writeFileSync(PAGE, runtimeCheckDocument(checksJs));
}

/**
 * The page's DOM once the checks have reported. Headless Chrome prints the
 * DOM and then, on current macOS builds, does not exit; so the closing tag is
 * the signal, and the browser's whole process group is stopped then.
 */
function dumpDom() {
  fs.rmSync(PROFILE, { recursive: true, force: true });
  const args = [
    "--headless=new",
    "--disable-gpu",
    "--no-first-run",
    "--no-default-browser-check",
    `--user-data-dir=${PROFILE}`,
    "--window-size=1000,760",
    "--virtual-time-budget=30000",
    "--dump-dom",
    pathToFileURL(PAGE).href,
  ];
  return new Promise((resolve) => {
    const child = spawn(CHROME, args, { stdio: ["ignore", "pipe", "pipe"], detached: true });
    let out = "";
    let settled = false;
    const finish = () => {
      if (settled) return;
      settled = true;
      clearTimeout(deadline);
      try {
        process.kill(-child.pid, "SIGKILL");
      } catch {
        // already gone
      }
      fs.rmSync(PROFILE, { recursive: true, force: true });
      resolve(out);
    };
    child.stdout.on("data", (chunk) => {
      out += chunk;
      if (out.includes("</html>")) finish();
    });
    child.on("close", finish);
    const deadline = setTimeout(finish, TIMEOUT_MS);
  });
}

function decode(text) {
  return text
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">")
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&amp;/g, "&");
}

async function main() {
  if (!fs.existsSync(CHROME)) fail(`Chrome not found at ${CHROME} (set CHROME=<path>)`);
  await buildPage();
  const dom = await dumpDom();
  const results = [...dom.matchAll(/<li data-check="(pass|fail)">([\s\S]*?)<\/li>/g)].map((m) => ({
    pass: m[1] === "pass",
    text: decode(m[2]),
  }));
  if (results.length === 0) fail(`the checks did not report (open ${path.relative(ROOT, PAGE)} in a browser to see why)`);
  for (const r of results) console.error(`${r.pass ? "PASS" : "FAIL"}: ${r.text}`);
  const failed = results.filter((r) => !r.pass).length;
  console.error(`INFO: ${results.length - failed} of ${results.length} runtime checks pass`);
  if (failed > 0) process.exit(1);
}

main().catch((err) => fail(err instanceof Error ? (err.stack ?? err.message) : String(err)));
