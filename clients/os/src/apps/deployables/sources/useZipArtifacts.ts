import { useRef } from "react";
import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useLiveCollection, type LiveCollectionHandle } from "../../../live/useLiveCollection";
import { ARTIFACT_CONCEPT, isZipArtifact } from "../concepts";

// The publish picker's feed: the caller's own Library zips.
//
// NARROWED IN THE SEED, and mirrored on the events with `inScope`, which is
// exactly the split the SDK's own contract asks for: the READ is the authority
// on membership, and `inScope` says the same thing about a folded event so a
// non-zip artifact somebody uploads while the picker is open does not appear in
// a list of bundles.
//
// The narrowing is a CLIENT-SIDE one over a server-side list. `libraryArtifacts`
// is owner-scoped and carries `archived!=true`; what it cannot express is "and
// only the zips", because the Library has no such facet. So the predicate lives
// here -- and the server refuses a non-zip by name anyway (`artifact_not_a_zip`),
// which is what keeps this a courtesy rather than a gate.

/** The first page `libraryArtifacts` returns; its `paginate` is 50. */
export const PICKER_PAGE_SIZE = 50;

export interface ZipArtifactsHandle {
  feed: LiveCollectionHandle<Row>;
  /**
   * Whether the Library page came back FULL -- so there may be older bundles
   * this picker is not offering.
   *
   * It is a question about the RAW page, not about the zips left after the
   * filter, and the difference is the whole reason it is measured here. Someone
   * with fifty Library entries of which three are zips has a full page and
   * three rows on screen; a caption conditioned on the row count would stay
   * silent for exactly the person most likely to be missing a bundle.
   *
   * A ref rather than state, deliberately: it is written inside the seed, which
   * completes by changing the snapshot -- so the render that first shows rows
   * is already the render that can read it, and a `setState` here would only
   * add a second render saying the same thing.
   */
  pageWasFull: boolean;
}

export function useZipArtifacts(enabled: boolean): ZipArtifactsHandle {
  const full = useRef(false);
  // A NULL KEY IS "DO NOT READ": the picker is behind a disclosure, and
  // seeding somebody's whole Library because a detail panel opened would be a
  // read nobody asked for. `useLiveCollection` builds no collection for a null
  // key, so this is an absence rather than an idle subscription.
  const feed = useLiveCollection<Row>(enabled ? "deployables:zips" : null, (connection) => ({
    concept: ARTIFACT_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.libraryArtifacts({}, { signal });
      const page = result.rows();
      full.current = page.length >= PICKER_PAGE_SIZE;
      return { rows: page.filter(isZipArtifact), nextCursor: "" };
    },
    inScope: isZipArtifact,
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, ARTIFACT_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
  return { feed, pageWasFull: full.current };
}
