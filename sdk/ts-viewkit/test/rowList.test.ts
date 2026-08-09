// Row-list rendering. The contract that matters: NO concept-specific code,
// ever. A concept declaring @displayCard gets its slots honored; one that
// declares nothing still renders usefully the day it is declared.

import test from "node:test";
import assert from "node:assert/strict";

import { renderRowList, rowDisplayId } from "../src/rowList.js";
import { renderToHtml } from "../src/vnode.js";

const AGENT = {
  id: "v1:agents:agent",
  entity: "agent",
  displayCard: { primary: "name", secondary: "role", status: "active" },
};

const BARE = { id: "v1:cluster:node", entity: "node" };

test("renders one element per row", () => {
  const html = renderToHtml(
    renderRowList(
      [
        { id: "a1", name: "Sofia", role: "hr" },
        { id: "a2", name: "Faye", role: "eng" },
      ],
      AGENT,
    ),
  );
  assert.equal(html.match(/data-row-id=/g)?.length, 2);
});

test("carries the row id as a data attribute for host delegation", () => {
  const html = renderToHtml(renderRowList([{ id: "a1", name: "Sofia" }], AGENT));
  assert.match(html, /data-row-id="a1"/);
});

test("uses the display card primary slot as the row label", () => {
  const html = renderToHtml(renderRowList([{ id: "a1", name: "Sofia" }], AGENT));
  assert.match(html, />Sofia</);
});

test("renders secondary and tertiary slots when present", () => {
  const html = renderToHtml(
    renderRowList([{ id: "a1", name: "Sofia", role: "hr" }], AGENT),
  );
  assert.match(html, /class="vk-row-secondary">hr</);
});

test("omits a slot the row has no value for", () => {
  const html = renderToHtml(renderRowList([{ id: "a1", name: "Sofia" }], AGENT));
  assert.doesNotMatch(html, /vk-row-secondary/);
});

test("renders a status badge from the status slot", () => {
  const html = renderToHtml(
    renderRowList([{ id: "a1", name: "Sofia", active: true }], AGENT),
  );
  // data-status keeps the raw value for host styling; the badge PROSE comes
  // from the slot's field name, never the literal "true" (memql#3303 -- the
  // whole mapping is pinned in displayCard.test.ts).
  assert.match(html, /class="vk-row-status" data-status="true">active</);
});

test("falls back to the row id when the concept declares no display card", () => {
  const html = renderToHtml(renderRowList([{ id: "bff-local" }], BARE));
  assert.match(html, />bff-local</);
});

test("falls back to the row id when the primary slot is absent from the row", () => {
  const html = renderToHtml(renderRowList([{ id: "a1", role: "hr" }], AGENT));
  assert.match(html, />a1</);
});

test("marks the selected row", () => {
  const html = renderToHtml(
    renderRowList([{ id: "a1" }, { id: "a2" }], BARE, "a2"),
  );
  assert.match(html, /data-row-id="a2" data-selected="true"/);
});

test("renders an empty-state element for zero rows", () => {
  const html = renderToHtml(renderRowList([], AGENT));
  assert.match(html, /vk-empty/);
  assert.match(html, /No rows/);
});

test("escapes row values", () => {
  const html = renderToHtml(
    renderRowList([{ id: "a1", name: "<img src=x onerror=evil()>" }], AGENT),
  );
  assert.doesNotMatch(html, /<img/);
  assert.match(html, /&lt;img/);
});

test("rowDisplayId prefers id and tolerates a missing one", () => {
  assert.equal(rowDisplayId({ id: "a1" }), "a1");
  assert.equal(rowDisplayId({}), "");
});

// A non-string `id` is reachable, not theoretical: RowLike is
// Record<string, unknown>, rowProjection merges payload fields into the same
// flat object the row intrinsics live in, and a concept is free to carry a
// numeric or object `id` in its payload. rowDisplayId must yield "" rather
// than a stringified value, because the result becomes `data-row-id` and is
// handed straight back to getRowByConceptAndId -- a "3" that was never a row
// id resolves the wrong row or none at all, silently. Empty is the honest
// answer, and renderRowList's primary slot falls back to the display card.
test("rowDisplayId rejects a non-string id rather than stringifying it", () => {
  assert.equal(rowDisplayId({ id: 3 }), "", "a numeric id must not become \"3\"");
  assert.equal(rowDisplayId({ id: true }), "");
  assert.equal(rowDisplayId({ id: null }), "");
  assert.equal(rowDisplayId({ id: undefined }), "");
  assert.equal(rowDisplayId({ id: { nested: "a1" } }), "");
  assert.equal(rowDisplayId({ id: ["a1"] }), "");
});

test("a row with a non-string id renders with an empty data-row-id, not a coerced one", () => {
  const html = renderToHtml(renderRowList([{ id: 3, name: "Sofia" }], AGENT));
  assert.match(html, /data-row-id=""/);
  assert.doesNotMatch(html, /data-row-id="3"/);
  assert.match(html, /vk-row-primary">Sofia</, "the display card still labels the row");
});
