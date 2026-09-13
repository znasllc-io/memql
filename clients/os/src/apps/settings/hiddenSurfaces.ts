// The permissions self-view (memql#4744, re-keyed to capabilities in epic
// memql#5289): everything the ONE access predicate keeps out of this session,
// and the resource each asks for.
//
// Presentation gating only. The panel says so, because the difference
// matters: a surface listed here is one this shell declines to DRAW, not
// one the engine declines to serve. Row admission and the capability gates
// are the authority on every read and write, and a person who reads this
// table as a permission audit would be reading it wrong.

import { describeResource } from "../../system/roles";
import { accessAdmits, sectionsFor } from "../../system/registry";
import type { OsRegistry } from "../../system/registry";

export interface HiddenSurface {
  /** "app" | "section" | "widget" -- what kind of thing is hidden. */
  kind: "app" | "section" | "widget";
  /** What to call it: "Users", "Settings -- Cluster", "Ask". */
  label: string;
  /** The capability its manifest asks for, in words: "read on app:users". */
  requires: string;
}

/**
 * Everything the effective set does not open, in registry order.
 *
 * READS MODULE STATE, so a caller that memoises this must name
 * `accessEpoch` in its deps (memql#4857).
 *
 * A hidden app's SECTIONS are not enumerated under it. The app is already
 * the answer -- listing "Users -- People", "Users -- Invites" under a hidden
 * "Users" pads the table with rows that all say the same thing, and buries
 * the case that is actually informative: a section gated ABOVE an app the
 * person can otherwise open.
 */
export function hiddenSurfaces(registry: OsRegistry): HiddenSurface[] {
  const hidden: HiddenSurface[] = [];

  for (const app of registry.apps) {
    if (!accessAdmits(app.requires)) {
      hidden.push({ kind: "app", label: app.name, requires: describeResource(app.requires) });
      continue;
    }
    const admitted = new Set(sectionsFor(app).map((s) => s.id));
    for (const section of app.sections ?? []) {
      if (admitted.has(section.id)) continue;
      hidden.push({
        kind: "section",
        label: `${app.name} -- ${section.name}`,
        requires: describeResource(section.requires),
      });
    }
  }

  for (const widget of registry.widgets) {
    if (accessAdmits(widget.requires)) continue;
    hidden.push({ kind: "widget", label: widget.name, requires: describeResource(widget.requires) });
  }

  return hidden;
}
