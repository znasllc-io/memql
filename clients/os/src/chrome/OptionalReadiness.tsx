import { useMemo } from "react";

import { usePublishAttention } from "../attention/Attention";
import type { AttentionChange } from "../attention/model";
import type { Readiness } from "../live/readiness";
import { MODULE_NAMES, MODULE_SETTINGS_SECTION, READINESS_MODULES, type ModuleId } from "../system/modules";
import type { Verdict } from "../system/readinessFold";
import { appById, canOpen, sectionsFor, type OsRegistry } from "../system/registry";
import { useSession } from "./access";
import { useOs } from "./state";

// THE MARK FOR AN OPTIONAL ITEM NOBODY HAS ANSWERED (epic memql#5479, design
// record D15).
//
// ===========================================================================
// A STATE, NOT NEW CHROME
// ===========================================================================
// An optional readiness item -- Pipelines is the first -- marks the place it is
// set up from while it is neither done nor dismissed: the Settings icon in the
// dock, the launcher tile and the section's entry in the window's own nav. All
// of those already draw the shared attention dot (src/attention), so this is a
// RUNTIME change published at shell lifetime -- the Deployables precedent --
// and nothing here draws anything. A closed Settings is marked too, which is
// the point: nobody opens Settings to find out whether Settings wants them.
//
// ===========================================================================
// ONLY "NOT NOW" ACKNOWLEDGES IT, SO IT CARRIES A TARGET
// ===========================================================================
// The window frame acknowledges every unseen change aimed at the section it is
// showing (`AttentionDestination` around each window's body, and the phone
// shell's), and a change with no target IS aimed at the section. Visiting
// Settings > Pipelines would then dismiss the item on sight, which D15 rules
// out: looking at what the item asks is not answering it. So the change names
// a deeper destination, `readiness`, that no `AttentionDestination` wraps --
// every ancestor still draws the dot, and the one way to clear it is the
// section's "Not now", which acknowledges exactly this change.
//
// ===========================================================================
// FOR WHOM
// ===========================================================================
// A viewer who may open the section the item is set up from: the app's door
// and the section's own resource, asked of the effective capability set the
// way every surface asks. For Pipelines that is `read app:settings/pipelines`,
// seeded on the owner alone. Recomputed on the access epoch, because the set
// lands after the first render and moves on focus and after a grant.

/** The publisher's source id, one per shell. */
export const OPTIONAL_READINESS_SOURCE = "readiness:optional";

/** Advance only when what an optional item asks changes meaningfully enough to
 *  ask again of the people who already said "Not now". */
export const OPTIONAL_READINESS_REVISION = "optional-1";

/** The deeper destination no window acknowledges on sight (see the header). */
export const OPTIONAL_READINESS_TARGET = "readiness";

/** The change id for one module's item. Receipt ids are shared across the OS,
 *  so the prefix names what kind of change this is. */
export function optionalReadinessId(module: ModuleId): string {
  return `readiness:${module}`;
}

/**
 * Whether a module is an optional item that is still open: optional, may be
 * answered "Not now", and KNOWN not to be set up.
 *
 * `unreported` is not open. A module nobody has reported is not known to be
 * unset, and marking Settings for it would turn a quiet agent node into a
 * claim that the owner has something to do.
 */
export function optionalItemOpen(verdict: Verdict | null): boolean {
  return (
    verdict !== null &&
    verdict.optional &&
    verdict.dismissable &&
    (verdict.state === "unconfigured" || verdict.state === "partial")
  );
}

/** The changes to publish: one per open optional item this viewer can reach. */
export function optionalReadinessChanges(readiness: Readiness | undefined, registry: OsRegistry): AttentionChange[] {
  if (!readiness?.loaded) return [];
  const out: AttentionChange[] = [];
  for (const module of READINESS_MODULES) {
    if (!optionalItemOpen(readiness.of(module))) continue;
    const target = MODULE_SETTINGS_SECTION[module];
    // An item with nowhere to be set up from has nowhere to point a mark at.
    if (target === null) continue;
    const app = appById(registry, target.app);
    if (app === undefined || !canOpen(registry, target.app)) continue;
    if (!sectionsFor(app).some((section) => section.id === target.section)) continue;
    out.push({
      id: optionalReadinessId(module),
      revision: OPTIONAL_READINESS_REVISION,
      appId: target.app,
      sectionId: target.section,
      target: OPTIONAL_READINESS_TARGET,
      label: `${MODULE_NAMES[module]} can be set up`,
      kind: "runtime",
    });
  }
  return out;
}

/** Lives with the shell, inside the attention provider. Renders nothing. */
export function OptionalReadiness() {
  const { readiness } = useSession();
  const { registry, accessEpoch } = useOs();
  // `accessEpoch` in the deps for the launcher's reason (memql#4857):
  // `sectionsFor` and `canOpen` read the effective capability set out of band,
  // so without it this would keep the answer it computed before the set
  // landed -- which is no mark for anybody.
  const changes = useMemo(
    () => optionalReadinessChanges(readiness, registry),
    [readiness, registry, accessEpoch],
  );
  usePublishAttention(OPTIONAL_READINESS_SOURCE, changes);
  return null;
}
