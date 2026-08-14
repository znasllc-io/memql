// The value viewer.
//
// Four groups of assertions carry this file, and each pins a property whose
// failure mode is worse than it sounds:
//
//   THE CYCLE GUARD, ported whole from the renderer this replaces. Its
//   ancestor-chain semantics are the subtle part: an object that appears twice
//   as a SIBLING is finite and must render in full both times, while a genuine
//   loop must still terminate. An implementation that only ever adds to `seen`
//   passes the cyclic case and silently prints "[circular]" over real data.
//
//   THE BOUNDS. Paging, string truncation and the node budget are what make a
//   4MB payload safe to hand a webview, and each must SAY it truncated --
//   silent truncation is indistinguishable from a value that ended there.
//
//   ESCAPING, which is not optional: row data is untrusted.
//
//   THE COLLAPSED SPELLING. `open` is emitted only when open, and never as ""
//   or "false", because both of those render every node the wrong way round in
//   a React host.

import test from "node:test";
import assert from "node:assert/strict";

import {
  DEFAULT_MAX_STRING_LENGTH,
  VALUE_VIEW_ATTRS,
  joinPath,
  renderValueView,
  valueTypeName,
} from "../src/valueView.js";
import { renderToHtml } from "../src/vnode.js";

function html(value: unknown, options?: Parameters<typeof renderValueView>[1]): string {
  return renderToHtml(renderValueView(value, options));
}

// -----------------------------------------------------------------------------
// the shapes a payload can hold
// -----------------------------------------------------------------------------

