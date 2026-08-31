import { getRowByConceptAndId, type LiveState, type Row } from "@znasllc-io/memql-sdk-core/client";

import type { LiveListSource } from "../../../live/LiveList";
import { useOsConnection } from "../../../live/connection";
import { useLiveCollection } from "../../../live/useLiveCollection";
import { nodeFromRow, WORKBENCH_NODE_TYPE } from "../rows";

export const WORKBENCH_WORKSPACE_CONCEPT = "v1:workbench:workspace";
export const CLUSTER_NODE_CONCEPT = "v1:cluster:node";

// The workbenches screen's state: the replicas that host per-plan working
// directories, and the directories on them.
//
// ===========================================================================
// BOTH FEEDS ARE LIVE, AND THE SECOND ONE ONLY IS BECAUSE THIS WAS CHECKED
// ===========================================================================
// v1:workbench:workspace carries three explicit broadcast rules
// (component/node/routing.go), so its events reach a browser subscriber.
//
// v1:cluster:node does too, and by a route that is easy to miss: there is no
// rule naming it. It is covered by the `graph.node.{created,updated,deleted}
// .v1:cluster:*` WILDCARDS in the core rule block, which have been forwarding
// since the mesh existed. The first version of this file asserted the
// opposite, built a polled query around it, and printed the claim on the page
// as operator-facing copy -- a statement about the system that was simply
// untrue. Read the rules, do not reason from the absence of a rule with the
// concept's name in it.
//
// The concept also declares NO row-authz tier, so admission is open on the
// subscription path exactly as it is on the read path (memql#4309): every
// signed-in user sees replica rows.
//
// ===========================================================================
// THE NODE READ IS clusterNodes, AND IT NARROWS NOWHERE BUT HERE
// ===========================================================================
// dsl/cluster/queries.memql declares no per-nodeType read: clusterNodes has
// an EMPTY body -- no filter, no sort, no shape, no paginate -- and
// nodesForDeployment / nodesNotInDeployment narrow by deployment rather than
// by role. So the narrowing to nodeType=workbench happens here, in the seed
// (where this read's meaning belongs) and again in `inScope`, which says the
// same thing about arriving events. Without the second one a bff node's
// heartbeat folds into the workbench list.
//
// ===========================================================================
// AND THE HISTORY IS COLLAPSED IN THE SEED, NOT AFTER IT
// ===========================================================================
// v1:cluster:node is APPEND-ONLY: every liveness transition writes a new row
// under the same id, and clusterNodes returns the whole history in NO
// DECLARED ORDER (its body is empty, so there is no `sort`). The collection
// folds by id and therefore keeps whichever row it saw LAST -- which, over an
// unordered read, is an arbitrary one of a replica's lifetime.
//
// So latestPerId runs INSIDE the seed, over the whole history, before the
// collection ever sees it. That makes the collapse a property of the read
// rather than of the order the read happened to come back in. Folded events
// need no such care: they arrive newest-last by construction.
//
// staleClusterNodes would have collapsed server-side, but it also filters
// `health!="stopped"`, and a replica that STOPPED is the most interesting row
// on a screen whose job is saying where a workspace's files went.

export interface WorkbenchesState {
  /**
   * The workspaces feed, carrying RAW wire rows. Null until the connection
   * exists, which LiveList renders as the disconnected caption rather than as
   * an empty list.
   *
   * Raw because the fold has to be: an arriving event's payload is upserted
   * AS the row type with no projection hook in between, so a collection typed
   * with a projected row holds a raw one from the first update onward.
   * Callers wrap this in `useLiveView` and project there.
   */
  source: LiveListSource<Row> | null;
  workspaceState: LiveState;
  workspaceError: string;
  reseedWorkspaces: () => void;

  /** The workbench replicas, same contract, same reason. */
  nodeSource: LiveListSource<Row> | null;
  nodeState: LiveState;
  nodeError: string;
  reseedNodes: () => void;
}

export function useWorkbenches(): WorkbenchesState {
  const connection = useOsConnection();

  const workspaces = useLiveCollection<Row>(
    connection === null ? null : "fleet:workspaces",
    (conn) => ({
      concept: WORKBENCH_WORKSPACE_CONCEPT,
      // myWorkspaces is scoped server-side on actor.userId and returns live
      // AND released rows, which is what lets the released toggle be a view
      // over these rows rather than a second read.
      seed: async (_cursor, signal) => {
        const result = await conn.query.myWorkspaces({}, { signal });
        return { rows: result.rows(), nextCursor: "" };
      },
      reread: async (rowId, signal) => {
        const row = await getRowByConceptAndId(conn.query, WORKBENCH_WORKSPACE_CONCEPT, rowId, {
          signal,
        });
        return (row as Row) ?? null;
      },
      paged: false,
    }),
  );

  const nodes = useLiveCollection<Row>(
    connection === null ? null : "fleet:workbench:nodes",
    (conn) => ({
      concept: CLUSTER_NODE_CONCEPT,
      seed: async (_cursor, signal) => {
        const result = await conn.query.clusterNodes({}, { signal });
        // The collapse belongs to the READ (see the header), and it keeps the
        // RAW row while comparing PROJECTED fields -- the collection stores
        // what the wire sent and the surface projects on the way out.
        const newest = new Map<string, { createdAt: string; raw: Row }>();
        for (const raw of result.rows()) {
          const node = nodeFromRow(raw);
          if (node.id === "" || node.nodeType !== WORKBENCH_NODE_TYPE) continue;
          const held = newest.get(node.id);
          if (held === undefined || node.createdAt >= held.createdAt) {
            newest.set(node.id, { createdAt: node.createdAt, raw });
          }
        }
        return { rows: [...newest.values()].map((one) => one.raw), nextCursor: "" };
      },
      reread: async (rowId, signal) => {
        const row = await getRowByConceptAndId(conn.query, CLUSTER_NODE_CONCEPT, rowId, { signal });
        return (row as Row) ?? null;
      },
      // The seed's narrowing, restated for arriving events -- every other
      // node type in the mesh heartbeats onto this same concept. Projected
      // here for the same reason every predicate over these rows is: the row
      // this receives is whatever the wire sent.
      inScope: (raw) => nodeFromRow(raw).nodeType === WORKBENCH_NODE_TYPE,
      paged: false,
    }),
  );

  return {
    source: workspaces.source,
    workspaceState: workspaces.snapshot.state,
    workspaceError: workspaces.snapshot.error,
    reseedWorkspaces: workspaces.reseed,
    nodeSource: nodes.source,
    nodeState: nodes.snapshot.state,
    nodeError: nodes.snapshot.error,
    reseedNodes: nodes.reseed,
  };
}
