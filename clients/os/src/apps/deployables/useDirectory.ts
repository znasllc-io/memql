import { useEffect, useRef } from "react";
import { type Row } from "@znasllc-io/memql-sdk-core/client";
import { flatten } from "../../kit/rows";
import { usePagedCollection } from "../../live/useLiveCollection";
import { useGraphRevision } from "../../live/useGraphRevision";
import { useLiveView } from "../../live/liveView";
import { useDebouncedValue } from "../../logs/useDebouncedValue";
import { siteFromRow } from "./rows";
import { deploymentFromRow, packageFromRow } from "./packages/rows";
import type { DeployableListGroup, DeployableListRow, ListFilter } from "./list";

type Entry = { id: string; createdAt?: string; row?: Record<string, unknown>; group?: Record<string, unknown>; total?: number; archivedCount?: number; provenanceError?: string };
const META = "directory-metadata";
function deployable(entry: Entry): DeployableListRow | null {
  const row = entry.row;
  if (!row) return null;
  return { ...row, site: row.site ? siteFromRow(row.site as Row) : null, pkg: row.pkg ? packageFromRow(row.pkg as Row) : null, parked: row.parked ? deploymentFromRow(row.parked as Row) : null } as unknown as DeployableListRow;
}
function source(entry: Entry): DeployableListGroup | null {
  const group = entry.group;
  if (!group) return null;
  return { ...group, pkg: group.pkg ? packageFromRow(group.pkg as Row) : null, rows: [] } as unknown as DeployableListGroup;
}
export function useDeployableDirectory(resource: "sources" | "deployables", filter: ListFilter, archived: boolean, enabled: boolean) {
  const search = useDebouncedValue(filter.search.trim(), 250);
  const args = { resource, search: search || undefined, archived, ...(resource === "deployables" ? { kind: filter.kind || undefined, status: filter.status || undefined, accountId: filter.accountId === "all" ? undefined : filter.accountId, source: filter.source || undefined } : {}) };
  const key = JSON.stringify(args);
  const feed = usePagedCollection<Entry>(enabled ? `deployables:directory:${key}` : null, connection => ({
    concept: "v1:platform:site", actions: [],
    seed: async (cursor, signal) => {
      const result = await connection.query.deployableDirectoryPage({ ...args, cursor: cursor || undefined }, { signal });
      const first = result.rows()[0];
      if (!first) throw new Error("The directory returned no result.");
      const row = flatten(first);
      return { rows: [{ id: META, total: Number(row.total ?? 0), archivedCount: Number(row.archivedCount ?? 0), provenanceError: typeof row.provenanceError === "string" ? row.provenanceError : "" }, ...(Array.isArray(row.entries) ? row.entries as Entry[] : [])], nextCursor: typeof row.nextCursor === "string" ? row.nextCursor : "" };
    },
  }));
  const revision = useGraphRevision(["v1:platform:site", "v1:platform:package", "v1:platform:packageDeployment", "v1:platform:sourceCredential", "v1:platform:sourceConnection"], enabled, { externalRecovery: true });
  const previous = useRef({ key, revision });
  useEffect(() => { const before = previous.current; previous.current = { key, revision }; if (enabled && before.key === key && before.revision !== revision) feed.reseed(); }, [key, revision, enabled, feed.reseed]);
  const flat = useLiveView<Entry, DeployableListRow>(feed.source, key, rows => rows.map(deployable).filter((row): row is DeployableListRow => row !== null));
  const sources = useLiveView<Entry, DeployableListGroup>(feed.source, key, rows => rows.map(source).filter((row): row is DeployableListGroup => row !== null));
  const metadata = feed.snapshot.rows.find(row => row.id === META);
  return { ...feed, flat, sources, total: metadata?.total ?? 0, archivedCount: metadata?.archivedCount ?? 0, provenanceError: metadata?.provenanceError ?? "" };
}