test("renders a scalar field as key, type and value", () => {
  const out = html({ name: "Sofia" });
  assert.match(out, /vk-vv-key">name</);
  assert.match(out, /vk-vv-type-string">string</);
  assert.match(out, /vk-vv-value">Sofia</);
});

test('"42" and 42 are distinguishable, which is the whole point of the badge', () => {
  const asString = html({ v: "42" });
  const asNumber = html({ v: 42 });
  assert.match(asString, /vk-vv-type-string/);
  assert.match(asNumber, /vk-vv-type-number/);
  // The rendered TEXT is identical -- the badge is the only thing that differs.
  assert.match(asString, /vk-vv-value">42</);
  assert.match(asNumber, /vk-vv-value">42</);
});

test("renders null as an explicit marker rather than blank", () => {
  assert.match(html({ deletedAt: null }), /vk-vv-value vk-vv-null">null</);
  assert.match(html({ missing: undefined }), /vk-vv-value vk-vv-null">null</);
});

test("walks a nested object recursively", () => {
  const out = html({ provenance: { source: "seed", actor: "system" } });
  assert.match(out, /vk-vv-key">provenance</);
  assert.match(out, /vk-vv-key">source</);
  assert.match(out, /vk-vv-value">seed</);
});

test("renders arrays with indices", () => {
  const out = html({ tags: ["a", "b"] });
  assert.match(out, /vk-vv-key">\[0\]</);
  assert.match(out, /vk-vv-value">a</);
  assert.match(out, /vk-vv-key">\[1\]</);
});

test("renders empty collections as explicit markers", () => {
  assert.match(html({ metadata: {} }), /vk-vv-value vk-vv-empty">\{\}</);
  assert.match(html({ tags: [] }), /vk-vv-value vk-vv-empty">\[\]</);
});

test("an empty root says so rather than rendering nothing", () => {
  assert.match(html({}), /vk-vv-nothing/);
  assert.match(html([]), /vk-vv-nothing/);
});

test("a bare scalar at the root renders", () => {
  assert.match(html("just a string"), /vk-vv-value">just a string</);
  assert.match(html(42), /vk-vv-type-number/);
});

test("a function names its type instead of crashing", () => {
  assert.match(html({ fn: () => undefined }), /vk-vv-type-unknown/);
});

test("valueTypeName covers every branch", () => {
  assert.equal(valueTypeName("a"), "string");
  assert.equal(valueTypeName(1), "number");
  assert.equal(valueTypeName(1n), "number");
  assert.equal(valueTypeName(true), "boolean");
  assert.equal(valueTypeName(null), "null");
  assert.equal(valueTypeName(undefined), "null");
  assert.equal(valueTypeName({}), "object");
  assert.equal(valueTypeName([]), "array");
  assert.equal(valueTypeName(Symbol("s")), "unknown");
});

// -----------------------------------------------------------------------------
// collapse
// -----------------------------------------------------------------------------

test("a collapsed node carries a count of what is behind it", () => {
  const out = html({ payload: { deep: { a: 1, b: 2, c: 3 } } }, { expandDepth: 1 });
  assert.match(out, /vk-vv-count">\{\.\.\.\} 3 keys</);
});

test("counts are singular when there is one of something", () => {
  assert.match(html({ o: { a: 1 } }), /\{\.\.\.\} 1 key</);
  assert.match(html({ a: [1] }), /\[\.\.\.\] 1 item</);
});

test("counts group thousands, so a big array reads at a glance", () => {
  const big = Array.from({ length: 1284 }, (_, i) => i);
  assert.match(html({ items: big }, { pageSize: 1 }), /\[\.\.\.\] 1,284 items</);
});

test("`open` is emitted only when open, and never as an empty or false string", () => {
  const out = html({ shallow: { a: 1 }, deep: { b: { c: { d: 1 } } } }, { expandDepth: 1 });
  // An empty string is falsy in React (every node would collapse); the string
  // "false" is truthy (every node would open). Absence is the only spelling of
  // closed that survives both hosts.
  assert.doesNotMatch(out, /open=""/);
  assert.doesNotMatch(out, /open="false"/);
  assert.match(out, /open="open"/);
  // The depth-1 branch is open, the depth-2 one is not.
  assert.equal(out.match(/open="open"/g)?.length, 2);
});

test("collapse needs no script: it is details and summary", () => {
  const out = html({ payload: { a: 1 } });
  assert.match(out, /<details /);
  assert.match(out, /<summary /);
  // No inline handler may ever appear -- a webview CSP forbids them.
  assert.doesNotMatch(out, /on[a-z]+=/);
});

// -----------------------------------------------------------------------------
// filter
// -----------------------------------------------------------------------------

test("a filter keeps matching keys and drops the rest", () => {
  const out = html({ name: "Sofia", role: "hr", active: true }, { filter: "role" });
  assert.match(out, /vk-vv-key">role</);
  assert.doesNotMatch(out, /vk-vv-key">name</);
  assert.doesNotMatch(out, /vk-vv-key">active</);
});

test("a filter matches values as well as keys", () => {
  const out = html({ name: "Sofia", role: "hr" }, { filter: "sofia" });
  assert.match(out, /vk-vv-key">name</);
  assert.doesNotMatch(out, /vk-vv-key">role</);
});

test("a match deep in the tree is revealed WITH its ancestors", () => {
  const out = html(
    { payload: { lineage: { originatingPlanId: "p1" }, other: "x" }, id: "a1" },
    { filter: "originating" },
  );
  assert.match(out, /vk-vv-key">payload</);
  assert.match(out, /vk-vv-key">lineage</);
  assert.match(out, /vk-vv-key">originatingPlanId</);
  // ...and the branches that hold no match are gone.
  assert.doesNotMatch(out, /vk-vv-key">other</);
  assert.doesNotMatch(out, /vk-vv-key">id</);
});

test("a revealed ancestor is open, because revealing into a closed node reveals nothing", () => {
  const out = html(
    { a: { b: { c: { d: { needle: "x" } } } } },
    { filter: "needle", expandDepth: 0 },
  );
  assert.match(out, /vk-vv-key">needle</);
  // expandDepth 0 would collapse every branch; the filter overrides it, so all
  // four ancestors are open and the match is actually visible.
  assert.equal(out.match(/<details /g)?.length, 4);
  assert.equal(out.match(/open="open"/g)?.length, 4);
});

test("a branch whose own key matches renders its whole subtree", () => {
  // Searching for `lineage` and getting the key with none of its contents
  // would be a worse answer than no filter at all.
  const out = html({ lineage: { planId: "p1", taskId: "t1" }, other: 1 }, { filter: "lineage" });
  assert.match(out, /vk-vv-key">planId</);
  assert.match(out, /vk-vv-key">taskId</);
  assert.doesNotMatch(out, /vk-vv-key">other</);
});

test("a matching node is marked as the match, not merely revealed", () => {
  const out = html({ payload: { lineage: { id: "x" } } }, { filter: "lineage" });
  assert.match(out, /vk-vv-branch vk-vv-match/);
});

test("a filter that matches nothing says so", () => {
  assert.match(html({ a: 1 }, { filter: "zzz" }), /No key or value matches the filter/);
});

test("the filter is case-insensitive and ignores surrounding space", () => {
  assert.match(html({ OriginatingPlanId: 1 }, { filter: "  originating " }), /OriginatingPlanId/);
});

// -----------------------------------------------------------------------------
// the cycle guard -- ported whole, semantics and all
// -----------------------------------------------------------------------------

test("terminates on a self-referential payload instead of recursing forever", () => {
  const cyclic: Record<string, unknown> = { name: "root" };
  cyclic["self"] = cyclic;
  assert.match(html(cyclic), /vk-vv-value vk-vv-cycle">\[circular\]</);
});

test("a non-cyclic object reused by two siblings renders in full both times", () => {
  const shared = { host: "api.local.znas.io", port: 443 };
  const out = html({ primary: shared, mirror: shared });
  assert.doesNotMatch(
    out,
    /vk-vv-cycle/,
    "a shared sibling is finite, not circular -- marking it would hide real data",
  );
  assert.equal(out.match(/vk-vv-value">api\.local\.znas\.io</g)?.length, 2);
});

test("a shared sibling deeper in the tree is still not mistaken for a cycle", () => {
  const shared = { id: "a1" };
  const out = html({ payload: { left: shared, right: { inner: shared } } }, { expandDepth: 9 });
  assert.doesNotMatch(out, /vk-vv-cycle/);
  assert.equal(out.match(/vk-vv-value">a1</g)?.length, 2);
});

test("a cycle reached through a shared sibling is still caught", () => {
  const loop: Record<string, unknown> = { name: "root" };
  loop["self"] = loop;
  const out = html({ first: loop, second: loop });
  assert.match(out, /vk-vv-value vk-vv-cycle">\[circular\]</);
  assert.equal(
    out.match(/vk-vv-value">root</g)?.length,
    2,
    "the shared sibling still renders twice; only the self-reference inside it is cut",
  );
});

// -----------------------------------------------------------------------------
// bounds -- and saying so
// -----------------------------------------------------------------------------

test("a long string truncates and counts what was cut", () => {
  const long = "x".repeat(DEFAULT_MAX_STRING_LENGTH + 4120);
  const out = html({ notes: long });
  assert.match(out, /vk-vv-truncated/);
  assert.match(out, /\.\.\. 4,120 more characters/);
  assert.equal(out.includes("x".repeat(DEFAULT_MAX_STRING_LENGTH + 1)), false);
});

test("a string exactly at the limit is not truncated", () => {
  const exact = "x".repeat(DEFAULT_MAX_STRING_LENGTH);
  assert.doesNotMatch(html({ notes: exact }), /vk-vv-truncated/);
});

test("a long array pages, naming the remainder and where it resumes", () => {
  const items = Array.from({ length: 250 }, (_, i) => `item-${i}`);
  const out = html({ items }, { pageSize: 100 });
  assert.match(out, /\.\.\. 150 more items/);
  assert.match(out, new RegExp(`${VALUE_VIEW_ATTRS.moreFrom}="100"`));
  assert.match(out, new RegExp(`${VALUE_VIEW_ATTRS.more}="items"`));
  assert.equal(out.match(/vk-vv-key">\[\d+\]</g)?.length, 100);
});

test("an object pages too -- a 10,000-key object is the same problem as an array", () => {
  const wide: Record<string, number> = {};
  for (let i = 0; i < 300; i += 1) wide[`k${i}`] = i;
  const out = html({ wide }, { pageSize: 50 });
  assert.match(out, /\.\.\. 250 more keys/);
});

test("the node budget bounds the render and SAYS that it did", () => {
  const items = Array.from({ length: 500 }, (_, i) => ({ i }));
  const out = html({ items }, { nodeBudget: 20, pageSize: 1000 });
  assert.match(out, /vk-vv-budget/);
  assert.match(out, /too large to render in full/);
});

test("a 4MB payload renders promptly and bounded", () => {
  // A measured assertion, not a judgement. The shape is deliberately hostile:
  // 20,000 rows, each an object, each carrying a long string -- more than any
  // real row and exactly the case that used to hang a webview.
  const rows = Array.from({ length: 20_000 }, (_, i) => ({
    id: `row-${i}`,
    note: "y".repeat(200),
  }));
  const payload = { concept: "v1:knowledge:document", payload: { rows } };
  assert.ok(JSON.stringify(payload).length > 4_000_000, "the fixture must actually be 4MB+");

  const started = process.hrtime.bigint();
  const out = html(payload);
  const elapsedMs = Number(process.hrtime.bigint() - started) / 1_000_000;

  assert.ok(elapsedMs < 1_000, `render took ${elapsedMs.toFixed(0)}ms`);
  assert.ok(out.length < 2_000_000, `emitted ${out.length} bytes`);
  assert.match(out, /vk-vv-more|vk-vv-budget/, "and it says it did not show everything");
});

// -----------------------------------------------------------------------------
// escaping
// -----------------------------------------------------------------------------

test("escapes keys and values", () => {
  const out = html({ "<k>": "<v>" });
  assert.doesNotMatch(out, /<k>/);
  assert.match(out, /&lt;k&gt;/);
  assert.match(out, /&lt;v&gt;/);
});

test("escapes a path that reaches an attribute", () => {
  const out = html({ '"onload=x': 1 }, { copy: true });
  assert.doesNotMatch(out, /"onload=x/);
  assert.match(out, /&quot;onload=x/);
});

test("escapes a script tag hiding in a truncated string", () => {
  // MIXED CASE, and the assertion is case-insensitive. A `/<script>/` check is
  // a weaker claim than it looks: HTML tag names are case-insensitive, so a
  // leak spelled `<SCRIPT>` would pass it. What is actually being asserted is
  // that no raw tag from the value survives, whatever its spelling.
  const out = html({ n: `<ScRiPt>alert(1)</ScRiPt>${"z".repeat(600)}` });
  assert.doesNotMatch(out, /<\/?script/i);
  assert.match(out, /&lt;ScRiPt&gt;/);
});

// -----------------------------------------------------------------------------
// paths, breadcrumb and copy
// -----------------------------------------------------------------------------

test("array indices attach to the path rather than being dotted", () => {
  assert.equal(joinPath(["payload", "phases", "[0]", "name"]), "payload.phases[0].name");
  assert.equal(joinPath(["[0]", "a"]), "[0].a");
  assert.equal(joinPath([]), "");
});

test("a caller-supplied path renders as the breadcrumb", () => {
  const out = html({ a: 1 }, { path: ["payload", "lineage"] });
  assert.match(out, /vk-vv-crumb">payload\.lineage</);
});

test("no breadcrumb bar when the value is the whole document", () => {
  assert.doesNotMatch(html({ a: 1 }), /vk-vv-crumb/);
});

test("every node carries its own path, prefixed by the caller's", () => {
  const out = html({ lineage: { planId: "p1" } }, { path: ["payload"] });
  assert.match(out, new RegExp(`${VALUE_VIEW_ATTRS.path}="payload.lineage.planId"`));
});

test("copy is off by default -- a button that does nothing is worse than no button", () => {
  assert.doesNotMatch(html({ a: 1 }), /vk-vv-copy/);
});

test("copy renders value and path on a scalar, JSON and path on a branch", () => {
  const out = html({ payload: { a: 1 } }, { copy: true });
  assert.match(out, new RegExp(`${VALUE_VIEW_ATTRS.copy}="value"`));
  assert.match(out, new RegExp(`${VALUE_VIEW_ATTRS.copy}="json"`));
  assert.match(out, new RegExp(`${VALUE_VIEW_ATTRS.copy}="path"`));
  assert.match(out, /<button /);
  // Still no inline handler: the host reads the attributes off one delegated
  // listener.
  assert.doesNotMatch(out, /on[a-z]+="/);
});
