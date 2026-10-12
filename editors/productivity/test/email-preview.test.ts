import { test } from "node:test";
import assert from "node:assert/strict";
import { JSDOM } from "jsdom";
import { PREVIEW_CSP, previewHTML, skeletonHTML } from "../src/emailPreview.js";
import { EMAIL_COPY, fill } from "../src/emailCopy.js";
import { BASE_STYLES, ICONS, pageCSP, pageShell } from "../src/pageBase.js";
import type { EmailBlock } from "../src/emailDocument.js";

test("the preview keeps the email's layout and block marks, and nothing that runs or reaches out", () => {
  const window = new JSDOM("").window;
  Object.assign(globalThis, { DOMParser: window.DOMParser });
  const html = `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-transitional.dtd"><html><head><meta http-equiv="refresh" content="0;url=https://outside.test"><style>@media (max-width:599px){.ecw{width:100%!important}}</style></head><body style="background-color:#eef0f3;margin:0"><table class="ecw"><tr><td data-block="headline" style="color:#1f5f8b"><a href="https://outside.test" onclick="alert(1)">Hi</a></td><td data-block="Bad Id">x</td></tr></table><script>alert(1)</script><img src="https://tracker.test/p.gif" alt="Pixel"><img src="data:image/png;base64,iVBORw0KGgo=" alt="Logo"></body></html>`;
  const out = previewHTML(html, { blocks: true });
  assert.ok(out.startsWith('<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN"'));
  const doc = new window.DOMParser().parseFromString(out, "text/html");
  assert.equal(doc.body.getAttribute("style"), "background-color:#eef0f3;margin:0");
  assert.equal(doc.querySelector('[data-block="headline"]')?.getAttribute("style"), "color:#1f5f8b");
  assert.equal(doc.querySelectorAll("[data-block]").length, 1, "only block ids survive as marks");
  assert.equal(doc.querySelectorAll("script,[href],[onclick],meta[http-equiv=refresh]").length, 0);
  assert.equal(doc.querySelectorAll("img").length, 1, "remote pictures never load");
  assert.match(doc.querySelector('meta[http-equiv="Content-Security-Policy"]')?.getAttribute("content") ?? "", /img-src data:/);
  assert.match(doc.querySelector("style")?.textContent ?? "", /\.ecw/);
  assert.ok(!previewHTML(html, { blocks: false }).includes("data-block"));
  window.close();
});

// The helpers below are not part of the brief's cases; they keep the extra cases short.
const withDOM = <T>(run: (window: JSDOM["window"]) => T): T => {
  const window = new JSDOM("").window;
  Object.assign(globalThis, { DOMParser: window.DOMParser });
  try { return run(window); } finally { window.close(); }
};
const parse = (html: string) => new JSDOM(html).window.document;
/** Every declaration list in a style attribute of the markup. */
const styles = (html: string) => [...html.matchAll(/ style="([^"]*)"/g)].map(match => match[1] as string);

test("a document without the XHTML doctype previews in standards mode, names the picture it cannot show, and always carries the policy", () => {
  withDOM(window => {
    const out = previewHTML('<html lang="en"><head><title>Offer</title><link rel="stylesheet" href="https://outside.test/a.css"><base href="https://outside.test/"></head><body bgcolor="#ffffff" onload="alert(1)" background="https://tracker.test/b.png"><p>Hello</p><img src="https://tracker.test/p.gif" alt="Acme logo"><img src="https://tracker.test/q.gif"></body></html>', { blocks: true });
    assert.ok(out.startsWith("<!doctype html><html><head>"));
    const doc = new window.DOMParser().parseFromString(out, "text/html");
    assert.equal(doc.body.getAttribute("bgcolor"), "#ffffff");
    assert.equal(doc.body.hasAttribute("onload"), false);
    assert.equal(doc.body.hasAttribute("background"), false);
    assert.equal(doc.querySelectorAll("link,base,title,img").length, 0);
    assert.match(doc.body.textContent ?? "", /\[Image: Acme logo\]\[Image: external asset\]/);
    assert.equal(doc.querySelectorAll("meta").length, 1);
    assert.equal(doc.querySelector("meta")?.getAttribute("content"), PREVIEW_CSP);
  });
});

