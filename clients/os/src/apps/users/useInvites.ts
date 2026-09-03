import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { invitationFromRow, invitationIsPending } from "./rows";

// The invitations feed: one LiveCollection over the cluster's outstanding user
// invitations.
//
// UNLIKE THE PEOPLE FEED, THIS ONE IS LIVE IN BOTH DIRECTIONS.
// component/node/routing.go carries BOTH
// `graph.node.created.v1:identity:invitation` and its `updated` twin to
// browser subscribers, on the stated grounds that an invitation is a human
// action and therefore low volume. That is what lets an acceptance take a row
// off this list without a refetch: `markUserInvitationAccepted` writes
// `status: "accepted"`, the update broadcasts, and the row stops satisfying
// the read's own membership predicate.
//
// `inScope` IS THAT PREDICATE, said again about arriving events. The
// subscription is scoped by CONCEPT and the read is scoped by
// `kind=="user" && statusIsPending && isActiveRecord`, so without it every
// guest invitation in the cluster and every invitation anyone accepts would
// fold straight into this list -- appearing live and then vanishing on the
// next reseed, which is the worst of both.
//
// It deliberately does NOT touch seeded rows (see LiveCollectionSpec): the
// read is the authority on membership, and this is a client-side mirror of a
// decision the server already made.

export const INVITATION_CONCEPT = "v1:identity:invitation";

/**
 * Every outstanding user invitation, live.
 *
 * The gate is the query's own: `pendingUserInvitations` carries
 * `requiresDeveloperOrAbove` as a top-level conjunct, so the engine empties
 * the result below that whatever this code renders. Developer is included
 * because it can ISSUE an invitation, and a caller who can send one but not
 * see the outstanding ones cannot revoke a link sent to the wrong address.
 *
 * Its projection is `invitationAdminSummary` (memql#4735), which is what makes
 * this read safe to run from a browser at all: the query used to declare no
 * shape, and a query with no shape projects the concept's every field --
 * `tokenHash`, `previousTokenHash` and `bindingHash` included. Those are the
 * key the redeem path looks an invitation up BY, and a pending list has never
 * read them.
 */
export function useInvites(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("users:invites", (connection) => ({
    concept: INVITATION_CONCEPT,
    actions: ["created", "updated"],
    seed: async (_cursor, signal) => {
      const result = await connection.query.pendingUserInvitations({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, INVITATION_CONCEPT, rowId, {
        signal,
      });
      return (row as Row) ?? null;
    },
    inScope: (row) => invitationIsPending(invitationFromRow(row)),
    paged: false,
  }));
}
