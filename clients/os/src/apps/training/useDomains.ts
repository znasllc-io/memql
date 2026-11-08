import { useMemo } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useLiveCollection } from "../../live/useLiveCollection";
import { domainMetaFromRow, type DomainMeta, type DomainRollup } from "./rows";

// Domain configuration is a compact index shared by the browser, upload picker
// and review queue. The engine folds authorized chunks before sending one row
// per domain; the browser never downloads the whole chunk population for counts.
// Knowledge writes are not broadcast. Shared session reads refresh on resume
// and retain the last answer when the app closes, without claiming a live feed.
export interface DomainsFeed {
  rollups: DomainRollup[];
  domains: Map<string, DomainMeta>;
  state: "loading" | "ready" | "error";
  error: string;
  readAt: Date | null;
  reload: () => void;
}

export function useDomains(enabled = true): DomainsFeed {
  const feed = useLiveCollection<Row>(enabled ? "training:domain-index" : null, connection => {
    let lastCatalog: Row[] = [];
    return {
    concept: "v1:knowledge:knowledgeDomain", actions: [], paged: false,
    seed: async (_cursor, signal) => {
      const [result, catalog] = await Promise.all([
        connection.query.knowledgeTrainingDomainRollups({}, { signal }),
        connection.query.knowledgeDomainsAll({}, { signal }).catch(() => null),
      ]);
      const index = result.single();
      if (!index || !Array.isArray(index.rollups)) throw new Error("The domain index response is incomplete.");
      if (catalog) lastCatalog = catalog.rows();
      return { rows: [{ ...index, domains: lastCatalog, id: "index", readAt: new Date().toISOString() }], nextCursor: "" };
    },
  }; });
  const index = feed.snapshot.rows[0];
  const rollups = (index?.rollups ?? []) as DomainRollup[];
  const domains = useMemo(() => {
    const out = new Map<string, DomainMeta>();
    for (const row of (index?.domains ?? []) as Row[]) {
      const meta = domainMetaFromRow(row);
      if (meta.id) out.set(meta.id, meta);
    }
    return out;
  }, [index]);
  return {
    rollups, domains,
    state: feed.snapshot.error ? "error" : index ? "ready" : "loading",
    error: feed.snapshot.error,
    readAt: typeof index?.readAt === "string" ? new Date(index.readAt) : null,
    reload: feed.reseed,
  };
}
