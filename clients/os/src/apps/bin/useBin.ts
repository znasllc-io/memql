import { getRowByConceptAndId, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";
import { useSession } from "../../chrome/access";
import { flatten } from "../../kit/rows";
import { useLiveCollection, usePagedCollection } from "../../live/useLiveCollection";
import { ARTIFACT_CONCEPT, FOLDER_CONCEPT } from "../files/concepts";
import { artifactFromRow, folderFromRow, isContentKind } from "../files/rows";

/** The archive reads only searched ten-row pages. Folder names are a separate
 * structural directory so "was filed in" stays accurate beyond loaded pages. */
export function useBinFeeds(search = "", enabled = true) {
  const viewer = useSession().access?.userId ?? "";
  const needle = search.trim();
  const key = JSON.stringify(needle);
  const artifacts = usePagedCollection<Row>(enabled ? `bin:artifacts:${key}` : null, connection => ({
    concept: ARTIFACT_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.libraryArtifactsBrowse({ archived: true, search: needle || undefined }, { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (id, signal) => await getRowByConceptAndId(connection.query, ARTIFACT_CONCEPT, id, { signal }) as Row | null,
    comparePageRows: (a, b) => String(b["createdAt"] ?? "").localeCompare(String(a["createdAt"] ?? "")) || String(b["id"]).localeCompare(String(a["id"])),
    inScope: raw => {
      const row = artifactFromRow(raw);
      return row.ownerUserId === viewer && row.archived && isContentKind(row.kind) && (!needle || [row.title, row.summary, row.id, row.producedByWorkerName, ...row.labels].some(value => value.toLowerCase().includes(needle.toLowerCase())));
    },
  }));
  const folders = usePagedCollection<Row>(enabled ? `bin:folders:${key}` : null, connection => ({
    concept: FOLDER_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.libraryArchivedFoldersBrowse({ search: needle || undefined }, { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (id, signal) => await getRowByConceptAndId(connection.query, FOLDER_CONCEPT, id, { signal }) as Row | null,
    comparePageRows: (a, b) => String(b["createdAt"] ?? "").localeCompare(String(a["createdAt"] ?? "")) || String(b["id"]).localeCompare(String(a["id"])),
    inScope: raw => {
      const row = folderFromRow(raw);
      return rowString(flatten(raw), "ownerUserId") === viewer && row.archived && !row.deleted && (!needle || row.name.toLowerCase().includes(needle.toLowerCase()));
    },
  }));
  const names = useLiveCollection<Row>(enabled ? "bin:folder-names" : null, connection => ({
    concept: FOLDER_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.libraryArchivedFolders({}, { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (id, signal) => await getRowByConceptAndId(connection.query, FOLDER_CONCEPT, id, { signal }) as Row | null,
    comparePageRows: (a, b) => String(b["createdAt"] ?? "").localeCompare(String(a["createdAt"] ?? "")) || String(b["id"]).localeCompare(String(a["id"])),
    inScope: raw => { const row = folderFromRow(raw); return rowString(flatten(raw), "ownerUserId") === viewer && row.archived && !row.deleted; },
  }));
  return { artifacts, folders, names };
}
