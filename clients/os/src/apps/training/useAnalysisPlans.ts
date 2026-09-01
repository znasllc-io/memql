import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { PLAN_CONCEPT } from "./concepts";

// The analysis feed: ONE LiveCollection over the caller's plan rows.
//
// ===========================================================================
// WHY THIS IS LIVE WITH NO ENGINE WORK
// ===========================================================================
// `component/node/routing.go` carries `graph.node.created.v1:planner:*`,
// `.updated.` and `.deleted.` to browser subscribers -- three wildcard rules
// over the whole planner namespace. So the epic's headline is true as it
// stands: the attachment handler stamps a queued Plan synchronously and then
// finishes the work on a DETACHED goroutine, on whichever node took the
// upload, and the status transitions land under the person watching with
// nothing polling.
//
// READ THE ROUTING RULES, NOT THE CONCEPT NAMES. The Fleet's first cut decided
// `v1:cluster:node` was dark by looking for a rule carrying that concept's own
// name rather than reading the patterns, and printed the mistake on the page
// as operator-facing copy.
//
// ===========================================================================
// THE SEED IS SCOPED SERVER-SIDE; THE SUBSCRIPTION IS NOT
// ===========================================================================
// `plansForUser` is `@actor` and binds `requestedBy==actor.userId`, so the
// seed is the caller's own plans and nothing else. The SUBSCRIPTION has no
// such gate: `v1:planner:plan` declares no row-authz tier (memql#4366), and a
// concept that declares nothing admits every subscriber -- so other people's
// plan rows arrive on this feed. `planBelongsHere` (rows.ts) is where that is
// filtered, labelled as the client-side residual it is, exactly the way Nexus
// labels its own.
//
// The KEY is a constant. It must encode everything that changes what is READ
// and nothing that merely arrives late -- an actor id folded into it would
// restart the collection from empty the moment access resolved, which
// unmounts the list somebody is watching an upload in.

export function useAnalysisPlans(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("training:plans", (connection) => ({
    concept: PLAN_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.plansForUser({}, { signal });
      // ONE PAGE, DELIBERATELY. `plansForUser` paginates at 50 newest-first,
      // and this surface is about work that is happening or just happened --
      // walking a person's whole plan history to show a dropzone would be a
      // read nobody asked for. The section says it is showing recent analyses
      // rather than claiming a total.
      return { rows: result.rows(), nextCursor: "" };
    },
    // The re-read a `payload_omitted` event lands on, and the one a gap
    // recovery uses. `v1:planner:plan` is undeclared rather than granted, so
    // the first case does not arise today; the fold uses the same seam for
    // both, so it is wired either way.
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, PLAN_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}