test("nothing that runs, embeds, submits or reaches out survives the preview, whether or not block marks are kept", () => {
  // One row per kind of hostile markup. Each must be gone from the preview, so adding
  // any of these elements or attributes to the allowlists fails here.
  const cases: Array<[string, string, string]> = [
    ["a form", '<form action="https://outside.test/collect"><input name="q"><button>Send</button></form>', "form"],
    ["a nested frame", '<iframe src="https://outside.test/frame"></iframe>', "iframe"],
    ["an object", '<object data="https://outside.test/plugin.swf"><param name="movie" value="x"></object>', "object"],
    ["an embed", '<embed src="https://outside.test/plugin.swf">', "embed"],
    ["an svg", '<svg><a href="https://outside.test"><text>Hi</text></a><image href="https://tracker.test/s.png"/></svg>', "svg"],
    ["math", '<math><mi xlink:href="https://outside.test">x</mi></math>', "math"],
    ["a picture with sources", '<picture><source srcset="https://tracker.test/a.png 1x"><img src="data:image/png;base64,iVBORw0KGgo=" alt="Logo"></picture>', "picture,source,[srcset]"],
    ["a source on its own", '<source src="https://tracker.test/a.mp4" srcset="https://tracker.test/a.png">', "source"],
    ["a srcset on a picture that is allowed", '<img src="data:image/png;base64,iVBORw0KGgo=" srcset="https://tracker.test/b.png 2x" alt="Logo">', "[srcset]"],
    ["a template", '<template><img src="https://tracker.test/t.png"></template>', "template"],
    ["noscript", '<noscript><img src="https://tracker.test/n.png"></noscript>', "noscript"],
    ["audio", '<audio src="https://tracker.test/a.mp3" autoplay></audio>', "audio"],
    ["video", '<video poster="https://tracker.test/p.png"><source src="https://tracker.test/v.mp4"></video>', "video,source"],
    ["a base address", '<base href="https://outside.test/">', "base"],
    ["a stylesheet link", '<link rel="stylesheet" href="https://outside.test/a.css">', "link"],
    ["a rival policy", '<meta http-equiv="Content-Security-Policy" content="default-src *; img-src *">', 'meta[content*="*"]'],
  ];
  withDOM(window => {
    for (const [name, markup, gone] of cases) {
      // In the head and in the body: the parser moves some of them, and both places must be clean.
      for (const html of [`<html><head>${markup}</head><body><p data-block="intro">Hi</p></body></html>`, `<html><head></head><body><p data-block="intro">Hi</p>${markup}</body></html>`]) {
        for (const blocks of [true, false]) {
          const doc = new window.DOMParser().parseFromString(previewHTML(html, { blocks }), "text/html");
          assert.equal(doc.querySelectorAll(gone).length, 0, `${name} (blocks: ${blocks}) left ${gone}`);
          assert.equal(doc.querySelectorAll("meta").length, 1, `${name}: the preview's own policy is the only meta`);
          assert.equal(doc.querySelector("meta")?.getAttribute("content"), PREVIEW_CSP, name);
          assert.equal(doc.querySelectorAll("[data-block]").length, blocks ? 1 : 0, `${name}: block marks follow the option`);
        }
      }
    }
  });
});

test("a page nonce is a plain token, never a way into the policy", () => {
  for (const nonce of ["x'; script-src *", "", "abc\n", "a b", "abc'", 'abc"']) {
    assert.throws(() => pageCSP(nonce), /plain token/, JSON.stringify(nonce));
    assert.throws(() => pageShell({ title: "T", nonce, script: "s.js", styles: "", body: "" }), /plain token/, JSON.stringify(nonce));
  }
  const hex = "9f86d081884c7d659a2feaa0c55ad015";
  assert.equal(pageCSP(hex), `default-src 'none'; script-src 'nonce-${hex}'; style-src 'unsafe-inline'; img-src data:; frame-src 'self'; form-action 'none'; base-uri 'none'`);
});

