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

// The cycle guard tracks the ANCESTOR chain, not "every object ever seen".
// The distinction is the difference between a correct guard and a wrong one:
// renderValue adds an object to `seen` before descending into it and DELETES
// it again on the way out, so an object that merely appears twice as a
// SIBLING -- the same nested record referenced from two fields, which JSON.parse
// on a wire payload does not produce but an in-process caller readily does --
// renders in full both times. An implementation that only ever added to `seen`
// would pass the cyclic test above while silently rendering "[circular]" for
// the second, perfectly finite occurrence.
test("a non-cyclic object reused by two siblings renders in full both times", () => {
  const shared = { host: "cockpit.local.znas.io", port: 443 };
  const html = renderToHtml(renderDetail({ primary: shared, mirror: shared }));

  assert.doesNotMatch(
    html,
    /vk-cycle/,
    "a shared sibling is finite, not circular -- marking it would hide real data",
  );
  assert.equal(
    html.match(/vk-value">cockpit\.local\.znas\.io</g)?.length,
    2,
    "both occurrences must render their contents",
  );
});

test("a shared sibling deeper in the tree is still not mistaken for a cycle", () => {
  const shared = { id: "a1" };
  const html = renderToHtml(
    renderDetail({ payload: { left: shared, right: { inner: shared } } }),
  );

  assert.doesNotMatch(html, /vk-cycle/);
  assert.equal(html.match(/vk-value">a1</g)?.length, 2);
});

test("a cycle reached through a shared sibling is still caught", () => {
  // The two properties must not cancel each other out: the ancestor chain
  // still has to terminate a genuine loop that is entered via a node the
  // walk has already visited and left.
  const loop: Record<string, unknown> = { name: "root" };
  loop["self"] = loop;
  const html = renderToHtml(renderDetail({ first: loop, second: loop }));

  assert.match(html, /vk-value vk-cycle">\[circular\]</);
  assert.equal(
    html.match(/vk-value">root</g)?.length,
    2,
    "the shared sibling still renders twice; only the self-reference inside it is cut",
  );
});
