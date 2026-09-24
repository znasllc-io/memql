import { getRowByConceptAndId, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../../kit/rows";
import { useLiveCollection, type LiveCollectionHandle } from "../../../live/useLiveCollection";
import { DEPLOYMENT_CONCEPT, PENDING_DEPLOYMENT_STATUSES } from "./rows";

// One bounded population of current work and review gates, never full timelines.
// Keep seed and event scope identical so leaving/reopening survives broadcasts,
// reconnects, and a different browser or BFF replica.
export function usePendingDeployments(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("deployables:pendingDeployments", (connection) => ({
    concept: DEPLOYMENT_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.packageDeploymentsPending({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    inScope: (row) => PENDING_DEPLOYMENT_STATUSES.has(rowString(flatten(row), "status")),
    // The re-read a `payload_omitted` event lands on, and the collection's
    // gap recovery -- the same seam every other feed in this app wires.
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, DEPLOYMENT_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}
