import { test } from "node:test";
import assert from "node:assert/strict";
import { JSDOM } from "jsdom";
import { safeEmailHTML } from "../src/templatePreview.js";

test("email preview retains layout but removes executable content, links, and remote loads", () => {
  const window = new JSDOM("").window;
  Object.assign(globalThis, { DOMParser: window.DOMParser });
  const input = `<html><head><meta http-equiv="refresh" content="0;url=https://outside.test"><style>table{border-collapse:collapse}</style></head><body>
    <table cellpadding="12"><tr><td style="color:#145c44" onclick="alert(1)">Welcome</td></tr></table>
    <script>alert(1)</script><form action="https://outside.test"><input name="secret"></form><iframe src="https://outside.test"></iframe>
    <a href="https://outside.test" ping="https://tracker.test">Read more</a><img src="https://private.test/logo.png" alt="Client logo"><svg onload="alert(1)"></svg>
    </body></html>`;
  const preview = safeEmailHTML(input);
  const doc = new window.DOMParser().parseFromString(preview, "text/html");
  assert.equal(doc.querySelector("td")?.textContent, "Welcome");
  assert.equal(doc.querySelector("table")?.getAttribute("cellpadding"), "12");
  assert.equal(doc.querySelector("td")?.getAttribute("style"), "color:#145c44");
  assert.equal(doc.querySelectorAll("script,form,iframe,svg,input,[href],[src],[onclick],[ping]").length, 0);
  assert.match(doc.body.textContent || "", /Client logo/);
  assert.equal(doc.querySelector('meta[http-equiv="refresh"]'), null);
  assert.match(doc.querySelector("meta")?.getAttribute("content") || "", /default-src 'none'/);
  assert.match(doc.querySelector("style")?.textContent || "", /border-collapse/);
  window.close();
});