test("every keyboard stop on the email pages gets the same visible focus ring", () => {
  // The one focus rule: whatever a keyboard can land on, the ring is 2 pixels of the theme's focus colour.
  const rule = /([^{}]*:focus-visible[^{}]*)\{outline:2px solid var\(--vscode-focusBorder\);outline-offset:2px\}/.exec(BASE_STYLES);
  assert.ok(rule, "the focus rule exists");
  const selectors = rule![1]!.split(",").map(selector => selector.trim());
  for (const stop of ["a", "button", "input", "select", "summary", "textarea", "[tabindex]", '[role="treeitem"]', '[role="radio"]', '[role="switch"]']) {
    assert.ok(selectors.includes(`${stop}:focus-visible`), `${stop} shows the focus ring`);
  }
});

test("a document with nothing but a frameset still previews as an empty page", () => {
  withDOM(window => {
    const out = previewHTML("<html><frameset><frame src='https://outside.test'></frameset></html>", { blocks: false });
    const doc = new window.DOMParser().parseFromString(out, "text/html");
    assert.equal(doc.querySelectorAll("frameset,frame").length, 0);
    assert.equal(doc.body.textContent, "");
    assert.equal(doc.querySelectorAll("meta").length, 1);
  });
});

test("the skeleton follows the blocks and carries no words from the email", () => {
  const blocks: EmailBlock[] = [
    { id: "logo", type: "image", src: "artifact:logo-1", alt: "Acme logo", width: 600, height: 155 },
    { id: "intro", type: "section", blocks: [
      { id: "story", type: "text", text: "<img src=x onerror=alert(1)> ".repeat(12) },
      { id: "offer", type: "highlight", text: "Twenty percent off\nuntil Friday" },
    ] },
    { id: "cta", type: "button", label: "Claim the offer", href: "https://acme.example/offer", width: 305 },
    { id: "pair", type: "columns", columns: [
      { width: 50, blocks: [{ id: "photo", type: "image", src: "", alt: "Winter sale", width: 276 }] },
      { width: 50, blocks: [{ id: "note", type: "text", text: "Free delivery on every order" }] },
    ] },
    { id: "foot", type: "footer", lines: ["Acme Co, 1 Main Street"], links: [{ label: "Unsubscribe", href: "{{unsubscribeUrl}}" }] },
  ];
  const out = skeletonHTML(blocks);
  assert.ok(out.startsWith('<div class="sk-email" aria-hidden="true">') && out.endsWith("</div>"));
  assert.equal((out.match(/class="sk sk-picture"/g) ?? []).length, 2);
  assert.ok(out.includes('<div class="sk sk-picture" style="width:100.00%;aspect-ratio:600/155"></div>'));
  assert.ok(out.includes('<div class="sk sk-button" style="width:50.83%"></div>'));
  assert.equal((out.match(/class="sk-columns"/g) ?? []).length, 1);
  assert.equal((out.match(/class="sk-footer"/g) ?? []).length, 1);
  assert.doesNotMatch(out, /onerror|alert|Claim the offer|Unsubscribe|Main Street|Acme|Winter|delivery|Twenty|Friday/);

  const doc = parse(out);
  const section = doc.querySelector(".sk-section");
  // 348 characters of text make five lines (one per 70), the last at 60 percent; the highlight is one band after them.
  assert.deepEqual([...section!.children].map(child => child.className), [...Array(5).fill("sk sk-line"), "sk sk-band"]);
  assert.equal(section!.children[4]?.getAttribute("style"), "width:60%");
  assert.equal(section!.children[3]?.hasAttribute("style"), false);
  assert.deepEqual([...doc.querySelectorAll(".sk-columns > div")].map(column => column.getAttribute("style")), ["flex:50", "flex:50"]);
  assert.equal(doc.querySelectorAll(".sk-columns .sk-picture").length, 1);
  assert.equal(doc.querySelectorAll(".sk-footer .sk-line").length, 3);
  // The skeleton is a flat list of its blocks' shapes, in order.
  assert.deepEqual([...doc.querySelector(".sk-email")!.children].map(child => child.className), ["sk sk-picture", "sk-section", "sk sk-button", "sk-columns", "sk-footer"]);
});

