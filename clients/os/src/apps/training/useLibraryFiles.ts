import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";
import { usePagedCollection, type PagedCollectionHandle } from "../../live/useLiveCollection";
import { FILE_CONCEPT } from "./concepts";
import { fileBelongsHere, fileFromRow } from "./rows";

/** Recent files are a paged worklist. The full library is never a seed. */
export function useLibraryFiles(enabled = true, search = ""): PagedCollectionHandle<Row> {
  return usePagedCollection<Row>(enabled ? `training:files:${search}` : null, connection => ({
    concept: FILE_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.trainingFilesBrowse({ search }, { cursor, signal });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    inScope: raw => {
      const file = fileFromRow(raw);
      return fileBelongsHere(file) && (!search || `${file.name}\n${file.summary}`.toLowerCase().includes(search.toLowerCase()));
    },
    reread: async (rowId, signal) => (await getRowByConceptAndId(connection.query, FILE_CONCEPT, rowId, { signal })) as Row | null,
  }));
}
