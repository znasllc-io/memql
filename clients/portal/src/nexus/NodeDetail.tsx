import type { ReactNode } from "react";

import { ButtonLink } from "../ui";
import { RowDetailDialog } from "../components/RowDetailDialog";
import { useRowDetail } from "../cluster/useConceptRows";
import { artifactPath } from "../artifacts/urls";
import { conceptRowPath } from "../concepts/urls";
import { constructsPath } from "./urls";
import type { LayoutResult } from "./scene/layout";

// A node, opened.
//
// The click target on the canvas is a URL (design D5), and this is what that
// URL renders: the portal's own RowDetailDialog over a FRESH AUTHORITATIVE
// READ of the row, not the copy the scene is holding.
//
// That freshness is the point rather than a detail. The map's copy is as old
// as the last event that touched it, and the detail pane is precisely where
// an operator goes to check a current value -- the same reasoning
// useRowDetail's own header gives for not reading out of the paged rows.
// It is also what makes the read AUTHORIZED: the dialog shows what the
// cluster will hand this caller for this row right now, not what a CDC
// payload once said.
//
// `you` and a cluster node open nothing, because neither has a row: `you` is
// the caller (the /me surface answers that question) and a cluster is a
// drawing device standing in for a phase.

export function NodeDetail({
  scene,
  nodeId,
  onClose,
}: {
  scene: LayoutResult;
  nodeId: string;
  onClose: () => void;
}): ReactNode {
  const node = scene.nodes.get(nodeId);
  const conceptId = node?.conceptId ?? "";
  const rowId = node?.rowId ?? "";
  const detail = useRowDetail(conceptId, rowId);

  // A node id from a URL that names nothing in this scene: a stale link, or a
  // node that has not arrived yet at the moment the scrubber sits at. Closing
  // is the honest response -- a dialog reading "no such node" over a map that
  // shows the goal fine is worse than the map.
  if (node === undefined || conceptId === "" || rowId === "") return null;

  return (
    <RowDetailDialog
      open
      onClose={onClose}
      rowId={rowId}
      row={detail.row}
      loading={detail.loading}
      error={detail.error}
      missing={detail.missing}
      actions={
        <div className="flex flex-wrap gap-2">
          {/* The portal's existing doors, per design 4.4. Links rather than
              buttons: each is a destination, and a person may want to open
              one in a new tab from a map they do not want to leave. */}
          <ButtonLink size="xs" href={conceptRowPath(conceptId, rowId)}>
            Open in the concept browser
          </ButtonLink>
          {node.kind === "artifact" ? (
            <ButtonLink size="xs" href={artifactPath(rowId)}>
              Open in the Library
            </ButtonLink>
          ) : null}
          {node.kind === "construct" || node.kind === "bundle" ? (
            <ButtonLink size="xs" href={constructsPath(planIdFromScene(scene))}>
              Open in Constructs
            </ButtonLink>
          ) : null}
        </div>
      }
    />
  );
}

// The plan id is on the goal node, which every scene has: the layout always
// places `goal`, and its rowId IS the plan. Reading it from there rather than
// threading a prop keeps this component's contract to "a scene and a node".
function planIdFromScene(scene: LayoutResult): string {
  return scene.nodes.get("goal")?.rowId ?? "";
}

// Exported so the pages can decide whether a node id is worth routing to at
// all -- clicking `you` should not push a history entry that renders nothing.
export function nodeOpensADialog(scene: LayoutResult, nodeId: string): boolean {
  const node = scene.nodes.get(nodeId);
  return node !== undefined && node.conceptId !== "" && node.rowId !== "";
}