const SKELETON_OPEN = '<div class="sk-email" aria-hidden="true">';
/** The markup one block adds to the skeleton. */
const shapeOf = (block: Record<string, unknown>) => {
  const out = skeletonHTML([{ id: "b", ...block } as EmailBlock]);
  assert.ok(out.startsWith(SKELETON_OPEN) && out.endsWith("</div>"));
  return out.slice(SKELETON_OPEN.length, -"</div>".length);
};
const LINE = '<div class="sk sk-line"></div>', LAST_LINE = '<div class="sk sk-line" style="width:60%"></div>';

test("each kind of block has its own shape, sized from the block and clamped", () => {
  const cases: Array<[string, Record<string, unknown>, string]> = [
    ["a heading is a fifth shorter than its size", { type: "heading", size: 20 }, '<div class="sk sk-heading sk-center" style="height:16px"></div>'],
    ["a heading without a size takes the renderer's 28", { type: "heading" }, '<div class="sk sk-heading sk-center" style="height:22.4px"></div>'],
    ["a large heading stops at 40", { type: "heading", size: 96 }, '<div class="sk sk-heading sk-center" style="height:40px"></div>'],
    ["a small heading stops at 14", { type: "heading", size: 8 }, '<div class="sk sk-heading sk-center" style="height:14px"></div>'],
    ["a heading is centred, as the renderer draws it, unless it says otherwise", { type: "heading", align: "left" }, '<div class="sk sk-heading" style="height:22.4px"></div>'],
    ["a heading aligned right sits on the right", { type: "heading", align: "right" }, '<div class="sk sk-heading sk-right" style="height:22.4px"></div>'],
    ["a heading is as long as its longest line, at half its size a character", { type: "heading", size: 20, text: "Twelve chars\nSix ch" }, '<div class="sk sk-heading sk-center" style="width:20.00%;height:16px"></div>'],
    ["a short heading is at least 15 percent of its row", { type: "heading", size: 20, text: "Hi" }, '<div class="sk sk-heading sk-center" style="width:15.00%;height:16px"></div>'],
    ["a long heading stops at its row", { type: "heading", size: 40, text: "x".repeat(200) }, '<div class="sk sk-heading sk-center" style="width:100.00%;height:32px"></div>'],
    ["a short text is two lines", { type: "text", text: "x".repeat(70) }, LINE + LAST_LINE],
    ["a centred text ends on a centred line", { type: "text", text: "x".repeat(70), align: "center" }, LINE + '<div class="sk sk-line sk-center" style="width:60%"></div>'],
    ["a text is a line per 70 characters", { type: "text", text: "x".repeat(141) }, LINE + LINE + LAST_LINE],
    ["a long text stops at eight lines", { type: "text", text: "x".repeat(5000) }, LINE.repeat(7) + LAST_LINE],
    ["an empty text is two lines", { type: "text" }, LINE + LAST_LINE],
    ["a highlight is one band", { type: "highlight", text: "One\nTwo" }, '<div class="sk sk-band"></div>'],
    ["a button without a width is 300 pixels", { type: "button", label: "Go" }, '<div class="sk sk-button" style="width:50.00%"></div>'],
    ["a divider is a rule", { type: "divider" }, '<div class="sk-rule"></div>'],
    ["a space is half its height", { type: "spacer", height: 24 }, '<div class="sk-gap" style="height:12px"></div>'],
    ["a thin space stops at 4", { type: "spacer", height: 1 }, '<div class="sk-gap" style="height:4px"></div>'],
    ["a tall space stops at 60", { type: "spacer", height: 1000 }, '<div class="sk-gap" style="height:60px"></div>'],
    ["social links are three icons", { type: "social", links: [{ network: "Mail" }] }, '<div class="sk-social"><i></i><i></i><i></i></div>'],
    ["a footer is three short lines", { type: "footer", lines: ["Acme Co"] }, `<div class="sk-footer">${LINE.repeat(3)}</div>`],
    ["HTML is two lines", { type: "html", html: "<p>Hello</p>" }, LINE + LAST_LINE],
    ["a picture is its share of 600 pixels at its own ratio", { type: "image", width: 301 }, '<div class="sk sk-picture" style="width:50.17%;aspect-ratio:301/151"></div>'],
    ["a picture with a height keeps it", { type: "image", width: 100, height: 40 }, '<div class="sk sk-picture" style="width:16.67%;aspect-ratio:100/40"></div>'],
    ["a section holds its blocks", { type: "section", blocks: [{ id: "c", type: "divider" }] }, '<div class="sk-section"><div class="sk-rule"></div></div>'],
    ["a section without blocks is empty", { type: "section" }, '<div class="sk-section"></div>'],
    ["columns share their row by width", { type: "columns", columns: [{ width: 33.33, blocks: [{ id: "c", type: "divider" }] }, { width: 66.67 }] }, '<div class="sk-columns"><div style="flex:33.33"><div class="sk-rule"></div></div><div style="flex:66.67"></div></div>'],
    // In a column, a picture or a button takes its share of the column: 150 pixels of a half of 600 is half the column.
    ["a picture in a column is its share of the column", { type: "columns", columns: [{ width: 50, blocks: [{ id: "c", type: "image", width: 150, height: 100 }] }, { width: 50 }] }, '<div class="sk-columns"><div style="flex:50"><div class="sk sk-picture" style="width:50.00%;aspect-ratio:150/100"></div></div><div style="flex:50"></div></div>'],
    ["a picture wider than its column fills it", { type: "columns", columns: [{ width: 25, blocks: [{ id: "c", type: "image", width: 600, height: 300 }] }] }, '<div class="sk-columns"><div style="flex:25"><div class="sk sk-picture" style="width:100.00%;aspect-ratio:600/300"></div></div></div>'],
    ["a button in a column is its share of the column", { type: "columns", columns: [{ width: 50, blocks: [{ id: "c", type: "button", width: 150 }] }] }, '<div class="sk-columns"><div style="flex:50"><div class="sk sk-button" style="width:50.00%"></div></div></div>'],
    ["a text in a column takes more lines", { type: "columns", columns: [{ width: 50, blocks: [{ id: "c", type: "text", text: "x".repeat(140) }] }] }, `<div class="sk-columns"><div style="flex:50">${LINE.repeat(3)}${LAST_LINE}</div></div>`],
    ["columns narrower than their row stand centred at their own width", { type: "columns", maxWidth: 300, columns: [{ width: 50, blocks: [{ id: "c", type: "image", width: 150 }] }] }, '<div class="sk-columns sk-center" style="width:50.00%"><div style="flex:50"><div class="sk sk-picture" style="width:100.00%;aspect-ratio:150/75"></div></div></div>'],
    ["a section narrower than the email is the row of its blocks", { type: "section", maxWidth: 300, blocks: [{ id: "c", type: "image", width: 150, height: 150 }] }, '<div class="sk-section"><div class="sk sk-picture" style="width:50.00%;aspect-ratio:150/150"></div></div>'],
  ];
  for (const [name, block, expected] of cases) assert.equal(shapeOf(block), expected, name);
  assert.equal(skeletonHTML([]), `${SKELETON_OPEN}</div>`);
});

