// A route being composed: its chain edits and the acts its state allows.
//
// Pure, so the composer's two promises can be asserted without a DOM: an edit
// never mutates what it was given (the baseline a Cancel returns to), and the
// action bar offers exactly the acts that are legal (DESIGN.md rule 12 -- an
// act that is not legal is ABSENT, never disabled).

import type { ActionBarTone } from "../../../kit/ActionBar";

/** Put `entry` at `target`: an index to replace, or `entries.length` to append. */
export function equip(entries: readonly string[], entry: string, target: number): string[] {
  const next = [...entries];
  if (target >= 0 && target < next.length) next[target] = entry;
  else next.push(entry);
  return next;
}

/** Move the slot at `from` to `to`. Out of range is no move at all. */
export function moveSlot(entries: readonly string[], from: number, to: number): string[] {
  if (from === to || from < 0 || to < 0 || from >= entries.length || to >= entries.length) return [...entries];
  const next = [...entries];
  const [moved] = next.splice(from, 1);
  next.splice(to, 0, moved!);
  return next;
}

export function removeSlot(entries: readonly string[], index: number): string[] {
  return entries.filter((_, i) => i !== index);
}

export type RouteAct = "cancel" | "restore" | "save" | "keep" | "confirmRestore";

export interface RouteBar {
  state: string;
  detail: string;
  tone: ActionBarTone;
  acts: RouteAct[];
}

/** The scope of a route change, said once and quietly (design brief, section 4). */
export const ROUTE_SCOPE = "Applies to every rule that takes this route";

/**
 * The bar for a route page, from its state.
 *
 * Save is last and primary, and only there when the draft could be saved.
 * Restore shipped is a text act beside it, and only on a shipped route that
 * has been changed -- a custom route has no shipped sources to go back to, and
 * the engine refuses the reset by name. Restoring asks first, in the bar.
 */
export function routeBar(input: {
  shipped: boolean;
  customized: boolean;
  protected: boolean;
  dirty: boolean;
  valid: boolean;
  busy: boolean;
  confirmingRestore: boolean;
  /** What the draft would serve with, in words: "serves with Claude Code". */
  serving: string;
}): RouteBar {
  if (input.protected) {
    return { state: "Protected", detail: "Changing it needs an embedding migration", tone: "none", acts: [] };
  }
  if (input.busy) return { state: "Saving", detail: "", tone: "busy", acts: [] };
  const restorable = input.shipped && input.customized;
  if (input.confirmingRestore && restorable) {
    return { state: "Restore shipped sources?", detail: "Your changes to this route are removed", tone: "paused", acts: ["keep", "confirmRestore"] };
  }
  if (input.dirty) {
    const acts: RouteAct[] = ["cancel"];
    if (restorable) acts.push("restore");
    if (input.valid) acts.push("save");
    return { state: "Unsaved", detail: input.serving, tone: "paused", acts };
  }
  if (restorable) return { state: "Changed from shipped", detail: ROUTE_SCOPE, tone: "live", acts: ["restore"] };
  return { state: input.shipped ? "Shipped" : "Saved", detail: ROUTE_SCOPE, tone: "live", acts: [] };
}
