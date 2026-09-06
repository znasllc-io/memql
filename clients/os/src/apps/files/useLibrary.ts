import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { ARTIFACT_CONCEPT, COMPOSITION_CONCEPT, FILE_CONCEPT, FOLDER_CONCEPT } from "./concepts";

// The Files feeds: TWO retained collections at the app root -- the Library's
// index rows and its folder rows -- shared by the browse, the tree, the
// inspector and the archive walk. The Deployables rule holds: a second
// subscription over either concept would be one that can disagree.
//
// ===========================================================================
// THE SEED INCLUDES ARCHIVED ROWS, AND THAT IS THE POINT OF PICKING IT
// ===========================================================================
// `libraryArtifacts` carries `archived != true`, so a browse seeded from it
// could only ever show the archived rows that happened to flip while this
// window was open -- the toggle would answer from a population that depends
// on when you looked. `libraryArtifactsByLens(lens: "artifact")` deliberately
// carries no archive conjunct (its header says so: a caller who asked for one
// lens asked about that facet), and the artifact lens IS this app's whole
// population -- file / document / generated_output are its exact members,
// while every records kind lives on the record lens. So one read seeds the
// complete truth and "show archived" stays a pure client-side fold.
//
// Both seeds WALK THE CURSOR inside the seed (the collections are paged), so
// a Library past one page arrives whole without tearing the subscription
// down. Records-lens rows still arrive as EVENTS -- the subscription is
// scoped by concept, not by lens -- and the projection drops them
// (`isContentKind`), which the conformance tests pin.

export interface LibraryFeeds {
  artifacts: LiveCollectionHandle<Row>;
  folders: LiveCollectionHandle<Row>;
  /**
   * The backing file rows, for the ORIGIN LINK STATES (epic memql#4783).
   *
   * A THIRD CONCEPT, which is not a violation of the one-feed rule: that rule
   * is per CONCEPT -- two subscriptions over the same one are free to disagree
   * about what the cluster holds, and two concepts cannot.
   *
   * It has to be its own feed rather than a field on the index, and the reason
   * is a cycle. `linkState` lives on v1:library:file and is deliberately NOT
   * promoted to v1:library:artifact: forwarding it would need an automation on
   * `node.updated` for the file, and the artifact->file archive automation
   * already runs the other way -- the pair closes a loop where each write
   * publishes an event the other subscribes to, which that automation's own
   * header warns about by name.
   *
   * `v1:library:file` broadcasts created AND updated (component/node/routing.go),
   * so a state the cockpit's verify lane reports lands on the row somebody is
   * looking at, live, with no engine change.
   */
  files: LiveCollectionHandle<Row>;
  /**
   * The compositions (epic memql#4981, #4983) -- what the Materializer made.
   *
   * A FIFTH CONCEPT, and the one-feed rule is per CONCEPT: what must never
   * happen is two subscriptions over the same one, free to disagree. Retained
   * at the app root like its four siblings rather than inside the place,
   * because `useLiveCollection` retains inside an effect and a section switch
   * would otherwise tear it down and re-seed.
   *
   * IT HAS TO BE THE WHOLE SET. There is no server-side "artifacts that are
   * outputs" read -- the join is `composition.outputFileId` against each
   * artifact's backing file -- so the place cannot know its own population
   * from anything narrower. The same feed answers the inspector's one line,
   * which is why Files makes no `compositionForOutputFile` call: a second
   * read of one row is a second answer free to disagree with this one.
   */
  compositions: LiveCollectionHandle<Row>;
}

export function useLibraryFeeds(): LibraryFeeds {
  const artifacts = useLiveCollection<Row>("files:artifacts", (connection) => ({
    concept: ARTIFACT_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.libraryArtifactsByLens(
        { lens: "artifact" },
        { signal, ...(cursor !== "" ? { cursor } : {}) },
      );
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, ARTIFACT_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
  }));

  const folders = useLiveCollection<Row>("files:folders", (connection) => ({
    concept: FOLDER_CONCEPT,
    // ONE subscription, TWO reads in the seed (epic memql#4842, #4846).
    // `libraryFolders` carries `archived != true`, and the Bin place has
    // to list archived folders -- so the seed walks the live read and then
    // the archived one into a single population. Still one collection and
    // one subscription over the concept (the Deployables rule is about
    // subscriptions that can disagree, not about how a seed assembles the
    // complete truth); the tree fold keeps dropping archived folders
    // client-side exactly as before.
    seed: async (_cursor, signal) => {
      const rows: Row[] = [];
      for (const read of ["libraryFolders", "libraryArchivedFolders"] as const) {
        let cursor = "";
        for (;;) {
          const result = await connection.query[read](
            {},
            { signal, ...(cursor !== "" ? { cursor } : {}) },
          );
          rows.push(...result.rows());
          cursor = result.meta()?.cursor ?? "";
          if (cursor === "") break;
        }
      }
      return { rows, nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, FOLDER_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
  }));

  const files = useLiveCollection<Row>("files:files", (connection) => ({
    concept: FILE_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.libraryFilesForOwner(
        {},
        { signal, ...(cursor !== "" ? { cursor } : {}) },
      );
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, FILE_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
  }));

  const compositions = useLiveCollection<Row>("files:compositions", (connection) => ({
    concept: COMPOSITION_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.compositions(
        {},
        { signal, ...(cursor !== "" ? { cursor } : {}) },
      );
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, COMPOSITION_CONCEPT, rowId, {
        signal,
      });
      return (row as Row) ?? null;
    },
  }));

  return { artifacts, folders, files, compositions };
}