test("the skeleton uses only numbers, however odd a block's fields are", () => {
  const out = skeletonHTML([
    { id: "a", type: "image", width: "600;background:url(//outside.test/a.png)", height: Number.POSITIVE_INFINITY },
    { id: "b", type: "heading", size: 1e9 },
    { id: "c", type: "spacer", height: -40 },
    { id: "d", type: "button", width: Number.NaN },
    { id: "e", type: "columns", columns: [{ width: "50;color:red", blocks: [] }, { width: 1e12 }, "nope", null] },
    { id: "f", type: "text", text: 42 },
    { id: "g", type: "image", width: 1e9, height: 1e9 },
    { id: "h", type: "marquee", text: "Not a block" },
    { id: "i", type: "section", blocks: "not a list" },
  ] as unknown as EmailBlock[]);
  assert.deepEqual(styles(out), [
    "width:100.00%;aspect-ratio:600/300", "height:40px", "height:4px", "width:50.00%", "flex:50", "flex:100", "width:60%", "width:100.00%;aspect-ratio:600/2000",
  ]);
  assert.ok(styles(out).every(style => /^[a-z-]+:[0-9./]+(?:%|px)?(?:;[a-z-]+:[0-9./]+(?:%|px)?)*$/.test(style)), "only numbers, units and ratios");
  assert.doesNotMatch(out, /outside|marquee|Not a block|color|nope/);
  // Nesting deeper than any email goes is cut off rather than followed.
  let deep: Record<string, unknown> = { id: "leaf", type: "divider" };
  for (let level = 0; level < 5000; level++) deep = { id: `s${level}`, type: "section", blocks: [deep] };
  assert.ok(skeletonHTML([deep as EmailBlock]).length < 2000);
});

