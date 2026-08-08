// Detail rendering. The cockpit calls this shape "Hybrid C": no
// concept-specific rendering, just a recursive walk of the row's payload,
// provenance and intrinsics. Tests pin the walk's handling of every JSON
// shape a payload can hold.

import test from "node:test";
import assert from "node:assert/strict";

import { renderDetail } from "../src/detail.js";
import { renderToHtml } from "../src/vnode.js";

test("renders a scalar field as a key/value pair", () => {
  const html = renderToHtml(renderDetail({ name: "Sofia" }));
  assert.match(html, /vk-key">name</);
  assert.match(html, /vk-value">Sofia</);
});

test("renders numbers and booleans", () => {
  const html = renderToHtml(renderDetail({ count: 3, active: true }));
  assert.match(html, /vk-value">3</);
  assert.match(html, /vk-value">true</);
});

test("renders null as an explicit marker rather than blank", () => {
  const html = renderToHtml(renderDetail({ deletedAt: null }));
  assert.match(html, /vk-value vk-null">null</);
});

test("walks a nested object recursively", () => {
  const html = renderToHtml(
    renderDetail({ provenance: { source: "seed", actor: "system" } }),
  );
  assert.match(html, /vk-key">provenance</);
  assert.match(html, /vk-key">source</);
  assert.match(html, /vk-value">seed</);
});

test("renders arrays with indices", () => {
  const html = renderToHtml(renderDetail({ tags: ["a", "b"] }));
  assert.match(html, /vk-key">\[0\]</);
  assert.match(html, /vk-value">a</);
  assert.match(html, /vk-key">\[1\]</);
});

test("renders an array of objects", () => {
  const html = renderToHtml(renderDetail({ phases: [{ name: "one" }] }));
  assert.match(html, /vk-key">\[0\]</);
  assert.match(html, /vk-key">name</);
  assert.match(html, /vk-value">one</);
});

test("renders an empty object as an explicit marker", () => {
  const html = renderToHtml(renderDetail({ metadata: {} }));
  assert.match(html, /vk-value vk-empty-value">\{\}</);
});

test("renders an empty array as an explicit marker", () => {
  const html = renderToHtml(renderDetail({ tags: [] }));
  assert.match(html, /vk-value vk-empty-value">\[\]</);
});

test("escapes keys and values", () => {
  const html = renderToHtml(renderDetail({ "<k>": "<v>" }));
  assert.doesNotMatch(html, /<k>/);
  assert.match(html, /&lt;k&gt;/);
  assert.match(html, /&lt;v&gt;/);
});

test("renders an empty row without throwing", () => {
  assert.match(renderToHtml(renderDetail({})), /vk-detail/);
});

test("terminates on a self-referential payload instead of recursing forever", () => {
  const cyclic: Record<string, unknown> = { name: "root" };
  cyclic["self"] = cyclic;
  const html = renderToHtml(renderDetail(cyclic));
  assert.match(html, /vk-value vk-cycle">\[circular\]</);
});
