import { test } from "node:test";
import assert from "node:assert/strict";
import { JSDOM } from "jsdom";
import { safeEmailHTML, sampleFields, sampleTemplate } from "../src/templatePreview.js";

test("personalization samples are literal, HTML escaped, and never change the source", () => {
  const template = {subject:"Hi {{displayName}}",textBody:"{{email}} {{fields.company}} {{unknown}}",
    htmlBody:'<p>{{displayName}} · {{fields.company}}</p><a title="{{fields.company}}">More</a>'};
  assert.deepEqual(sampleFields(template),["displayName","email","fields.company"]);
  const preview = sampleTemplate(template,{displayName:"{{email}}",email:"alex@example.test","fields.company":'<img src=x onerror="bad">'});
  assert.equal(preview.subject,"Hi {{email}}","replacement values cannot recursively name another field");
  assert.match(preview.textBody,/<img src=x onerror="bad">/);
  assert.match(preview.textBody,/\{\{unknown\}\}/);
  assert.ok(!preview.htmlBody.includes("<img"));
  assert.match(preview.htmlBody,/&lt;img src=x onerror=&quot;bad&quot;&gt;/);
  assert.equal(template.subject,"Hi {{displayName}}");
});

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
