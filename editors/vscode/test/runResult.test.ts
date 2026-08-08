// Result projection, and the honesty of the banner.
//
// The banner is the part that matters. A `tool` is a declaration bound to a
// Go-backed handler; it cannot be session-defined, so Run necessarily invokes
// the DEPLOYED definition -- from the SAME button that runs a query straight
// out of the buffer. Without an explicit statement on the result, a developer
// has every reason to read a tool result as their edits' output, and edits
// they are staring at had no effect on it.
//
// Stating the good case too is what keeps the bad one legible: a banner that
// only ever appears on tools is one the reader learns to skip.

import test from "node:test";
import assert from "node:assert/strict";

import type { Row } from "@znasllc-io/memql-sdk-core/client";
import type { ConceptLike } from "@znasllc-io/memql-view-kit";

import {
  BUFFER_RESULT_BANNER,
  REUSED_INJECTION_BANNER,
  TOOL_RESULT_BANNER,
  UNTYPED_GROUP_ID,
  groupRowsByConcept,
  resultBannerFor,
} from "../src/state/runResult.js";

const CONCEPTS = new Map<string, ConceptLike>([
  [
    "v1:cognition:space",
    { id: "v1:cognition:space", entity: "space", displayCard: { primary: "name" } },
  ],
]);

function row(overrides: Row = {}): Row {
  return { id: "a", concept: "v1:cognition:space", ...overrides };
}

// -----------------------------------------------------------------------------
// Grouping
// -----------------------------------------------------------------------------

test("groupRowsByConcept -- resolves the descriptor so @displayCard applies", () => {
  const groups = groupRowsByConcept([row()], CONCEPTS);
  assert.equal(groups.length, 1);
  assert.deepEqual(groups[0]?.concept.displayCard, { primary: "name" });
});

test("groupRowsByConcept -- a result spanning two concepts becomes two groups", () => {
  // A logic construct can return rows from more than one query, and each set
  // has to render against its OWN display card.
  const groups = groupRowsByConcept(
    [row(), row({ id: "b", concept: "v1:agents:agent" }), row({ id: "c" })],
    CONCEPTS,
  );
  assert.deepEqual(groups.map((g) => g.concept.id), ["v1:cognition:space", "v1:agents:agent"]);
  assert.equal(groups[0]?.rows.length, 2);
  assert.equal(groups[1]?.rows.length, 1);
});

test("groupRowsByConcept -- an UNKNOWN concept degrades to a card-less descriptor", () => {
  // Not an error: a concept can be registered on the cluster and absent from
  // a cached list, and view-kit already falls back to the row id. This is what
  // lets a concept declared five minutes ago render with no client change.
  const groups = groupRowsByConcept([row({ concept: "v1:brand:new" })], CONCEPTS);
  assert.equal(groups[0]?.concept.id, "v1:brand:new");
  assert.equal(groups[0]?.concept.displayCard, undefined);
});

test("groupRowsByConcept -- rows with no concept intrinsic land in the untyped bucket", () => {
  // A logic construct returning a computed summary or a count is still worth
  // showing; it simply has no display card and no Concepts surface to link to.
  const groups = groupRowsByConcept([{ total: 3 }], CONCEPTS);
  assert.equal(groups[0]?.concept.id, UNTYPED_GROUP_ID);
  // `entity` is what view-kit puts in its empty-state text, so it has to read
  // as a phrase rather than as an identifier.
  assert.equal(groups[0]?.concept.entity, "result");
});

test("groupRowsByConcept -- an empty result is an empty group list", () => {
  assert.deepEqual(groupRowsByConcept([], CONCEPTS), []);
});

// -----------------------------------------------------------------------------
// Banners
// -----------------------------------------------------------------------------

test("resultBannerFor -- a tool result says the DEPLOYED definition ran", () => {
  const banner = resultBannerFor({ ranDeployedDefinition: true, injected: false });
  assert.equal(banner, TOOL_RESULT_BANNER);
  assert.match(banner, /DEPLOYED/);
  assert.match(banner, /not this buffer/);
});

test("resultBannerFor -- a freshly injected run says the buffer ran and nothing was saved", () => {
  const banner = resultBannerFor({ ranDeployedDefinition: false, injected: true });
  assert.equal(banner, BUFFER_RESULT_BANNER);
  assert.match(banner, /Nothing was saved or deployed/);
});

test("resultBannerFor -- a reused injection still says the buffer ran", () => {
  // The distinction matters: "not re-injected" must not read as "not your
  // code". The bundle was unchanged, so what ran is still the buffer.
  const banner = resultBannerFor({ ranDeployedDefinition: false, injected: false });
  assert.equal(banner, REUSED_INJECTION_BANNER);
  assert.match(banner, /as previously session-defined/);
});

test("the three banners are distinct", () => {
  const set = new Set([TOOL_RESULT_BANNER, BUFFER_RESULT_BANNER, REUSED_INJECTION_BANNER]);
  assert.equal(set.size, 3);
});
