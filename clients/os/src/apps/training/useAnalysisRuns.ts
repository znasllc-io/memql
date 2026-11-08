import { getRowByConceptAndId, sameEntityId, type Row } from "@znasllc-io/memql-sdk-core/client";
import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { RUN_CONCEPT } from "./concepts";
import { runFromRow, runBelongsHere } from "./rows";

/** Resolve latest analysis evidence only for files on the current worklist. */
export function useAnalysisRuns(fileIds: readonly string[]): LiveCollectionHandle<Row> {
  const ids = [...fileIds].sort();
  return useLiveCollection<Row>(ids.length ? `training:analyses:${JSON.stringify(ids)}` : null, connection => {
    const found = new Set<string>();
    return {
      concept: RUN_CONCEPT,
      seed: async (cursor, signal) => {
        if (!cursor) found.clear();
        const result = await connection.query.trainingAnalysisRunsForFiles({ fileIds: ids }, { cursor, signal });
        const rows = result.rows().filter(row => {
          const file = runFromRow(row).fileId;
          if (found.has(file)) return false;
          found.add(file);
          return true;
        });
        const complete = ids.every(id => [...found].some(file => sameEntityId(id, file)));
        return { rows, nextCursor: complete ? "" : result.meta()?.cursor ?? "" };
      },
      inScope: row => runBelongsHere(runFromRow(row)) && ids.some(id => sameEntityId(id, runFromRow(row).fileId)),
      reread: async (rowId, signal) => (await getRowByConceptAndId(connection.query, RUN_CONCEPT, rowId, { signal })) as Row | null,
    };
  });
}
