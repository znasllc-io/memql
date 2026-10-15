import { useEffect, useMemo, useRef, useState } from "react";
import {
  Concepts,
  getRowByConceptAndId,
  handOffDescription,
  rowString,
  sameEntityId,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { useLiveCollection } from "../../live/useLiveCollection";
import { modelsOf } from "./rows";
import { executeOver, type DescribedFile } from "./actions";

// Handing a described Markdown file's description to the Library, as the person.
//
// ===========================================================================
// WHY THE OS DOES THIS AND NOT THE CLUSTER
// ===========================================================================
// A Markdown file asked for by describing it is filed EMPTY, with the
// description kept on its composition. Writing it is the Library's revision
// workflow, and the Library refuses a revision request made from inside a work
// run -- a running job must not request a revision on a person's behalf. So the
// person's own client sends the description, once the file has an entry in the
// Library: `handOffDescription` in the SDK posts it as a document-level comment
// (the library_file sources attached where the Library accepts them) and asks
// for the revision. It never throws; the file exists and is good whatever
// happens next.
//
// ===========================================================================
// FOLLOWING THE FILE IS THE FEEDS' JOB, NOT A TIMER'S
// ===========================================================================
// `followComposition` polls, and that is right for an editor with no feed. The
// OS has one: the composition feed says when the file is ready, and the
// Library's artifact feed says when its Files entry exists -- a new file's
// entry is written after the file, by an automation, so it may trail the
// composition by a moment. Both are subscriptions, so nothing here reads on an
// interval.
//
// ===========================================================================
// EXACTLY ONCE PER COMPOSITION
// ===========================================================================
// The hand-off is keyed by composition and begins at most once in the life of
// this window. The ids it derives from the request id make a repeat harmless in
// the Library as well -- a second call finds the first one's receipts -- but a
// second call is still a second round trip and a second chance to show the
// person a failure about a request that went through.
//
// WHAT IT CANNOT DO is outlive the window. If the person closes it between "the
// file is ready" and "the description is sent", the composition keeps the
// statement and the editor's empty page offers Describe document.

export interface DescriptionOutcome {
  compositionId: string;
  /** Why the description could not be handed on, in the cluster's words; "" when it was. */
  failure: string;
  /** The references that went without being attached, by the names the person gave them. */
  unattached: string[];
}

/**
 * Hands each described file on once its composition is ready and its Files
 * entry exists, and reports what became of each.
 */
export interface FileEntry {
  artifactId: string;
  title: string;
}

/** One narrow live read shared by description hand-off and automatic opening. */
export function useFilesEntries(
  ids: readonly string[],
): ReadonlyMap<string, FileEntry> {
  const fileIds = [...new Set(ids)].filter(Boolean).sort();
  const entries = useLiveCollection<Row>(
    fileIds.length === 0
      ? null
      : `materializer:files-entries:${fileIds.join(",")}`,
    (live) => ({
      concept: Concepts.LIBRARY_ARTIFACT,
      seed: async (_cursor, signal) => {
        const found = await Promise.all(
          fileIds.map((fileId) =>
            live.query.libraryArtifactForFile({ fileId }, { signal }),
          ),
        );
        return {
          rows: found.flatMap((result) => result.rows()),
          nextCursor: "",
        };
      },
      inScope: (row) =>
        fileIds.some((id) =>
          sameEntityId(rowString(row, "sourceConceptRef"), id),
        ),
      reread: async (rowId, signal) =>
        ((await getRowByConceptAndId(
          live.query,
          Concepts.LIBRARY_ARTIFACT,
          rowId,
          { signal },
        )) as Row) ?? null,
      paged: false,
    }),
  );
  return useMemo(
    () =>
      new Map(
        fileIds.flatMap((id) => {
          const row = entries.snapshot.rows.find((row) =>
            sameEntityId(rowString(row, "sourceConceptRef"), id),
          );
          return row
            ? [
                [
                  id,
                  {
                    artifactId: rowString(row, "id"),
                    title: rowString(row, "title"),
                  },
                ] as const,
              ]
            : [];
        }),
      ),
    [entries.snapshot, fileIds.join(",")],
  );
}

export function useDescriptionHandOff(
  described: DescribedFile[],
  compositions: Row[],
  stopped: ReadonlySet<string>,
  entries: ReadonlyMap<string, FileEntry>,
): DescriptionOutcome[] {
  const connection = useOsConnection();
  const begun = useRef(new Set<string>());
  const [outcomes, setOutcomes] = useState<DescriptionOutcome[]>([]);
  useEffect(() => {
    if (!connection) return;
    for (const file of described) {
      if (begun.current.has(file.compositionId)) continue;
      if (stopped.has(file.compositionId)) continue;
      const row = compositions.find((row) =>
        sameEntityId(rowString(row, "id"), file.compositionId),
      );
      if (!row || rowString(row, "status") !== "ready") continue;
      // A file's inputs can change between the client's prediction and the
      // server's read. Never ask for a second draft of a file a model wrote.
      if (modelsOf(row).length || row.provenanceEmbedded !== false) continue;
      const entry = entries.get(rowString(row, "outputFileId"));
      if (!entry?.artifactId) continue;
      begun.current.add(file.compositionId);
      void handOffDescription(executeOver(connection.query), {
        artifactId: entry.artifactId,
        statement: file.statement,
        sources: file.sources,
        requestId: file.requestId,
      }).then((result) =>
        setOutcomes((previous) => [
          ...previous,
          {
            compositionId: file.compositionId,
            failure: result.revisionFailure ?? "",
            unattached: result.unattached ?? [],
          },
        ]),
      );
    }
  }, [connection, described, compositions, stopped, entries]);
  return [
    ...outcomes,
    ...described
      .filter(
        (file) =>
          stopped.has(file.compositionId) &&
          !begun.current.has(file.compositionId),
      )
      .map((file) => ({
        compositionId: file.compositionId,
        failure: "Stopped before the description was sent.",
        unattached: [],
      })),
  ];
}
