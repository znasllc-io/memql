import type { OsAppSection } from "../../system/registry";

// The Bin's identity, and the one id the shell shares with it.

export const BIN_APP_ID = "bin";

/**
 * The dock droppable's id. Shared with `chrome/Desktop.tsx`, which must NOT
 * act on a drop that landed here: archiving is a write with a confirm and a
 * refusal to render, and both belong where the gesture ended rather than on
 * the surface it started from.
 */
export const BIN_DROPPABLE_ID = "bin";

/**
 * What a draggable hands the Bin when it lands there.
 *
 * Carried on the draggable's own `data` rather than looked up by id, because
 * the two things that can be dragged here live in different places -- a Files
 * row is in the app's feed, a desk icon is in the desktop document -- and the
 * dock holds neither. Passing the facts with the drag means the Bin can name
 * what it is about to archive without subscribing to anything.
 */
export interface BinDropPayload {
  /** The v1:library:artifact id to archive. "" makes the drop a no-op. */
  artifactId: string;
  /** What to call it in the confirm and the refusal. */
  name: string;
  /**
   * True for a Library FOLDER. The Bin refuses these: archiving a folder is a
   * recursive walk with a live count in its confirm, and that flow belongs to
   * the Files app which can see the tree. A dock icon cannot count what is
   * inside something, and a confirm that could not name the number would be
   * asking somebody to approve an amount of destruction it declined to state.
   */
  folder: boolean;
  /** The desk item to remove once the archive lands, or "" for a drag that
   *  did not come from a desk. */
  deskItemId: string;
}

/** The sections this app declares, in manifest order. Exported because the
 *  manifest and the title-bar gear must offer the same set. */
export const BIN_SECTIONS: OsAppSection[] = [
  { id: "items", name: "Bin" },
  // The app's slice of the cluster's logs (epic memql#4895): the lines it
  // tagged and the lines about the things it owns. Admin-floored because
  // every read on the log store is (spec L3), and this is the ONE section
  // whose floor is not this app's to choose.
  { id: "logs", name: "Logs", roles: { min: "admin" } },
  { id: "settings", name: "Settings" },
];
