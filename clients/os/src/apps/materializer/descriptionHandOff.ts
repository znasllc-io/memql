import { useEffect, useRef, useState } from "react";
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
export function useDescriptionHandOff(described: DescribedFile[], compositions: Row[]): DescriptionOutcome[] {
  const connection = useOsConnection();
  const begun = useRef(new Set<string>());
  const [outcomes, setOutcomes] = useState<DescriptionOutcome[]>([]);

  // The files whose composition is ready and whose hand-off has not begun.
  const waiting = described.flatMap((file) => {
    if (begun.current.has(file.compositionId)) return [];
    const row = compositions.find((candidate) => sameEntityId(rowString(candidate, "id"), file.compositionId));
    const fileId = row !== undefined && rowString(row, "status") === "ready" ? rowString(row, "outputFileId") : "";
    return fileId === "" ? [] : [{ file, fileId }];
  });
  const fileIds = waiting.map((entry) => entry.fileId).sort();

  // THE FILES ENTRIES OF THOSE FILES, LIVE. The key is the set of files, so a
  // file joining the set restarts the read -- and one that has begun leaves it.
  const entries = useLiveCollection<Row>(fileIds.length === 0 ? null : `materializer:files-entries:${fileIds.join(",")}`, (live) => ({
    concept: Concepts.LIBRARY_ARTIFACT,
    seed: async (_cursor, signal) => {
      const found = await Promise.all(fileIds.map((fileId) => live.query.libraryArtifactForFile({ fileId }, { signal })));
      return { rows: found.flatMap((result) => result.rows()), nextCursor: "" };
    },
    // A subscription is scoped by concept, and the read by file: without this a
    // new entry for ANY file would be taken for the one being waited on.
    inScope: (row) => fileIds.some((fileId) => sameEntityId(rowString(row, "sourceConceptRef"), fileId)),
    reread: async (rowId, signal) =>
      ((await getRowByConceptAndId(live.query, Concepts.LIBRARY_ARTIFACT, rowId, { signal })) as Row) ?? null,
    paged: false,
  }));

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null) return;
    for (const { file, fileId } of waiting) {
      const entry = entries.snapshot.rows.find((row) => sameEntityId(rowString(row, "sourceConceptRef"), fileId));
      const artifactId = entry === undefined ? "" : rowString(entry, "id");
      if (artifactId === "" || begun.current.has(file.compositionId)) continue;
      begun.current.add(file.compositionId);
      void handOffDescription(executeOver(query), {
        artifactId,
        statement: file.statement,
        sources: file.sources,
        requestId: file.requestId,
      }).then((result) =>
        setOutcomes((prev) => [
          ...prev,
          { compositionId: file.compositionId, failure: result.revisionFailure ?? "", unattached: result.unattached ?? [] },
        ]),
      );
    }
    // The set that is waiting is what the key names, and a change to it is a
    // change of key -- so the snapshot is the only other thing this reads.
  }, [connection, entries.snapshot, fileIds.join(",")]);

  return outcomes;
}
