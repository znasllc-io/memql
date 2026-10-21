import { describe, expect, it } from "vitest";

import { embeddingModelMissing } from "../../src/apps/setup/InferenceStop";

// The inference verdict needs an embedding model as well as a door (design
// record 2026-10-18-routes). The stop names that one missing thing, and only
// when a door IS open -- a fresh cluster still sees the doors.
describe("embeddingModelMissing", () => {
  const lane = (name: string, complete: boolean) => ({ name, complete });

  it("is true when a door is open and the embeddings lane is not", () => {
    expect(embeddingModelMissing([lane("local", false), lane("app", true), lane("federation", false), lane("embeddings", false)])).toBe(true);
  });

  it("is false on a fresh cluster with no door at all", () => {
    expect(embeddingModelMissing([lane("local", false), lane("app", false), lane("federation", false), lane("embeddings", false)])).toBe(false);
  });

  it("is false once an embedding model is there", () => {
    expect(embeddingModelMissing([lane("app", true), lane("embeddings", true)])).toBe(false);
  });

  it("is false for a verdict written before the lane existed", () => {
    expect(embeddingModelMissing([lane("app", true)])).toBe(false);
    expect(embeddingModelMissing(undefined)).toBe(false);
  });
});
