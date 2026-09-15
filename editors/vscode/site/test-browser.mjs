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
    colorScheme: "light",
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
  const choice = (value) =>
    page.locator(`input[name="appearance"][value="${value}"]`);
  const choose = async (value) => choice(value).locator("..").click();
  const theme = async (expected, preference) => {
    await page.waitForFunction(
      ([resolved, selected]) =>
        document.documentElement.dataset.theme === resolved &&
        document.documentElement.dataset.appearance === selected,
      [expected, preference],
    );
    assert.equal(await choice(preference).isChecked(), true);
    assert.equal(
      await page.evaluate(
        () => getComputedStyle(document.documentElement).colorScheme,
      ),
      expected,
    );
  };
  await theme("light", "system");
  await page.emulateMedia({ colorScheme: "dark" });
  await theme("dark", "system");
  await choose("light");
  await page.reload();
  await theme("light", "light");
  await page.emulateMedia({ colorScheme: "light" });
  await page.emulateMedia({ colorScheme: "dark" });
  await theme("light", "light");
  await choose("system");
  await theme("dark", "system");
  await page.emulateMedia({ colorScheme: "light" });
  await theme("light", "system");
  await choice("system").focus();
  await page.keyboard.press("ArrowRight");
  await theme("light", "light");
  assert.equal(
    await choice("light").evaluate((input) => {
      const style = getComputedStyle(input.nextElementSibling);
      return (
        style.outlineStyle !== "none" && parseFloat(style.outlineWidth) >= 2
      );
    }),
    true,
    "keyboard selection has a visible focus ring",
  );
  assert.ok((await page.locator(".brand-mark").count()) >= 3);
  assert.equal(
    await page.locator(".brand-mark").evaluateAll((marks) =>
      marks.every((mark) => {
        const style = getComputedStyle(mark);
        return (
          style.maskImage.includes("mark.svg") &&
          mark.getBoundingClientRect().width > 0
        );
      }),
    ),
    true,
    "all three marks use canonical geometry",
  );
  assert.equal(
    (await context.request.get(url + "brand/mark.svg")).status(),
    200,
  );
  assert.equal(
    await page.locator("#core-predicates").getAttribute("aria-selected"),
    "true",
  );
  for (const [index, expected] of [
    "concept researchBrief",
    "trait hasDraftStatus",
    "mutation researchBrief",
  ].entries()) {
    await page.locator("[data-core-example]").nth(index).click();
    assert.ok(
      (await page.locator("#core-code").innerText()).includes(expected),
    );
    assert.equal(
      await page.locator("[data-core-example][aria-selected=true]").count(),
      1,
    );
    assert.equal(
      await page.locator("#tab-search").getAttribute("aria-selected"),
      "true",
      "core selection leaves advanced example intact",
    );
  }
  await page.locator("#core-write").focus();
  await page.keyboard.press("Home");
  assert.equal(
    await page.evaluate(() => document.activeElement.id),
    "core-model",
  );
  await page.keyboard.press("ArrowDown");
  assert.equal(
    await page.evaluate(() => document.activeElement.id),
    "core-predicates",
  );
  assert.ok(
    (await page.locator("#core-code").innerText()).includes(
      "spec researchBrief hasResearchAnswer",
    ),
  );
  for (let i = 0; i < 4; i++) {
    await page.locator("[data-example]").nth(i).click();
    assert.equal(
      await page.locator("[data-example][aria-selected=true]").count(),
      1,
    );
    assert.ok(
      (await page.locator("#example-code").innerText()).includes(
        [
          "logic relevantResearchFiles",
          "logic draftResearchAnswer",
          "query researchBrief",
          "automation prepareResearchBrief",
        ][i],
      ),
    );
  }
  await page.locator("#tab-search").focus();
  await page.keyboard.press("ArrowDown");
  assert.equal(
    await page.locator("#tab-ai").getAttribute("aria-selected"),
    "true",
  );
  assert.equal(await page.evaluate(() => document.activeElement.id), "tab-ai");
  await page.keyboard.press("End");
  assert.equal(
    await page.evaluate(() => document.activeElement.id),
    "tab-automation",
  );
  await page.keyboard.press("Home");
  assert.equal(
    await page.evaluate(() => document.activeElement.id),
    "tab-search",
  );
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.locator("#copy-code").click();
  assert.ok(
    (await page.evaluate(() => navigator.clipboard.readText())).includes(
      "logic relevantResearchFiles",
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
  for (const name of ["brief.memql", "memql.toml"]) {
    const response = await context.request.get(url + name);
    assert.equal(response.status(), 200);
    assert.equal(
      await response.text(),
      await readFile(
        path.join(here, "../../../examples/research-desk/research", name),
        "utf8",
      ),
    );
  }
  const guideResponse = await context.request.get(url + "research-guide.md");
  assert.equal(guideResponse.status(), 200);
  assert.ok(
    (await guideResponse.text()).includes("Prerequisites for a real run"),
  );
  await page.locator(".run-guide summary").click();
  assert.equal(await page.locator(".run-guide").getAttribute("open"), "");
  await page.locator(".run-guide summary").click();
  await page.locator("#tab-cache").click();
  // The longest tab must remain readable at narrow widths too.
  await page.locator("#tab-automation").click();
  for (const width of [1440, 768, 390, 320]) {
    await page.setViewportSize({ width, height: 1000 });
    assert.equal(
      await page.evaluate(
        () => document.documentElement.scrollWidth > innerWidth,
      ),
      false,
      `automation overflow at ${width}px`,
    );
  }
  await page.locator("#tab-cache").click();
  const sizes = [1440, 1024, 768, 390, 320];
  for (const appearance of ["light", "dark"]) {
    await choose(appearance);
    await theme(appearance, appearance);
    for (const width of sizes) {
      await page.setViewportSize({ width, height: 1000 });
      await page.evaluate(() => document.fonts.ready);
      assert.equal(
        await page.evaluate(
          () => document.documentElement.scrollWidth > innerWidth,
        ),
        false,
        `no overflow in ${appearance} at ${width}px`,
      );
      if ([1440, 390].includes(width))
        await page.screenshot({
          path: path.join(artifacts, `${appearance}-${width}.png`),
          fullPage: true,
        });
    }
    // Resolve actual rendered colours, including ancestor backgrounds, not just
    // stylesheet token strings. Every selected pair carries ordinary text.
    const checkContrast = async (selectors) => {
      const failures = await page.evaluate((selectors) => {
        const canvas = document.createElement("canvas");
        canvas.width = canvas.height = 1;
        const ctx = canvas.getContext("2d", { willReadFrequently: true });
        const rgba = (color) => {
          ctx.clearRect(0, 0, 1, 1);
          ctx.fillStyle = color;
          ctx.fillRect(0, 0, 1, 1);
          return Array.from(ctx.getImageData(0, 0, 1, 1).data);
        };
        const lum = (rgb) =>
          rgb
            .slice(0, 3)
            .map((v) => {
              v /= 255;
              return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
            })
            .reduce((sum, v, i) => sum + v * [0.2126, 0.7152, 0.0722][i], 0);
        return selectors.flatMap((selector) =>
          Array.from(document.querySelectorAll(selector)).flatMap((el) => {
            let parent = el,
              bg;
            while (parent) {
              bg = rgba(getComputedStyle(parent).backgroundColor);
              if (bg[3] === 255) break;
              parent = parent.parentElement;
            }
            if (!parent) return [`${selector}: no opaque background`];
            const a = lum(rgba(getComputedStyle(el).color)),
              b = lum(bg);
            const ratio = (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
            return ratio < 4.5 ? [`${selector}: ${ratio.toFixed(2)}`] : [];
          }),
        );
      }, selectors);
      assert.deepEqual(
        failures,
        [],
        `${appearance} rendered text contrast >= 4.5:1`,
      );
    };
    await checkContrast([
      ".lead",
      ".primary",
      ".editor-code",
      ".editor .comment",
      ".editor .keyword",
      ".editor .string",
      ".editor .type",
      ".editor-mode",
      ".editor-footer",
      "#example-description",
      ".example-tabs small",
      ".copy",
      ".appearance-controls span",
      ".install-steps p",
    ]);
    await page.locator(".primary").first().hover();
    await checkContrast([".primary"]);
  }
  assert.equal(
    await page.evaluate(
      () => getComputedStyle(document.documentElement).scrollBehavior,
    ),
    "auto",
    "reduced motion disables smooth scrolling",
  );
  const plain = await browser.newContext({
    javaScriptEnabled: false,
    colorScheme: "dark",
  });
  const plainPage = await plain.newPage();
  await plainPage.goto(url);
  assert.ok(
    (await plainPage.locator("#example-code").innerText()).includes(
      "logic relevantResearchFiles",
    ),
  );
  assert.equal(await plainPage.locator("a[download]").count(), 2);
  assert.ok(
    (await plainPage.locator("#core-code").innerText()).includes(
      "trait hasDraftStatus",
    ),
  );
  assert.equal(
    await plainPage.locator(".appearance-controls").isVisible(),
    false,
  );
  const darkGround = await plainPage.evaluate(
    () => getComputedStyle(document.documentElement).backgroundColor,
  );
  await plainPage.emulateMedia({ colorScheme: "light" });
  assert.notEqual(
    await plainPage.evaluate(
      () => getComputedStyle(document.documentElement).backgroundColor,
    ),
    darkGround,
    "CSS follows the OS even without JavaScript",
  );
  await page.evaluate(() =>
    localStorage.setItem("memql-editor-site.appearance", "invalid"),
  );
  await page.reload();
  await theme("light", "system");
  const blocked = await browser.newContext({ colorScheme: "dark" });
  await blocked.addInitScript(() =>
    Object.defineProperty(window, "localStorage", {
      get() {
        throw new DOMException("blocked", "SecurityError");
      },
    }),
  );
  const blockedPage = await blocked.newPage();
  await blockedPage.goto(url);
  assert.equal(
    await blockedPage.locator("html").getAttribute("data-theme"),
    "dark",
  );
  await blockedPage.locator('input[value="light"]').locator("..").click();
  assert.equal(
    await blockedPage.locator("html").getAttribute("data-theme"),
    "light",
    "selection works when storage is denied",
  );
  assert.deepEqual(errors, []);
  console.log(
    JSON.stringify({
      ok: true,
      widths: sizes,
      checks: [
        "canonical marks",
        "System/Light/Dark live OS changes and persistence",
        "invalid and denied storage",
        "rendered text contrast",
        "CSP",
        "core and advanced tab groups",
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
