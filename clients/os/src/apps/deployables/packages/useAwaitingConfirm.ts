import { getRowByConceptAndId, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../../kit/rows";
import { useLiveCollection, type LiveCollectionHandle } from "../../../live/useLiveCollection";
import { DEPLOYMENT_CONCEPT } from "./rows";

// The parked runs: ONE LiveCollection over every deployment at
// `awaiting_confirm` the caller may read (epic memql#4885, design section A).
//
// ===========================================================================
// THE FOURTH FEED AT THE ROOT, AND WHY IT IS THE RECORDED EXCEPTION
// ===========================================================================
// clients/os/README.md ("Packages, inside Deployables") states the rule: a
// package's deployment TIMELINE is retained by the page and never by the app
// root, because keeping every package's timeline live would subscribe the
// window to every deploy in the cluster to render one. This feed is retained
// at the root beside the sites, the packages and the credentials, and it is
// the ONE recorded exception to that rule.
//
// It is an exception the rule allows for, because it holds PARKED RUNS ONLY:
// a run at `awaiting_confirm` is a handful of rows -- one per source somebody
// analyzed and has not yet confirmed -- and a person needs to see them before
// they open anything, which is what the list's waiting mark ("a deploy is
// waiting for you") is for. A person who closed the window mid-compose finds
// their run on its row rather than by remembering which source it was. The
// rule guards against the timeline of every deploy in the cluster; this feed
// never holds a timeline, and never holds a run that has moved on.
//
// The seed narrows to `awaiting_confirm` server-side, and `inScope` says the
// same thing about every folded event: a run that confirms, refuses or fails
// leaves the feed on its own update event, so the mark clears the moment the
// run moves and nothing here has to poll for it.

/**
 * Every parked run the caller may read, live.
 *
 * NO ARGUMENTS and a constant KEY: `packageDeploymentsAwaitingConfirm`
 * carries the owned tier, so the engine decides how far the list reaches.
 * The call is rendered by hand because the generated builder lands with the
 * engine task; the text is exactly what that builder renders.
 */
export function useAwaitingConfirm(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("deployables:awaitingConfirm", (connection) => ({
    concept: DEPLOYMENT_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.executeNamed(
        "packageDeploymentsAwaitingConfirm",
        "query packageDeploymentsAwaitingConfirm()",
        { signal },
      );
      return { rows: result.rows(), nextCursor: "" };
    },
    // The subscription is over the whole concept; the READ is parked runs
    // only. A folded event for a run that has moved on is out of scope, and
    // the collection removes the row rather than ignoring the event -- which
    // is exactly what clears the mark.
    inScope: (row) => rowString(flatten(row), "status") === "awaiting_confirm",
    // The re-read a `payload_omitted` event lands on, and the collection's
    // gap recovery -- the same seam every other feed in this app wires.
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, DEPLOYMENT_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}
