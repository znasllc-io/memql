import { createContext, useContext, type ReactNode } from "react";
import { getRowByConceptAndId, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";
import { useSession } from "../../chrome/access";
import { accessAdmits } from "../../system/registry";
import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { flatten } from "../../kit/rows";
import { useDemandedRead, useReadDemand } from "../../kit/readDemand";

export interface ExternalConnection { id: string; ownerUserId: string; provider: string; resourceId: string; label: string; status: string }
const CONCEPT = "v1:platform:externalConnection";
const Context = createContext<{ feed: LiveCollectionHandle<Row>; retain: () => () => void } | null>(null);
export function SharedExternalConnectionsProvider({ children }: { children: ReactNode }) {
  useSession();
  const demand = useReadDemand();
  const feed = useFeed(demand.requested && accessAdmits("app:settings/connections"));
  return <Context.Provider value={{ feed, retain: demand.retain }}>{children}</Context.Provider>;
}
function useFeed(enabled: boolean) {
  const { access } = useSession();
  const viewer = access?.userId ?? "";
  return useLiveCollection<Row>(enabled && viewer ? `connections:external:${viewer}` : null, connection => ({
    concept: CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.executeNamed("externalConnectionsMine", "query externalConnectionsMine()", { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (id, signal) => await getRowByConceptAndId(connection.query, CONCEPT, id, { signal }) as Row ?? null,
    paged: false,
  }));
}
export function useExternalConnections() {
  const shared = useContext(Context);
  useDemandedRead(shared?.retain);
  const own = useFeed(shared === null);
  const feed = shared?.feed ?? own;
  const { access } = useSession();
  const rows = feed.snapshot.rows.map(raw => {
    const row = flatten(raw);
    return Object.fromEntries(["id", "ownerUserId", "provider", "resourceId", "label", "status"].map(key => [key, rowString(row, key)])) as unknown as ExternalConnection;
  }).filter(row => row.ownerUserId === access?.userId && row.status === "active");
  return { ...feed, rows };
}
