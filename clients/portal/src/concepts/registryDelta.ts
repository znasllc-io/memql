// The concept-registry-delta reducer (memql#4238).
//
// A follow-mode concept subscription (sdk/ts QueryClient.subscribeConceptRegistry)
// delivers a snapshot then live add/remove deltas. This module folds those
// deltas into a registry state, preserving the sorted-by-id invariant the whole
// portal reads the registry through, and detecting a GENERATION GAP -- a missed
// delta (a slow consumer dropped one, or an incremental arrived before a
// snapshot) -- which the hook recovers from by re-snapshotting.
//
// Pure and React-free on purpose, so the fold semantics are pinned here rather
// than through three layers of render(), exactly like src/concepts/registry.ts.

import type { Concept, ConceptRegistryDelta } from "@znasllc-io/memql-sdk-core/client";

export interface RegistryState {
  // Sorted by id ascending -- the invariant every registry reader depends on.
  concepts: Concept[];
  // The generation of the last applied delta. 0 before the snapshot.
  generation: number;
  // True once a snapshot (reset) has been applied. Distinguishes "empty because
  // we have not heard yet" from "empty registry".
  ready: boolean;
}

export const EMPTY_REGISTRY: RegistryState = { concepts: [], generation: 0, ready: false };

export interface RegistryApplyResult {
  state: RegistryState;
  // gap === true means the caller must re-snapshot (re-subscribe): a delta was
  // missed, so `state` is left unchanged and applying this delta would corrupt
  // the registry.
  gap: boolean;
}

function sortById(concepts: Concept[]): Concept[] {
  return [...concepts].sort((a, b) => a.id.localeCompare(b.id));
}

// foldInto upserts `added` (by id) and drops `removed`, returning a fresh
// sorted array. Shared by the reset and incremental paths.
function foldInto(base: Concept[], added: Concept[], removed: string[]): Concept[] {
  const byId = new Map<string, Concept>();
  for (const c of base) byId.set(c.id, c);
  for (const c of added) byId.set(c.id, c);
  for (const id of removed) byId.delete(id);
  return sortById([...byId.values()]);
}

// applyRegistryDelta folds one delta into the state.
//
//   - reset: replace the whole registry with `added` (the snapshot).
//   - incremental, contiguous generation: upsert `added`, drop `removed`.
//   - incremental, generation <= current: a duplicate (the atomic snapshot can
//     overlap the first delta); ignore it, idempotently.
//   - incremental before any snapshot, or a generation gap: signal gap so the
//     caller re-snapshots.
export function applyRegistryDelta(
  prev: RegistryState,
  delta: ConceptRegistryDelta,
): RegistryApplyResult {
  if (delta.reset) {
    return {
      state: {
        concepts: foldInto([], delta.added, []),
        generation: delta.generation,
        ready: true,
      },
      gap: false,
    };
  }

  // An incremental before the snapshot: we cannot place it. Re-snapshot.
  if (!prev.ready) {
    return { state: prev, gap: true };
  }

  // Duplicate / stale -- already reflected. Idempotent no-op.
  if (delta.generation <= prev.generation) {
    return { state: prev, gap: false };
  }

  // A hole: we missed at least one delta. Re-snapshot rather than apply out of
  // order.
  if (delta.generation > prev.generation + 1) {
    return { state: prev, gap: true };
  }

  return {
    state: {
      concepts: foldInto(prev.concepts, delta.added, delta.removed),
      generation: delta.generation,
      ready: true,
    },
    gap: false,
  };
}
