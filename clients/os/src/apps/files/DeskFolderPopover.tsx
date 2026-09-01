import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { LiveList, ProvenanceDot } from "../../kit";
import { useMachines } from "../../live/machines";
import { useLiveView } from "../../live/liveView";
import { useLiveCollection } from "../../live/useLiveCollection";
import { VSCODE_NO_ANSWER_MESSAGE } from "../../items/vscode";
import { ARTIFACT_CONCEPT } from "./concepts";
import { artifactFingerprint, artifactFromRow, artifactName, fileStory, isContentKind, type ArtifactRow } from "./rows";

// A desk folder's popover: a LIVE view of the Library folder it points at
// (design D4). The collection is retained by THIS component and by nothing
// else on the desk, so the desk stays subscription-free until a popover
// opens and the subscription unmounts with it -- the release linger absorbs
// a close-and-reopen. Deliberately its own collection rather than the Files
// window's: the desk must work with no window open, and the app root's feed
// dies with its window.
//
// It lives in apps/files rather than in chrome because it is a Files surface
// the shell mounts: the projections, the story and the cue contract are the
// app's, and a second copy of them beside the chrome would be one that
// drifts.

export function DeskFolderPopover({
  folderId,
  noAnswerFor,
  onOpen,
}: {
  folderId: string;
  noAnswerFor: string | null;
  /** Fire the VS Code handoff; `anchor` keys the no-answer sentence back to
   *  the row that asked. */
  onOpen: (artifactId: string, anchor: string) => void;
}) {
  const artifacts = useLiveCollection<Row>(`desk:folder:${folderId}`, (connection) => ({
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
  const { presence } = useMachines();

  const view = useLiveView<Row, ArtifactRow>(artifacts.source, `desk:folder:${folderId}`, (rows) =>
    rows
      .map(artifactFromRow)
      .filter((r) => r.id !== "" && isContentKind(r.kind) && !r.archived && r.folderId === folderId)
      .sort((a, b) => b.createdAt.localeCompare(a.createdAt) || b.id.localeCompare(a.id)),
  );

  return (
    <div className="os-folder-popover" role="group" aria-label="Folder contents">
      <LiveList<ArtifactRow>
        source={view}
        rowId={(r) => r.id}
        fingerprint={artifactFingerprint}
        label="Folder contents"
        emptyText="Empty. Drop a file on the folder to file it here."
        renderRow={(row) => {
          const anchor = `pop:${row.id}`;
          const story = fileStory(
            row,
            row.producedByWorkerId ? presence(row.producedByWorkerId) : null,
          );
          return (
            <div className="os-folder-entry">
              <button
                type="button"
                className="os-folder-entry-open"
                title={story.sentence || undefined}
                onDoubleClick={() => onOpen(row.id, anchor)}
              >
                <ProvenanceDot tone={story.tone} />
                <span>{artifactName(row)}</span>
              </button>
              {noAnswerFor === anchor ? (
                <p className="os-caption" role="alert">
                  {VSCODE_NO_ANSWER_MESSAGE}
                </p>
              ) : null}
            </div>
          );
        }}
      />
    </div>
  );
}
