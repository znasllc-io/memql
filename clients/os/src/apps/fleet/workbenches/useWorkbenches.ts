import { useCallback, useEffect, useState } from "react";
import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import type { LiveListSource } from "../../../live/LiveList";
import { useOsConnection } from "../../../live/connection";
import { useLiveCollection } from "../../../live/useLiveCollection";
import {
  latestPerId,
  nodeFromRow,
  workspaceFromRow,
  WORKBENCH_NODE_TYPE,
  type WorkbenchNodeRow,
  type WorkspaceRow,
} from "../rows";

export const WORKBENCH_WORKSPACE_CONCEPT = "v1:workbench:workspace";
export const CLUSTER_NODE_CONCEPT = "v1:cluster:node";

// The workbenches screen's state: the replicas that host workspaces, and the
// workspaces living on them.
//
// ===========================================================================
// TWO READS, AND ONLY ONE OF THEM CAN BE LIVE
// ===========================================================================
// v1:workbench:workspace carries broadcast routing rules
// (component/node/routing.go), so its events cross replicas to a browser and
// the workspaces list is a genuine LiveList. v1:cluster:node does NOT: it is
// not in that rule set, so a subscription over it would receive nothing in
// the only topology that ships -- and a list that silently never updates is
// worse than one that says when it was read.
//
// So the replicas are a plain query, refreshed on request, and the surface
// says so. The alternative -- deriving the replica set from the nodeId on
// each workspace -- cannot answer either of the two questions this screen
// exists for: a replica with no workspaces would be invisible, and "no
// workbench replicas at all" would be indistinguishable from "no workspaces".
//
// ===========================================================================
// THE NODE READ IS clusterNodes, AND IT IS NOT NARROWED SERVER-SIDE
// ===========================================================================
// dsl/cluster/queries.memql declares no per-nodeType read: clusterNodes takes
// no arguments, and nodesForDeployment / nodesNotInDeployment narrow by
// deployment rather than by role. Adding one would be a DSL change this
// surface does not own, so the narrowing to nodeType=workbench happens after
// the read.
//
// clusterNodes ALSO returns the whole append-only history -- its own comment
// says the CLI collapses to latest-per-id in Go -- which is what latestPerId
// does here. Without it one replica renders once per liveness row it has ever
// written.
//
// staleClusterNodes would have collapsed server-side, but it also filters
// `health!="stopped"`, and a replica that STOPPED is the most interesting row
// on a screen whose job is saying where a workspace's files went. Hiding it
// would answer the one question the screen is for with silence.

export interface WorkbenchesState {
  /** The workspaces feed itself, for a LiveList (or a view over one). Null
   *  until the connection exists, which LiveList renders as the
   *  disconnected caption rather than as an empty list. */
  source: LiveListSource<WorkspaceRow> | null;
  workspaceError: string;
  reseedWorkspaces: () => void;

  nodes: WorkbenchNodeRow[];
  nodesLoading: boolean;
  nodesError: string;
  nodesReadAt: Date | null;
  refreshNodes: () => void;
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function useWorkbenches(): WorkbenchesState {
  const connection = useOsConnection();

  const { source, snapshot, reseed } = useLiveCollection<WorkspaceRow>(
    connection === null ? null : "fleet:workspaces",
    (conn) => ({
      concept: WORKBENCH_WORKSPACE_CONCEPT,
      // myWorkspaces is scoped server-side on actor.userId and returns live
      // AND released rows, which is what lets the released toggle be a view
      // over these rows rather than a second read.
      seed: async (_cursor, signal) => {
        const result = await conn.query.myWorkspaces({}, { signal });
        return { rows: result.rows().map(workspaceFromRow), nextCursor: "" };
      },
      reread: async (rowId, signal) => {
        const row = await getRowByConceptAndId(conn.query, WORKBENCH_WORKSPACE_CONCEPT, rowId, {
          signal,
        });
        return row ? workspaceFromRow(row as Row) : null;
      },
      paged: false,
    }),
  );

  const [nodes, setNodes] = useState<WorkbenchNodeRow[]>([]);
  const [nodesLoading, setNodesLoading] = useState(false);
  const [nodesError, setNodesError] = useState("");
  const [nodesReadAt, setNodesReadAt] = useState<Date | null>(null);
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (connection === null) return;
    let live = true;
    setNodesLoading(true);
    setNodesError("");

    void connection.query
      .clusterNodes({})
      .then((result) => {
        if (!live) return;
        setNodes(
          latestPerId(
            result
              .rows()
              .map(nodeFromRow)
              .filter((node) => node.id !== "" && node.nodeType === WORKBENCH_NODE_TYPE),
          ),
        );
        setNodesReadAt(new Date());
      })
      .catch((err: unknown) => {
        // The replicas already on screen are KEPT: they were true when they
        // were read, and blanking them on a failed refresh replaces a stale
        // answer with no answer.
        if (live) setNodesError(describe(err));
      })
      .finally(() => {
        if (live) setNodesLoading(false);
      });

    return () => {
      live = false;
    };
  }, [connection, epoch]);

  const refreshNodes = useCallback(() => setEpoch((n) => n + 1), []);

  return {
    source,
    workspaceError: snapshot.error,
    reseedWorkspaces: reseed,
    nodes,
    nodesLoading,
    nodesError,
    nodesReadAt,
    refreshNodes,
  };
}
