// The display-card fallback contract (memql#3318) and the boolean-status
// mapping (memql#3303).
//
// These tests are the contract. The prose lives in displayCard.ts and
// docs/public/concepts/display-cards.md; if the two disagree, this file is
// what a consumer's behaviour actually follows, so every clause of the rule
// gets an assertion here rather than only a sentence there.

import test from "node:test";
import assert from "node:assert/strict";

import {
  inferDisplayCard,
  resolveDisplayCard,
  statusFieldLabel,
  statusText,
  statusValue,
  PRIMARY_NAME_FIELDS,
  SECONDARY_NAME_FIELDS,
  STATUS_NAME_FIELDS,
} from "../src/displayCard.js";
import { renderRowList } from "../src/rowList.js";
import { renderToHtml } from "../src/vnode.js";

const BARE = { id: "v1:cluster:node", entity: "node" };

// ---------------------------------------------------------------------------
// Rule 1 -- a declared card is honoured verbatim.
// ---------------------------------------------------------------------------

test("a declared card is used exactly as declared", () => {
  const concept = {
    id: "v1:agents:agent",
    entity: "agent",
    displayCard: { primary: "role" },
  };
  // The row also carries `name`, which inference would have preferred.
  assert.deepEqual(resolveDisplayCard(concept, [{ id: "a1", name: "Sofia", role: "hr" }]), {
    primary: "role",
  });
});

test("inference never fills a slot a declared card omitted", () => {
  const concept = {
    id: "v1:todos:todo",
    entity: "todo",
    displayCard: { primary: "title" },
  };
  const card = resolveDisplayCard(concept, [{ id: "t1", title: "Ship it", done: true }]);
  assert.equal(card.status, undefined);
  assert.equal(card.secondary, undefined);
});

test("a declared-but-omitted status slot renders no badge", () => {
  const html = renderToHtml(
    renderRowList([{ id: "t1", title: "Ship it", done: true }], {
      id: "v1:todos:todo",
      entity: "todo",
      displayCard: { primary: "title" },
    }),
  );
  assert.doesNotMatch(html, /vk-row-status/);
});

// ---------------------------------------------------------------------------
// Rule 2 -- an undeclared concept is rendered through the inferred card.
// ---------------------------------------------------------------------------

test("primary is inferred from a name field", () => {
  assert.equal(inferDisplayCard([{ id: "n1", name: "bff" }]).primary, "name");
});

test("primary inference honours the documented candidate order", () => {
  const row: Record<string, unknown> = { id: "x" };
  for (const field of PRIMARY_NAME_FIELDS) row[field] = `value-${field}`;
  assert.equal(inferDisplayCard([row]).primary, PRIMARY_NAME_FIELDS[0]);
});

test("every documented primary candidate is actually inferable on its own", () => {
  for (const field of PRIMARY_NAME_FIELDS) {
    assert.equal(
      inferDisplayCard([{ id: "x", [field]: "v" }]).primary,
      field,
      `${field} is advertised in PRIMARY_NAME_FIELDS but never inferred`,
    );
  }
});

test("secondary and status are inferred from their own candidate lists", () => {
  for (const field of SECONDARY_NAME_FIELDS) {
    assert.equal(inferDisplayCard([{ id: "x", [field]: "v" }]).secondary, field);
  }
  for (const field of STATUS_NAME_FIELDS) {
    assert.equal(inferDisplayCard([{ id: "x", [field]: "v" }]).status, field);
  }
});

test("tertiary is never inferred", () => {
  const row: Record<string, unknown> = { id: "x", createdAt: "2026-01-01", createdBy: "u1" };
  for (const field of [...PRIMARY_NAME_FIELDS, ...SECONDARY_NAME_FIELDS, ...STATUS_NAME_FIELDS]) {
    row[field] = "v";
  }
  assert.equal(inferDisplayCard([row]).tertiary, undefined);
});

test("a row with no candidate field infers nothing and falls back to the id", () => {
  assert.deepEqual(inferDisplayCard([{ id: "bff-local", nodeType: "bff" }]), {});
  const html = renderToHtml(renderRowList([{ id: "bff-local", nodeType: "bff" }], BARE));
  assert.match(html, />bff-local</);
});

