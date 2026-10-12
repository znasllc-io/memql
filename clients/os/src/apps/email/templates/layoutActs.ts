import type { ActSpec } from "../acts";
import { layoutIsArchived, type LayoutRow } from "../rows";

// WHAT A LAYOUT'S PAGE OFFERS (DESIGN.md rule 12; Ruling 24), as a pure table.
//
//   built-in             nothing: it ships with the renderer and has no lifecycle
//   organization active  Archive
//   archived             Restore
//
// A layout is saved from a finished email in the editor, never here, so the
// only things that change one here are putting it away and bringing it back.
// Archiving takes it out of the editor's layout choices and leaves every email
// already made from it as it is, so it does not ask first.

const ARCHIVE: ActSpec = { id: "archive", label: "Archive", text: true };
const RESTORE: ActSpec = { id: "restore", label: "Restore", tone: "primary" };

export function layoutActs(l: LayoutRow): ActSpec[] {
  if (l.kind === "builtin") return [];
  return layoutIsArchived(l) ? [RESTORE] : [ARCHIVE];
}
