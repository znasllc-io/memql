#!/usr/bin/env node
// Photograph every gallery page with headless Chrome, wide and narrow.
//
//   npm run gallery:shoot                     -> every page
//   npm run gallery:shoot -- --only=progress  -> pages whose file name contains "progress"
//
// Writes dist-gallery/shots/<id>.<theme>.<width>.png at 1000x760 and 520x760:
// the width of an editor group beside the sidebar, and a narrow split. Chrome
// runs with a throwaway profile under dist-gallery/, so nothing touches the
// browser profile of the person running it. CHROME=<path> overrides the
// binary.

import { spawn } from "node:child_process";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const OUT = path.join(ROOT, "dist-gallery");
const SHOTS = path.join(OUT, "shots");
const PROFILE = path.join(OUT, ".chrome-profile");
const CHROME = process.env.CHROME ?? "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const WIDTHS = [1000, 520];
const HEIGHT = 760;
const PARALLEL = 4;
const CAPTURE_TIMEOUT_MS = 45_000;

function fail(message) {
  console.error(`ERROR: ${message}`);
  process.exit(1);
}

function parseArguments(argv) {
  let only = "";
  for (const arg of argv) {
    if (arg.startsWith("--only=")) only = arg.slice("--only=".length);
    else if (arg === "--help") {
      console.error("Usage: node gallery/shoot.mjs [--only=<substring>]");
      process.exit(0);
    } else fail(`unknown option: ${arg}`);
  }
  return { only };
}

/**
 * One capture. Headless Chrome writes the screenshot and then, on current
 * macOS builds, does not exit; so the file is the signal. Once it exists and
 * its size has held for a poll, the capture is done and the browser is
 * stopped. A hard timeout covers a page that never renders.
 */
function shoot(file, width) {
  const base = path.basename(file, ".html");
  const target = path.join(SHOTS, `${base}.${width}.png`);
  const profile = `${PROFILE}-${width}-${base}`;
  fs.rmSync(target, { force: true });
  fs.rmSync(profile, { recursive: true, force: true });
  const args = [
    "--headless=new",
    "--disable-gpu",
    "--hide-scrollbars",
    "--no-first-run",
    "--no-default-browser-check",
    `--user-data-dir=${profile}`,
    `--screenshot=${target}`,
    `--window-size=${width},${HEIGHT}`,
    "--virtual-time-budget=2000",
    pathToFileURL(file).href,
  ];
  return new Promise((resolve) => {
    // Its own process group, so stopping it takes the renderer and GPU
    // helpers with it rather than leaving them running.
    const child = spawn(CHROME, args, { stdio: ["ignore", "ignore", "pipe"], detached: true });
    let stderr = "";
    let lastSize = -1;
    let settled = false;
    child.stderr.on("data", (chunk) => (stderr += chunk));
    const finish = (ok) => {
      if (settled) return;
      settled = true;
      clearInterval(poll);
      clearTimeout(deadline);
      try {
        process.kill(-child.pid, "SIGKILL");
      } catch {
        // already gone
      }
      fs.rmSync(profile, { recursive: true, force: true });
      resolve({ target, ok, stderr });
    };
    const poll = setInterval(() => {
      const size = fs.existsSync(target) ? fs.statSync(target).size : -1;
      if (size > 0 && size === lastSize) finish(true);
      lastSize = size;
    }, 250);
    const deadline = setTimeout(() => finish(false), CAPTURE_TIMEOUT_MS);
    child.on("close", () => finish(fs.existsSync(target) && fs.statSync(target).size > 0));
  });
}

async function main() {
  const { only } = parseArguments(process.argv.slice(2));
  if (!fs.existsSync(CHROME)) fail(`Chrome not found at ${CHROME} (set CHROME=<path>)`);
  if (!fs.existsSync(OUT)) fail("no dist-gallery/ -- run npm run gallery first");
  const pages = fs
    .readdirSync(OUT)
    .filter((name) => name.endsWith(".html") && name !== "index.html" && name.includes(only))
    .sort()
    .map((name) => path.join(OUT, name));
  if (pages.length === 0) fail(`no gallery pages match "${only}"`);
  fs.mkdirSync(SHOTS, { recursive: true });

  const jobs = pages.flatMap((file) => WIDTHS.map((width) => () => shoot(file, width)));
  const failures = [];
  let next = 0;
  async function worker() {
    while (next < jobs.length) {
      const job = jobs[next];
      next += 1;
      const result = await job();
      if (!result.ok) failures.push(result);
    }
  }
  await Promise.all(Array.from({ length: PARALLEL }, worker));
  for (const f of failures) console.error(`ERROR: no capture for ${path.relative(ROOT, f.target)}\n${f.stderr}`);
  console.error(`INFO: ${jobs.length - failures.length} of ${jobs.length} captures in ${path.relative(ROOT, SHOTS)}/`);
  if (failures.length > 0) process.exit(1);
}

main();
