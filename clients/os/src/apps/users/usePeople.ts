import {
  getRowByConceptAndId,
  type QueryClient,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";

// The people feed: one LiveCollection over the cluster's user rows.
//
// ===========================================================================
// CREATED IS LIVE; UPDATED IS NOT, AND THAT IS DELIBERATE
// ===========================================================================
// component/node/routing.go carries `graph.node.created.v1:identity:user` to
// browser subscribers and carries NO `updated` rule for the same concept. That
// asymmetry is the whole exemplar: an invitee registers, the create broadcasts,
// and the row slides in with the arrival tick while somebody is watching.
//
// The absence of the update rule is a VOLUME decision, not an oversight, and
// this task must not add one. A user row churns on `lastSeenAt` and on
// preferences, so broadcasting updates would push an event per person per
// heartbeat across the mesh forever -- the same reasoning that keeps
// `v1:worker:invocation` off the broadcast list.
//
// What that costs, and how it is paid: an admin action's effect does not
// arrive as an event. So the detail panel re-reads its person on open, and
// every write in actions.ts hands its own updated row straight back to the
// surface (see `applyPerson`). Nothing here polls.

export const USER_CONCEPT = "v1:identity:user";

/**
 * Every user row the caller may read, live on creation.
 *
 * NO ARGUMENTS, and `searchUsers`' optional `active` filter is deliberately
 * not passed: the show-deactivated toggle is a view over rows already here
 * (see `settings.ts`). Seeding filtered would make the toggle re-run the read
 * and re-baseline every arrival cue, so flipping it would announce the whole
 * list as new.
 *
 * The gate is the query's own -- `searchUsers` carries `requiresOwnerOrAdmin`
 * as a top-level conjunct, so the engine empties the result for a caller below
 * the floor whatever this code renders. The manifest's `roles: { min: "admin" }`
 * is presentation on top of that, never instead of it.
 */
export function usePeople(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("users:people", (connection) => ({
    concept: USER_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.searchUsers({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, USER_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

/**
 * Re-read ONE person through the authorized read path.
 *
 * The detail panel's own read, used on open. It exists because there is no
 * `updated` broadcast for this concept (see above), so a person opened
 * minutes after the seed would otherwise render whatever the list happened to
 * hold. Best-effort by contract: the caller renders what it has when this
 * returns null rather than failing the panel.
 */
export async function rereadPerson(
  query: QueryClient,
  userId: string,
  signal?: AbortSignal,
): Promise<Row | null> {
  return getRowByConceptAndId(query, USER_CONCEPT, userId, signal ? { signal } : {});
}
