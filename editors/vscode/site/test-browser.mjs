import { chromium } from "playwright";
import { createServer } from "node:http";
import { readFile, mkdir } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import assert from "node:assert/strict";

const here = path.dirname(fileURLToPath(import.meta.url));
const output = path.join(here, "dist");
const artifacts = path.join(here, "artifacts");
const types = {
  ".html": "text/html",
  ".css": "text/css",
  ".js": "text/javascript",
  ".svg": "image/svg+xml",
  ".woff2": "font/woff2",
};
// An isolated, ephemeral preview. No existing browser, profile, or cluster is used.
const server = createServer(async (req, res) => {
  try {
    const url = new URL(req.url, "http://localhost");
    const file = path.resolve(
      output,
      "." +
        decodeURIComponent(url.pathname === "/" ? "/index.html" : url.pathname),
    );
    if (!file.startsWith(output + path.sep)) {
      res.writeHead(403).end();
      return;
    }
    const data = await readFile(file);
    res
      .writeHead(200, {
        "Content-Type": types[path.extname(file)] ?? "text/plain",
        "Content-Security-Policy":
          "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; object-src 'none'",
      })
      .end(data);
  } catch {
    res.writeHead(404).end();
  }
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const url = `http://127.0.0.1:${server.address().port}/`;
let browser;
try {
  await mkdir(artifacts, { recursive: true });
  browser = await chromium.launch({
    headless: true,
    ...(process.env.SITE_BROWSER_EXECUTABLE
      ? { executablePath: process.env.SITE_BROWSER_EXECUTABLE }
      : {}),
  });
  const context = await browser.newContext({
    viewport: { width: 1440, height: 1100 },
    reducedMotion: "reduce",
  });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("response", (response) => {
    if (response.status() >= 400)
      errors.push(`${response.status()} ${response.url()}`);
  });
  await page.goto(url, { waitUntil: "networkidle" });
  assert.equal(await page.locator("h1").count(), 1);
  assert.equal(
    await page
      .locator("img")
      .evaluateAll((images) =>
        images.every((image) => image.complete && image.naturalWidth > 0),
      ),
    true,
    "all image assets decode",
  );
  for (let i = 0; i < 4; i++) {
    await page.locator("[data-example]").nth(i).click();
    assert.equal(
      await page.locator("[role=tab][aria-selected=true]").count(),
      1,
    );
    assert.ok(
      (await page.locator("#example-code").innerText()).includes(
        [
          "concept readingItem",
          "mutation readingItem",
          "query readingItem",
          "tool listReadingItems",
        ][i],
      ),
    );
  }
  await page.locator("#tab-concept").focus();
  await page.keyboard.press("ArrowDown");
  assert.equal(
    await page.locator("#tab-mutation").getAttribute("aria-selected"),
    "true",
  );
  assert.equal(
    await page.evaluate(() => document.activeElement.id),
    "tab-mutation",
  );
  await page.keyboard.press("End");
  assert.equal(
    await page.evaluate(() => document.activeElement.id),
    "tab-tool",
  );
  await page.keyboard.press("Home");
  assert.equal(
    await page.evaluate(() => document.activeElement.id),
    "tab-concept",
  );
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.locator("#copy-code").click();
  assert.ok(
    (await page.evaluate(() => navigator.clipboard.readText())).includes(
      "concept readingItem",
    ),
  );
  await page.evaluate(() =>
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText: () => Promise.reject(new Error("blocked")) },
      configurable: true,
    }),
  );
  await page.locator("#copy-code").click();
  await page.waitForFunction(
    () =>
      document.querySelector("#copy-code").textContent ===
      "Select text to copy",
  );
  for (const name of ["reading.memql", "memql.toml"]) {
    const response = await context.request.get(url + name);
    assert.equal(response.status(), 200);
    assert.equal(
      await response.text(),
      await readFile(
        path.join(here, "../../../examples/reading-list", name),
        "utf8",
      ),
    );
  }
  await page.locator(".run-guide summary").click();
  assert.equal(await page.locator(".run-guide").getAttribute("open"), "");
  await page.locator(".run-guide summary").click();
  await page.locator("#tab-query").click();
  const sizes = [1440, 1024, 768, 390, 320];
  for (const width of sizes) {
    await page.setViewportSize({ width, height: 1000 });
    await page.evaluate(() => document.fonts.ready);
    assert.equal(
      await page.evaluate(
        () => document.documentElement.scrollWidth > innerWidth,
      ),
      false,
      `no overflow at ${width}px`,
    );
    if ([1440, 390].includes(width))
      await page.screenshot({
        path: path.join(artifacts, `${width}.png`),
        fullPage: true,
      });
  }
  assert.equal(
    await page.evaluate(
      () => getComputedStyle(document.documentElement).scrollBehavior,
    ),
    "auto",
    "reduced motion disables smooth scrolling",
  );
  const plain = await browser.newContext({ javaScriptEnabled: false });
  const plainPage = await plain.newPage();
  await plainPage.goto(url);
  assert.ok(
    (await plainPage.locator("#example-code").innerText()).includes(
      "concept readingItem",
    ),
  );
  assert.equal(await plainPage.locator("a[download]").count(), 2);
  assert.deepEqual(errors, []);
  console.log(
    JSON.stringify({
      ok: true,
      widths: sizes,
      checks: [
        "image decoding",
        "CSP",
        "tabs",
        "keyboard",
        "clipboard success/refusal",
        "downloads",
        "overflow",
        "reduced motion",
        "no-JavaScript fallback",
      ],
      screenshots: artifacts,
    }),
  );
} finally {
  await browser?.close();
  await new Promise((resolve) => server.close(resolve));
}
