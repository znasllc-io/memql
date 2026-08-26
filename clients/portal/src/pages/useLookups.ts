import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { LookupCache } from "@znasllc-io/memql-sdk-core/client";
import {
  resolveDisplayCard,
  type ConceptLike,
  type RefResolver,
  type RowLike,
} from "@znasllc-io/memql-view-kit";

import { useCluster } from "../cluster/ClusterProvider";
import { useConcepts } from "../cluster/useConcepts";

// Wiring lookup columns to the cluster (epic memql#4661, task memql#4671).
//
// ===========================================================================
// THE CACHE IS PER PAGE, AND ITS LIFETIME IS THE POINT
// ===========================================================================
// Held in a ref so it survives re-renders and dies with the page. A cache that
// outlived the page would serve a stale display name after somebody renamed
// the row it points at -- and the staleness would be invisible, because a
// resolved cell looks the same whether its label is current or a week old.
//
// ===========================================================================
// RESOLUTION IS AN EFFECT, RENDERING IS SYNCHRONOUS
// ===========================================================================
// A cell cannot await. So the resolver this returns is a pure lookup into what
// has already arrived, and the effect below is what makes things arrive: it
// collects every pointer value on the page, asks for the missing ones in one
// read per target concept, and bumps a nonce when the answers land.
//
// The first paint therefore renders IDS, and the second renders labels. That
// is deliberate rather than tolerated: the alternative is a page that blocks
// on a lookup, and an id is a true, useful thing to show in the meantime.

export interface Lookups {
  // Handed to view-kit's cell renderer. Pure: it answers from what has
  // arrived and never starts a read.
  readonly resolve: RefResolver;
  // Bumped when a batch lands, so a memo over the rendered arrangement
  // recomputes. Without it the resolver would be called again with the same
  // (stale) closure and the page would keep showing ids.
  readonly epoch: number;
}

export function useLookups(
  concept: ConceptLike | undefined,
  rows: readonly RowLike[],
): Lookups {
  const { query } = useCluster();
  const { concepts } = useConcepts();
  const cache = useRef(new LookupCache());
  const [epoch, setEpoch] = useState(0);

  // Which fields on this concept are relationship pointers, and where each
  // one points. From the DECLARATION, which is the only place the answer
  // exists: a foreign key is a string like any other string.
  const edges = useMemo(() => {
    const out = new Map<string, { field: string; target: string }>();
    for (const rel of concept?.relationships ?? []) {
      if (rel.field === "" || rel.field.includes(".")) continue;
      const label = rel.as !== undefined && rel.as !== "" ? rel.as : rel.field;
      if (!out.has(label)) out.set(label, { field: rel.field, target: rel.target });
    }
    return out;
  }, [concept]);

  useEffect(() => {
    if (query === null || edges.size === 0 || rows.length === 0) return;
    let live = true;

    // Grouped by TARGET CONCEPT, not by column: two columns pointing at the
    // same concept are one read, which is the whole reason to batch.
    const byConcept = new Map<string, string[]>();
    for (const { field, target } of edges.values()) {
      if (target === "") continue;
      const ids = byConcept.get(target) ?? [];
      for (const row of rows) {
        const value = row[field];
        if (typeof value === "string" && value !== "") ids.push(value);
      }
      byConcept.set(target, ids);
    }

    void Promise.all(
      [...byConcept.entries()].map(([target, ids]) => cache.current.resolve(query, target, ids)),
    )
      // A failed batch is SWALLOWED here on purpose. Every cell already
      // renders its id, which is a true and useful thing to show; a banner
      // saying "could not resolve a display name" would be noise on a page
      // that is working.
      .catch(() => undefined)
      .finally(() => {
        if (live) setEpoch((n) => n + 1);
      });

    return () => {
      live = false;
    };
  }, [query, edges, rows]);

  // The label to show for a resolved row: its display card's primary, which is
  // what the concept itself says identifies a row. Falling back to the id
  // means a target concept with no card still resolves to something -- and to
  // the same thing the cell would have shown unresolved, which is honest.
  const labelFor = useCallback(
    (target: string, row: RowLike): string => {
      const targetConcept = concepts.find((c) => c.id === target);
      const card = resolveDisplayCard(
        targetConcept ?? { id: target, entity: target },
        [row],
      );
      const primary = card.primary === undefined ? undefined : row[card.primary];
      if (typeof primary === "string" && primary !== "") return primary;
      if (typeof primary === "number" || typeof primary === "boolean") return String(primary);
      return "";
    },
    [concepts],
  );

  const resolve = useCallback<RefResolver>(
    (relationshipAs, rowId) => {
      const edge = edges.get(relationshipAs);
      if (edge === undefined || edge.target === "") return undefined;
      const found = cache.current.get(edge.target, rowId);
      // undefined -> not asked yet; null -> asked, and there is no such row
      // this caller can read. Both render as the id, which is the correct
      // answer to both and is never blank.
      if (found === undefined || found === null) return undefined;
      return labelFor(edge.target, found as RowLike);
    },
    [edges, labelFor],
  );

  return { resolve, epoch };
}

// referenceColumns names the lookup columns a concept could offer, for a
// composer's picker. Every relationship pointer that is also a DISPLAY column
// -- a pointer the table would otherwise render as a raw id.
export function referenceColumns(concept: ConceptLike, rows: readonly RowLike[]): readonly string[] {
  void rows;
  const out: string[] = [];
  for (const rel of concept.relationships ?? []) {
    if (rel.field === "" || rel.field.includes(".")) continue;
    out.push(rel.as !== undefined && rel.as !== "" ? rel.as : rel.field);
  }
  return out;
}