test("inference ignores object and array values", () => {
  // The loader rejects a DECLARED slot naming an object; the inferred path
  // must not become a way around that.
  const card = inferDisplayCard([
    { id: "x", name: { first: "Sofia" }, description: ["a", "b"], status: {} },
  ]);
  assert.deepEqual(card, {});
});

test("inference ignores an empty-string value", () => {
  assert.equal(inferDisplayCard([{ id: "x", name: "", title: "Real" }]).primary, "title");
});

test("a false boolean still counts as a present status value", () => {
  // `active: false` is information, not absence -- skipping it would hide
  // exactly the rows an operator is looking for.
  assert.equal(inferDisplayCard([{ id: "x", active: false }]).status, "active");
});

// ---------------------------------------------------------------------------
// Rule 3 -- inference is resolved once per row set.
// ---------------------------------------------------------------------------

test("one field is chosen for the whole set, not per row", () => {
  const rows = [
    { id: "r1", title: "First" },
    { id: "r2", name: "Second" },
  ];
  // `name` outranks `title`, and the row that lacks it falls back to its id
  // rather than labelling itself off a different field.
  assert.equal(inferDisplayCard(rows).primary, "name");
  const html = renderToHtml(renderRowList(rows, BARE));
  assert.match(html, /class="vk-row-primary">r1</);
  assert.match(html, /class="vk-row-primary">Second</);
  assert.doesNotMatch(html, />First</);
});

// ---------------------------------------------------------------------------
// Rule 4 -- the candidate lists are field names, nothing else.
// ---------------------------------------------------------------------------

test("the candidate lists contain no concept-shaped names", () => {
  // A concept id or a namespaced entity name appearing here would be the
  // concept-specific renderer code this package exists to not have.
  for (const field of [...PRIMARY_NAME_FIELDS, ...SECONDARY_NAME_FIELDS, ...STATUS_NAME_FIELDS]) {
    assert.match(
      field,
      /^[a-z][A-Za-z0-9]*$/,
      `${field} is not a plain field name -- candidate lists are field names, not concepts`,
    );
    assert.doesNotMatch(field, /^v1|:/);
  }
});

// ---------------------------------------------------------------------------
// memql#3303 -- a boolean status slot is not the word "true".
// ---------------------------------------------------------------------------

test("a true boolean status renders the field name", () => {
  assert.equal(statusText("active", true), "active");
});

test("a false boolean status renders the negated field name", () => {
  assert.equal(statusText("active", false), "not active");
});

test("an is/has predicate prefix is dropped from the badge label", () => {
  assert.equal(statusFieldLabel("isError"), "error");
  assert.equal(statusFieldLabel("hasAvatar"), "avatar");
  assert.equal(statusText("isError", true), "error");
  assert.equal(statusText("isError", false), "not error");
});

test("a field name that merely starts with the letters is/has is left alone", () => {
  assert.equal(statusFieldLabel("issued"), "issued");
  assert.equal(statusFieldLabel("hashed"), "hashed");
  assert.equal(statusFieldLabel("is"), "is");
});

test("a non-boolean status passes through untouched", () => {
  assert.equal(statusText("status", "in_progress"), "in_progress");
  assert.equal(statusText("attempts", 3), "3");
});

test("data-status keeps the raw value so host styling is unaffected", () => {
  assert.equal(statusValue(true), "true");
  assert.equal(statusValue(false), "false");
  assert.equal(statusValue("failed"), "failed");
  const html = renderToHtml(
    renderRowList([{ id: "a1", name: "Sofia", active: false }], {
      id: "v1:agents:agent",
      entity: "agent",
      displayCard: { primary: "name", status: "active" },
    }),
  );
  assert.match(html, /class="vk-row-status" data-status="false">not active</);
});

test("the boolean mapping is value-agnostic -- no status vocabulary is hard-coded", () => {
  // A concept nobody has ever seen gets the same treatment as `active`.
  assert.equal(statusText("quiesced", true), "quiesced");
  assert.equal(statusText("quiesced", false), "not quiesced");
});
