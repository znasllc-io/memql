import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { ARTIFACT_CONCEPT, FOLDER_CONCEPT } from "../files/concepts";

// The Bin's feeds: the caller's archived index rows and archived folders.
//
// ===========================================================================
// TWO DEDICATED READS, WHERE FILES FOLDS ONE WHOLE POPULATION
// ===========================================================================
// The Files browse seeds from `libraryArtifactsByLens(lens: "artifact")` --
// which deliberately carries no archive conjunct -- because its show-archived
// TOGGLE has to answer from a set that does not depend on when you looked. The
// reasoning inverts here. The Bin IS the archived set; nothing in this window
// ever shows anything else, and seeding it from the whole Library would pull
// every row somebody owns in order to render the few they threw away.
//
// The feed is still LIVE, because a subscription is scoped by CONCEPT rather
// than by query: an archive somewhere else arrives as an update on
// v1:library:artifact, the fold admits it, and the row rises in the Bin at the
// moment it leaves Files. `component/node/routing.go` broadcasts created and
// updated for artifact, file and folder alike, so all of that works with no
// engine change.
//
// ===========================================================================
// A THIRD FEED, AND IT IS NOT A VIOLATION OF THE ONE-FEED RULE
// ===========================================================================
// The rule the Deployables app states is per CONCEPT, not per app: what must
// never happen is two subscriptions over the SAME concept, free to disagree
// about what the cluster holds. Two concepts cannot disagree, because they
// describe different things.
//
// The Bin does NOT retain a feed over v1:library:file. A file row is read on
// DEMAND when somebody selects an item, because its size, its origin path and
// its link state are detail-panel facts and keeping every archived file's row
// live would subscribe this window to the analysis churn of the whole Library
// to render one panel.

export interface BinFeeds {
  artifacts: LiveCollectionHandle<Row>;
  folders: LiveCollectionHandle<Row>;
}

export function useBinFeeds(): BinFeeds {
  const artifacts = useLiveCollection<Row>("bin:artifacts", (connection) => ({
    concept: ARTIFACT_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.libraryArchivedArtifacts(
        {},
        { signal, ...(cursor !== "" ? { cursor } : {}) },
      );
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, ARTIFACT_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
  }));

  const folders = useLiveCollection<Row>("bin:folders", (connection) => ({
    concept: FOLDER_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.libraryArchivedFolders(
        {},
        { signal, ...(cursor !== "" ? { cursor } : {}) },
      );
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, FOLDER_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
  }));

  return { artifacts, folders };
}
