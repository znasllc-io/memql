// The permissions self-view (memql#4744): everything the ONE role predicate
// keeps out of this session, and the minimum role each needs.
//
// Presentation gating only. The panel says so, because the difference
// matters: a surface listed here is one this shell declines to DRAW, not
// one the engine declines to serve. Row admission is the authority on every
// read, and a person who reads this table as a permission audit would be
// reading it wrong.

import { roleAdmits } from "../../system/roles";
import { sectionsForRole } from "../../system/registry";
import type { OsRegistry } from "../../system/registry";
import type { RoleRequirement } from "../../system/roles";

export interface HiddenSurface {
  /** "app" | "section" | "widget" -- what kind of thing is hidden. */
  kind: "app" | "section" | "widget";
  /** What to call it: "Users", "Settings -- Cluster", "Ask". */
  label: string;
  /** The minimum role its manifest asks for. */
  requires: string;
}

/**
 * Everything the actor cannot see, in registry order.
 *
 * A hidden app's SECTIONS are not enumerated under it. The app is already
 * the answer -- listing "Users -- People", "Users -- Invites" under a hidden
 * "Users" pads the table with rows that all say the same thing, and buries
 * the case that is actually informative: a section gated ABOVE an app the
 * person can otherwise open.
 */
export function hiddenSurfaces(registry: OsRegistry, actorRole: string): HiddenSurface[] {
  const hidden: HiddenSurface[] = [];

  for (const app of registry.apps) {
    if (!roleAdmits(actorRole, app.roles)) {
      hidden.push({ kind: "app", label: app.name, requires: requirementOf(app.roles) });
      continue;
    }
    const admitted = new Set(sectionsForRole(app, actorRole).map((s) => s.id));
    for (const section of app.sections ?? []) {
      if (admitted.has(section.id)) continue;
      hidden.push({
        kind: "section",
        label: `${app.name} -- ${section.name}`,
        requires: requirementOf(section.roles),
      });
    }
  }

  for (const widget of registry.widgets) {
    if (roleAdmits(actorRole, widget.roles)) continue;
    hidden.push({ kind: "widget", label: widget.name, requires: requirementOf(widget.roles) });
  }

  return hidden;
}

/**
 * A surface with no requirement that is still hidden can only be hidden
 * because the ACTOR's role is unrankable (`roleAdmits` refuses to let an
 * unknown role unlock anything). Saying "requires none" there would be
 * true and useless; naming the real cause is what lets someone act on it.
 */
function requirementOf(requirement?: RoleRequirement): string {
  return requirement ? requirement.min : "a recognized role";
}
