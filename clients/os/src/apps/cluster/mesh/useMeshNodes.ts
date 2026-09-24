import { Concepts, getRowByConceptAndId, type LiveState, type Row } from "@znasllc-io/memql-sdk-core/client";

import type { LiveListSource } from "../../../live/LiveList";
import { useOsConnection } from "../../../live/connection";
import { useLiveCollection } from "../../../live/useLiveCollection";
import { meshNodeFromRow } from "./rows";

// The Mesh section's one feed: every `v1:cluster:node` row, live.
//
// LIVE BECAUSE THE ROWS BROADCAST, and checked rather than assumed: there is
// no rule naming v1:cluster:node, but the `graph.node.{created,updated,
// deleted}.v1:cluster:*` wildcards in component/node/routing.go carry it, and
// since memql#5338 they reach every node the browser could be attached to. A
// node's report moves once a minute with its heartbeat, so the page follows
// the mesh without a refresh control -- the refresh appears only when the
// feed says it has fallen behind.
//
// THE HISTORY IS COLLAPSED IN THE SEED, for the reason
// apps/fleet/workbenches/useWorkbenches.ts gives: the row is append-only, and
// a collection folding an unordered read by id would keep an arbitrary row of
// a replica's lifetime. Events need no such care; they arrive newest-last.
//
// KEYED BY THE COLLECTION'S OWN RULE, deliberately: the seed and the fold must
// agree on a row's key, and the fold keys an event by the payload's own `id`
// whatever `rowId` a spec passes. Both seams carry the BARE id (the engine
// bare-ifies on egress), so the default rule is the one that keeps an update
// on the row it updates.

export interface MeshNodesState {
  /** Raw wire rows; null until the connection exists. Callers project them
   *  in `useLiveView`, because an arriving event is upserted as the raw row. */
  source: LiveListSource<Row> | null;
  state: LiveState;
  error: string;
  reseed: () => void;
}

export function useMeshNodes(): MeshNodesState {
  const connection = useOsConnection();
  const nodes = useLiveCollection<Row>(
    connection === null ? null : "cluster:mesh:nodes",
    (conn) => ({
      concept: Concepts.CLUSTER_NODE,
      seed: async (_cursor, signal) => {
        const result = await conn.query.clusterNodes({}, { signal });
        const newest = new Map<string, { createdAt: string; raw: Row }>();
        for (const raw of result.rows()) {
          const node = meshNodeFromRow(raw);
          if (node.id === "") continue;
          const held = newest.get(node.id);
          if (held === undefined || node.createdAt >= held.createdAt) {
            newest.set(node.id, { createdAt: node.createdAt, raw });
          }
        }
        return { rows: [...newest.values()].map((one) => one.raw), nextCursor: "" };
      },
      reread: async (rowId, signal) => {
        const row = await getRowByConceptAndId(conn.query, Concepts.CLUSTER_NODE, rowId, { signal });
        return (row as Row) ?? null;
      },
      inScope: (raw) => meshNodeFromRow(raw).id !== "",
      paged: false,
    }),
  );
  return {
    source: nodes.source,
    state: nodes.snapshot.state,
    error: nodes.snapshot.error,
    reseed: nodes.reseed,
  };
}
