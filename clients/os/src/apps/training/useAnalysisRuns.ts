import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { RUN_CONCEPT } from "./concepts";

// The analysis feed: ONE LiveCollection over the caller's work runs.
//
// ===========================================================================
// WHAT THIS ADDS THAT THE FILE ROW CANNOT SAY
// ===========================================================================
// A file row says `ready`. It cannot say whether "ready" means twelve
// passages were indexed or that the file was a photograph with nothing in it
// to read -- both end at `ready` with `embeddingStatus: "complete"`. The run's
// outcome is where that lives, along with how many passages became
// searchable and whether the summariser was reached at all.
//
// So this feed is not a second opinion about the file's state; it is the only
// account of what the pass actually did, and `stageOf` (rows.ts) folds the two
// with the FILE leading -- because the file row is written synchronously by
// the upload route and the run row by a detached goroutine, so a surface that
// waited for the run would show nothing for the first moments of every upload.
//
// `v1:work:run` declares the composite owner tier and carries broadcast
// routing rules, so this is live and admitted per owner on both paths.

export function useAnalysisRuns(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("training:runs", (connection) => ({
    concept: RUN_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.workRunsForOwner({}, { signal });
      // One page of the newest, matching the file feed beside it. A run
      // whose file is off the end of the file page has nothing to decorate.
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, RUN_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}