test("fill replaces placeholders and leaves unknown ones visible", () => {
  assert.equal(fill("Published to {organization}", { organization: "Acme Co" }), "Published to Acme Co");
  assert.equal(fill("{x}", {}), "{x}");
  assert.equal(fill("Fix {count} issues to publish", { count: 3 }), "Fix 3 issues to publish");
  assert.equal(fill("{name} and {name}, not {other}", { name: "A" }), "A and A, not {other}");
  assert.equal(fill("{a} {b}", { a: "{b}", b: "x" }), "{b} x", "a value is never read as another placeholder");
  assert.equal(fill("{a}", { a: "$& $1 $$" }), "$& $1 $$", "a value is literal text");
  assert.equal(fill("{constructor} {toString}", {}), "{constructor} {toString}", "only the caller's own values count");
});

test("the email copy has no empty strings, only the placeholders the pages fill, and typographic apostrophes", () => {
  const strings: string[] = [];
  const collect = (value: unknown, path: string): void => {
    if (typeof value === "string") strings.push(value);
    else if (typeof value === "object" && value !== null) for (const [key, item] of Object.entries(value)) collect(item, `${path}.${key}`);
    else assert.fail(`${path} is not a string`);
  };
  collect(EMAIL_COPY, "EMAIL_COPY");
  assert.ok(strings.length >= 150, `${strings.length} strings`);
  assert.ok(strings.every(text => text.trim() !== ""));
  assert.ok(strings.every(text => !text.includes("'")), "apostrophes are typographic");
  assert.ok(strings.every(text => !/\bbytes?\b/i.test(text)), "limits counted in bytes are the code's; a person reads only that something is too long");
  assert.deepEqual([...new Set(strings.flatMap(text => [...text.matchAll(/\{([^{}]*)\}/g)].map(match => match[1])))].sort(), ["count", "domain", "name", "organization"]);
  assert.equal(fill(EMAIL_COPY.editor.published, { organization: "Acme Co" }), "Published to Acme Co");
  assert.equal(fill(EMAIL_COPY.editor.issuesMany, { count: 3 }), "Fix 3 issues to publish");
  assert.equal(fill(EMAIL_COPY.newEmail.notConnected, { domain: "acme.example" }), "Not connected to acme.example");
  assert.equal(fill(EMAIL_COPY.newEmail.removeReference, { name: "brief.md" }), "Remove brief.md");
  assert.equal(EMAIL_COPY.newEmail.failed, "Couldn’t write the email");
});

test("a page shell carries the strict policy, the nonce and nothing the caller did not pass", () => {
  assert.equal(pageCSP("abc123"), "default-src 'none'; script-src 'nonce-abc123'; style-src 'unsafe-inline'; img-src data:; frame-src 'self'; form-action 'none'; base-uri 'none'");
  // A file name can be anything: this one closes the title element and starts an image.
  const title = '</title><img src=x onerror=alert(1)> & "2026".email.json';
  const html = pageShell({ title, nonce: "abc123", script: "https://file+.vscode-resource.test/out/emailView.js?a=1&b=2", styles: BASE_STYLES + "#x{color:red}", body: '<main id="x">Hi</main>', bodyClass: 'wide" onload="alert(1)' });
  const doc = parse(html);
  assert.ok(html.startsWith("<!doctype html>"));
  assert.equal(doc.title, title, "the title is text, not markup");
  assert.equal(doc.querySelectorAll("img").length, 0);
  assert.equal(doc.querySelector('meta[http-equiv="Content-Security-Policy"]')?.getAttribute("content"), pageCSP("abc123"));
  assert.equal(doc.querySelectorAll("script").length, 1);
  assert.equal(doc.querySelector("script")?.getAttribute("nonce"), "abc123");
  assert.equal(doc.querySelector("script")?.getAttribute("src"), "https://file+.vscode-resource.test/out/emailView.js?a=1&b=2");
  assert.equal(doc.body.getAttribute("class"), 'wide" onload="alert(1)', "the class is one attribute value");
  assert.equal(doc.body.hasAttribute("onload"), false);
  assert.ok(doc.querySelector("style")?.textContent?.endsWith("#x{color:red}"));
  assert.equal(doc.querySelectorAll("style").length, 1);
  assert.equal(doc.querySelector("body > main#x")?.textContent, "Hi");
  assert.equal(doc.querySelector("body")?.lastElementChild?.localName, "script", "the page's script runs after its markup");
  assert.equal(parse(pageShell({ title: "T", nonce: "n", script: "s.js", styles: "", body: "" })).body.hasAttribute("class"), false);
});

