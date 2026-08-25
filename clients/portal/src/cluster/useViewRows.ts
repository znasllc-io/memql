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
// THE LIVE BAND IS PASSED THROUGH (memql#4539). It used to stop here: this
// hook opened the browser's subscription and then discarded every event, so
// every predefined and composed view was live on the wire and dead on the
// screen -- an operator watching Users saw nothing arrive, with no way to tell
// that from a quiet cluster.
//
// It is still a BAND rather than a splice, and that part was always right: a
// view's readings are computed over the paged window, so splicing arrivals in
// would change a count under the operator's eyes mid-scan, and the walk's
// cursor orders by createdAt ascending -- a row created now is one paging has
// not reached, and inserting it guarantees a duplicate later
// (src/concepts/liveBand.ts).

export interface ViewRowsState {
  // Undefined when this cluster declares no such concept -- a product bundle
  // this node does not mount, or a renamed id. The view says so rather than
  // rendering an empty population that reads as "you have no accounts".
  concept: Concept | undefined;
  // True while the registry itself is still loading, which is distinct from
  // "the concept is not here".
  registryLoading: boolean;
  registryError: string;
  rows: Row[];
  walk: ConceptRowsState["walk"];
  // The CDC arrivals since this view was opened. Render it with
  // <LiveBandPanel>; `reload` is what its "Reload the list" control calls.
  live: ConceptRowsState["live"];
  // Set when the subscription could not be opened. Kept SEPARATE from the
  // walk's error: an ordinary read succeeding must not erase a "live updates
  // are off" notice, or the view looks live moments after going deaf.
  liveDegraded: string;
  loadMore: () => void;
  retry: () => void;
  // Restart the walk from the first page and clear the band -- the honest
  // response to "rows changed since you loaded this".
  reload: () => void;
}

export function useViewRows(conceptId: string): ViewRowsState {
  const { concepts, loading, error } = useConcepts();
  const { walk, live, liveDegraded, loadMore, retry, reload } = useConceptRows(conceptId);

  const rows = useMemo(() => walk.rows.map(flattenForList), [walk.rows]);
  const concept = concepts.find((candidate) => candidate.id === conceptId);

  return {
    concept,
    registryLoading: loading,
    registryError: error,
    rows,
    walk,
    live,
    liveDegraded,
    loadMore,
    retry,
    reload,
  };
}
