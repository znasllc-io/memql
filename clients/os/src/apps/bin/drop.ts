import { BIN_DROPPABLE_ID, type BinDropPayload } from "./concepts";

// What a drop on the Bin means, as a function.
//
// Pulled out of the dock's drag handler so it can be checked without a
// pointer, a DndContext or a browser -- which matters because the interesting
// cases are the two REFUSALS, and both of them are "nothing happened" from the
// outside. A folder that quietly did nothing and a folder that was archived
// look identical in a test that only asserts on what rendered.

export type BinDropOutcome =
  | { kind: "ignore" }
  | { kind: "refuseFolder"; name: string }
  | { kind: "confirm"; drop: BinDropPayload }
  | { kind: "archive"; drop: BinDropPayload };

/**
 * Decide what a completed drag means for the Bin.
 *
 * `overId` is the droppable the pointer finished on; anything but the Bin's is
 * somebody else's business. A payload with no artifact id is a drag of
 * something the Bin cannot act on -- a window, a widget, an upload still in
 * flight -- and is ignored rather than refused, because refusing implies the
 * gesture was aimed here and it was not.
 *
 * A FOLDER IS REFUSED, and this is the one decision worth stating. Archiving a
 * folder is a recursive walk whose confirm names the live count of everything
 * inside it, and the Files app owns that flow because it is the surface that
 * can see the tree. A dock icon cannot count what is inside something, and a
 * confirm that could not name the number would be asking somebody to approve
 * an amount of destruction it had declined to state.
 */
export function decideBinDrop(
  overId: string | null,
  payload: BinDropPayload | null,
  confirmBeforeArchive: boolean,
): BinDropOutcome {
  if (overId !== BIN_DROPPABLE_ID) return { kind: "ignore" };
  if (payload === null || payload.artifactId.trim() === "") return { kind: "ignore" };
  if (payload.folder) return { kind: "refuseFolder", name: payload.name };
  return confirmBeforeArchive ? { kind: "confirm", drop: payload } : { kind: "archive", drop: payload };
}