test("the base styles are whole CSS that covers the shared controls and the skeleton", () => {
  const css = BASE_STYLES;
  assert.equal(css.split("{").length, css.split("}").length, "braces balance");
  const classes = new Set([...css.matchAll(/\.([a-z][a-z0-9-]*)/g)].map(match => match[1]));
  for (const name of ["primary", "secondary", "text-act", "icon-button", "views", "bar", "bar-state", "bar-acts", "busy", "notice", "subhead", "switch", "sr-only", "loading"]) {
    assert.ok(classes.has(name), `.${name} is styled`);
  }
  // Every class the skeleton can emit has a rule.
  const emitted = skeletonHTML([
    { id: "a", type: "image" }, { id: "b", type: "heading" }, { id: "c", type: "text" }, { id: "d", type: "highlight" }, { id: "e", type: "button" },
    { id: "f", type: "divider" }, { id: "g", type: "spacer" }, { id: "h", type: "section" }, { id: "i", type: "columns" }, { id: "j", type: "social" },
    { id: "k", type: "footer" }, { id: "l", type: "html" },
  ] as EmailBlock[]);
  const used = [...emitted.matchAll(/class="([^"]*)"/g)].flatMap(match => (match[1] as string).split(" "));
  assert.ok(used.length > 10);
  for (const name of used) assert.ok(classes.has(name), `.${name} is styled`);
  // The foundations the brief names.
  assert.match(css, /min-height:34px/);
  assert.match(css, /border-radius:6px/);
  assert.match(css, /outline:2px solid var\(--vscode-focusBorder\);outline-offset:2px/);
  assert.match(css, /color-mix\(in srgb,var\(--vscode-editor-foreground\) 10%,var\(--vscode-editor-background\)\)/);
  assert.match(css, /2\.4s/);
  assert.match(css, /prefers-reduced-motion:reduce/);
  // Only theme variables; every animation names a keyframes rule that exists.
  assert.doesNotMatch(css, /--memql-/);
  const keyframes = new Set([...css.matchAll(/@keyframes ([a-z-]+)/g)].map(match => match[1]));
  const animations = [...css.matchAll(/animation:([a-z-]+) /g)].map(match => match[1] as string);
  assert.ok(animations.length >= 2);
  for (const name of animations) assert.ok(keyframes.has(name), `@keyframes ${name} exists`);
});

test("the icons are the sixteen pixel line icons both pages share", () => {
  assert.deepEqual(Object.keys(ICONS).sort(), ["attach", "close", "design", "desktop", "down", "mic", "mobile", "picture", "plain", "plus", "source", "trash", "up"]);
  for (const [name, markup] of Object.entries(ICONS)) {
    const body = parse(markup).body;
    const svg = body.firstElementChild;
    assert.equal(body.children.length, 1, name);
    assert.equal(svg?.localName, "svg", name);
    assert.equal(svg?.getAttribute("width"), "16", name);
    assert.equal(svg?.getAttribute("height"), "16", name);
    assert.equal(svg?.getAttribute("stroke"), "currentColor", name);
    assert.equal(svg?.getAttribute("fill"), "none", name);
    assert.equal(svg?.getAttribute("aria-hidden"), "true", name);
    assert.ok((svg?.querySelectorAll("path,rect,circle").length ?? 0) > 0, `${name} draws something`);
  }
});
