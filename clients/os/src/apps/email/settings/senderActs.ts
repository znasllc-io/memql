import type { ActSpec } from "../acts";
import { senderIsRetired, type SenderIdentityRow } from "../rows";

// WHAT A SENDER'S PAGE OFFERS (DESIGN.md rule 12), as a pure table.
//
// A sender is put away, never deleted: past campaigns name it and its sending
// reputation is kept against its address, so there is no delete anywhere and
// the word for putting it away is Archive. Archiving asks first, because the
// engine REFUSES a campaign that names an archived sender rather than sending
// from somewhere else, and a person archiving one should learn that from the
// question, not from a refused send tomorrow.
//
//   active          Archive (asks)           Edit
//   asking          Keep                     Archive (does it)
//   editing         Cancel                   Save
//   archived                                 Restore
//
// An archived sender is not editable: it comes back first. The ids differ where
// the label does not -- the Archive that asks and the Archive that does are two
// acts -- so the page can tell them apart.

/** What a sender's page offers, given what is being done to it. */
export function senderActs(sender: SenderIdentityRow, editing: boolean, asking: "" | "archive"): ActSpec[] {
  if (senderIsRetired(sender)) return [{ id: "restore", label: "Restore", tone: "primary" }];
  if (editing) return [{ id: "cancel", label: "Cancel", text: true }, { id: "save", label: "Save", tone: "primary" }];
  if (asking === "archive") return [{ id: "keep", label: "Keep", text: true }, { id: "archive-confirmed", label: "Archive", tone: "danger" }];
  return [{ id: "archive", label: "Archive", text: true }, { id: "edit", label: "Edit", text: true }];
}
