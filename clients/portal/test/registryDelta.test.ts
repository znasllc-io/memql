// The concept-registry-delta reducer (memql#4238): the fold semantics, the
// sorted-by-id invariant, and the generation-gap detection that drives the
// hook's re-snapshot -- pinned here, pure, with no React in the way.

import { describe, expect, it } from "vitest";
import type { Concept, ConceptRegistryDelta } from "@znasllc-io/memql-sdk-core/client";

import { EMPTY_REGISTRY, applyRegistryDelta } from "../src/concepts/registryDelta";

function concept(id: string): Concept {
  const [version = "", domain = "", entity = ""] = id.split(":");
  return { id, version, domain, entity, description: "", type: "concept" };
}

function delta(partial: Partial<ConceptRegistryDelta>): ConceptRegistryDelta {
  return { generation: 0, added: [], removed: [], reset: false, ...partial };
}

describe("applyRegistryDelta", () => {
  it("applies a reset snapshot, sorting by id", () => {
    const { state, gap } = applyRegistryDelta(
      EMPTY_REGISTRY,
      delta({ generation: 5, reset: true, added: [concept("v1:b:b"), concept("v1:a:a")] }),
    );
    expect(gap).toBe(false);
    expect(state.ready).toBe(true);
    expect(state.generation).toBe(5);
    expect(state.concepts.map((c) => c.id)).toEqual(["v1:a:a", "v1:b:b"]);
  });

  it("upserts an incremental add in place, keeping the sort", () => {
    const snap = applyRegistryDelta(
      EMPTY_REGISTRY,
      delta({ generation: 1, reset: true, added: [concept("v1:a:a"), concept("v1:c:c")] }),
    ).state;
    const { state, gap } = applyRegistryDelta(
      snap,
      delta({ generation: 2, added: [concept("v1:b:b")] }),
    );
    expect(gap).toBe(false);
    expect(state.generation).toBe(2);
    expect(state.concepts.map((c) => c.id)).toEqual(["v1:a:a", "v1:b:b", "v1:c:c"]);
  });

  it("re-promote (same id, changed descriptor) replaces in place", () => {
    const snap = applyRegistryDelta(
      EMPTY_REGISTRY,
      delta({ generation: 1, reset: true, added: [{ ...concept("v1:a:a"), description: "old" }] }),
    ).state;
    const { state } = applyRegistryDelta(
      snap,
      delta({ generation: 2, added: [{ ...concept("v1:a:a"), description: "new" }] }),
    );
    expect(state.concepts).toHaveLength(1);
    expect(state.concepts[0]?.description).toBe("new");
  });

  it("drops a removed id", () => {
    const snap = applyRegistryDelta(
      EMPTY_REGISTRY,
      delta({ generation: 1, reset: true, added: [concept("v1:a:a"), concept("v1:b:b")] }),
    ).state;
    const { state, gap } = applyRegistryDelta(
      snap,
      delta({ generation: 2, removed: ["v1:a:a"] }),
    );
    expect(gap).toBe(false);
    expect(state.concepts.map((c) => c.id)).toEqual(["v1:b:b"]);
  });

  it("ignores a duplicate/stale delta (generation <= current) idempotently", () => {
    const snap = applyRegistryDelta(
      EMPTY_REGISTRY,
      delta({ generation: 3, reset: true, added: [concept("v1:a:a")] }),
    ).state;
    // A delta at the current generation is a no-op (the atomic snapshot can
    // overlap the first delta).
    const same = applyRegistryDelta(snap, delta({ generation: 3, added: [concept("v1:z:z")] }));
    expect(same.gap).toBe(false);
    expect(same.state).toBe(snap); // unchanged reference
    // An older generation is also ignored.
    const older = applyRegistryDelta(snap, delta({ generation: 2, removed: ["v1:a:a"] }));
    expect(older.gap).toBe(false);
    expect(older.state.concepts.map((c) => c.id)).toEqual(["v1:a:a"]);
  });

  it("signals a gap when a generation is skipped, leaving state unchanged", () => {
    const snap = applyRegistryDelta(
      EMPTY_REGISTRY,
      delta({ generation: 1, reset: true, added: [concept("v1:a:a")] }),
    ).state;
    const { state, gap } = applyRegistryDelta(
      snap,
      delta({ generation: 3, added: [concept("v1:b:b")] }), // skipped 2
    );
    expect(gap).toBe(true);
    expect(state).toBe(snap); // untouched -- the hook re-snapshots
  });

  it("signals a gap for an incremental that arrives before any snapshot", () => {
    const { gap, state } = applyRegistryDelta(
      EMPTY_REGISTRY,
      delta({ generation: 2, added: [concept("v1:a:a")] }),
    );
    expect(gap).toBe(true);
    expect(state.ready).toBe(false);
  });
});
