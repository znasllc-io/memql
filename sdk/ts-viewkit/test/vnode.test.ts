// Serializer tests. view-kit emits HTML into a VS Code webview and, later,
// the portal -- every value that reaches renderToHtml is untrusted row data,
// so escaping is a correctness requirement, not a nicety.

import test from "node:test";
import assert from "node:assert/strict";

import { h, text, renderToHtml, escapeHtml } from "../src/vnode.js";

test("renderToHtml serializes a bare element", () => {
  assert.equal(renderToHtml(h("div", {})), "<div></div>");
});

test("renderToHtml serializes attributes", () => {
  assert.equal(
    renderToHtml(h("div", { class: "row", "data-row-id": "abc" })),
    '<div class="row" data-row-id="abc"></div>',
  );
});

test("renderToHtml serializes nested children", () => {
  const node = h("ul", {}, [h("li", {}, [text("one")]), h("li", {}, [text("two")])]);
  assert.equal(renderToHtml(node), "<ul><li>one</li><li>two</li></ul>");
});

test("renderToHtml escapes text content", () => {
  assert.equal(
    renderToHtml(h("p", {}, [text('<script>alert("x")</script>')])),
    "<p>&lt;script&gt;alert(&quot;x&quot;)&lt;/script&gt;</p>",
  );
});

test("renderToHtml escapes attribute values", () => {
  assert.equal(
    renderToHtml(h("div", { title: 'a" onmouseover="evil()' })),
    '<div title="a&quot; onmouseover=&quot;evil()"></div>',
  );
});

test("renderToHtml emits void elements without a closing tag", () => {
  assert.equal(renderToHtml(h("br", {})), "<br>");
  assert.equal(renderToHtml(h("hr", { class: "sep" })), '<hr class="sep">');
});

test("escapeHtml handles the five significant characters", () => {
  assert.equal(escapeHtml(`&<>"'`), "&amp;&lt;&gt;&quot;&#39;");
});

test("escapeHtml leaves ordinary text untouched", () => {
  assert.equal(escapeHtml("v1:agents:agent"), "v1:agents:agent");
});
