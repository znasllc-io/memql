import { useMemo } from "react";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

import { useConcepts } from "./useConcepts";
import { useConceptRows, type ConceptRowsState } from "./useConceptRows";
import { flattenForList } from "../viewkit/rows";

// One concept's rows, ready for an element.
//
// Deliberately thin. It reuses the concept browser's machinery wholesale --
// the same registry read, the same keyset walk, the same CDC live band -- so a
// predefined view and the generic browser cannot disagree about what a
// concept's rows are. A second fetch path would be a second answer.
//
// TWO THINGS IT ADDS:
//
//   1. The DESCRIPTOR. Elements need a ConceptLike (id, entity, displayCard),
//      and the registry the browser already read is where it comes from --
//      there is no per-concept metadata call, and inventing one would be a
//      second source of truth for the display card.
//   2. The FLATTEN. The wire nests a node's payload under `payload`, while a
//      @displayCard slot names payload fields directly. Elements read bare
//      field names, so the merge has to happen before they see a row. Same
//      projection the browser's row list uses, from the same module.
//
// The live band is deliberately NOT merged in here. A view's readings are
// computed over the paged window; splicing CDC arrivals into it would change
// a count under the operator's eyes mid-scan, and the walk's cursor orders by
// createdAt ascending so a row created now is one paging has not reached --
// inserting it guarantees a duplicate later (src/concepts/liveBand.ts).

export interface ViewRowsState {
  // Undefined when this cluster declares no such concept -- a product bundle
  // this node does not mount, or a renamed id. The view says so rather than
  // rendering an empty population that reads as "you have no customers".
  concept: Concept | undefined;
  // True while the registry itself is still loading, which is distinct from
  // "the concept is not here".
  registryLoading: boolean;
  registryError: string;
  rows: Row[];
  walk: ConceptRowsState["walk"];
  loadMore: () => void;
  retry: () => void;
}

export function useViewRows(conceptId: string): ViewRowsState {
  const { concepts, loading, error } = useConcepts();
  const { walk, loadMore, retry } = useConceptRows(conceptId);

  const rows = useMemo(() => walk.rows.map(flattenForList), [walk.rows]);
  const concept = concepts.find((candidate) => candidate.id === conceptId);

  return {
    concept,
    registryLoading: loading,
    registryError: error,
    rows,
    walk,
    loadMore,
    retry,
  };
}
