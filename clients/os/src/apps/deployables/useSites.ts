import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { SITE_CONCEPT } from "./concepts";

// The deployables feed: ONE LiveCollection over this cluster's site rows.
//
// ===========================================================================
// CREATED AND UPDATED ARE BOTH LIVE, AND THAT IS WHY THIS APP HAS NO POLL
// ===========================================================================
// `component/node/routing.go` carries BOTH `graph.node.created.v1:platform:site`
// and `graph.node.updated.v1:platform:site` to browser subscribers. That
// asymmetry with `v1:identity:user` -- which broadcasts creates and
// deliberately not updates, because the row churns on `lastSeenAt` -- is the
// reason the Users app re-reads a person on open and this app does not: a site
// row moves when somebody deploys, which is a human action rather than a
// heartbeat.
//
// It is also what makes the epic's central claim true without any engine work:
// a CI publish through `POST /sites/{id}/bundles` flips `bundleRef` on a node
// nobody in this browser is talking to, the update broadcasts, and the row
// changes under the person watching it. Read the ROUTING RULES before
// concluding a concept is dark -- the Fleet got `v1:cluster:node` wrong by
// looking for a rule with the concept's name in it rather than reading the
// patterns, and printed the mistake on the page as operator-facing copy.
//
// ===========================================================================
// ONE COLLECTION FOR THE WHOLE APP
// ===========================================================================
// The list and the map are two READINGS of one feed, not two feeds. They are
// retained once, at the app root, and passed down: a second `useSites()` inside
// the map would open a second subscription and run a second seed, and the two
// would then be free to disagree about what the cluster currently holds --
// which is the one thing a map beside a list must never do.

/**
 * Every deployable the caller may read, live.
 *
 * NO ARGUMENTS. `sitesAll` carries `isNotDeleted` and the composite tier's own
 * predicate (`ownerUserId==actor.userId || actor.isClusterOwner==true`), so the
 * engine decides how far "all" reaches: a cluster owner's list is every site in
 * the cluster and an ordinary caller's is their own. Same call, same surface,
 * different population.
 *
 * The KEY is a constant. It must encode everything that changes what is READ,
 * and nothing that merely arrives late -- an actor id folded into it would
 * restart the collection from empty the moment access resolved, unmounting
 * whatever was open.
 */
export function useSites(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("deployables:sites", (connection) => ({
    concept: SITE_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.sitesAll({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    // The re-read a `payload_omitted` event lands on. A `granted`-tier row
    // cannot be decided against one row, so the engine sends the id alone and
    // the client re-reads it through the authorized path; `v1:platform:site` is
    // owner-or-cluster-owner rather than granted, but the collection's fold
    // uses the same seam for a gap recovery, so it is wired either way.
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, SITE_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}
