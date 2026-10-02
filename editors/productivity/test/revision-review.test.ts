import { test } from "node:test";
import assert from "node:assert/strict";
import { assertRevisionBase } from "../src/revisionReview.js";
import type { OpenDocument } from "../src/documents.js";

test("applying a reviewed draft requires the exact source bytes and saved revision", () => {
  const base = { resource: { id: "one" }, version: 3, revision: "file:3" } as OpenDocument;
  const proposal = { artifactId: "one", version: 3, revision: "file:3", content: "Original" };
  assert.doesNotThrow(() => assertRevisionBase(base, "Original", proposal));
  for (const change of [{ artifactId: "two" }, { version: 4 }, { revision: "file:4" }, { content: "Changed" }]) {
    assert.throws(() => assertRevisionBase(base, "Original", { ...proposal, ...change }), /document changed/);
  }
  assert.throws(() => assertRevisionBase(base, "Local changes", proposal), /document changed/);
});
