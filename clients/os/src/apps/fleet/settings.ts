// The Fleet app's own settings, and the store that keeps them.
//
// ===========================================================================
// WHY A SECOND STORE RATHER THAN A CORNER OF THE DESKTOP DOCUMENT
// ===========================================================================
// system/store.ts's DesktopDocument is the SHELL's state -- desks, surfaces,
// dock pins, theme -- and its `version: 1` gate rejects a document whose
// version it does not know. Folding per-app preferences into it would mean
// every app that ships a settings section bumps that version, and a bump
// discards the whole desktop of anyone who has not upgraded yet: a person
// would lose their desks because Fleet learned a checkbox.
//
// So this is a separate key with the SAME discipline -- versioned, sanitized
// on read, every storage touch in a try/catch -- rather than a different
// approach. `sanitizeFleetSettings` is the corrupt-value tolerance the
// desktop store's `sanitizeDocument` applies: an unreadable, wrong-version or
// partially-garbage document comes back as the DEFAULTS, never as a crash and
// never as half a document.
//
// The per-app settings registry (epic #4741) is where this generalizes. When
// it lands, this file becomes its first tenant; until then a hand-rolled
// store is honest and a shared abstraction inferred from one caller is not.

import type { ModuleId } from "../../system/modules";
import type { OsAppSection } from "../../system/registry";

/** The sections this app declares, in manifest order. Exported because the
 *  manifest and the settings picker must offer the same set: a preference
 *  naming a section that no longer exists is what `sanitize` repairs. */
export const FLEET_SECTIONS: OsAppSection[] = [
  { id: "overview", name: "Overview" },
  { id: "machines", name: "Machines" },
  { id: "policies", name: "Policies" },
  // What the fleet can actually SERVE, in the order the router picks from
  // (epic memql#5096). It follows Machines because the two read as one
  // progression -- the hardware, then what runs on it -- and precedes Routing
  // because the ranking this section shows is what Routing's model preference
  // reorders.
  //
  // NO ROLE FLOOR. Both readings behind it are caller-scoped projections:
  // `fleetModels` answers for the caller's own machines plus the
  // shared-inference set, and `inferenceStatus` answers one question about
  // this caller. There is nothing here a signed-in person may not see about
  // their own fleet.
  { id: "models", name: "Model library" },
  { id: "routing", name: "Machine routing" },
  // When work is handed to a local app on one of the caller's own machines,
  // and what happened when it was (epic memql#5009). It follows Workbenches
  // because the three read as one progression -- the cluster's own sandbox,
  // then the person's own computer -- and precedes Logs, which is every
  // app's last-but-one section by convention.
  //
  // NO ROLE FLOOR. Both concepts behind it declare the composite owner tier
  // (`@rowAuthz(owner="ownerUserId", clusterOwner)`), so every signed-in
  // person has a policy and runs of their own and the engine decides how far
  // the read reaches -- exactly the reasoning that leaves Machines, Routing
  // and Workbenches ungated.
  { id: "apps", name: "Activity", needs: ["localApps"] },
  { id: "workbenches", name: "Workspaces", needs: ["workbench"] },
  // The app's slice of the cluster's logs (epic memql#4895): the lines it
  // tagged and the lines about the things it owns. Admin-floored because
  // every read on the log store is (spec L3), and this is the ONE section
  // whose floor is not this app's to choose.
  { id: "logs", name: "Logs", requires: "app:fleet/logs" },
  { id: "settings", name: "Settings" },
];

export const FLEET_SECTION_IDS = FLEET_SECTIONS.map((s) => s.id);

export interface FleetSettings {
  version: 1;
  /** The section the app navigates to when its window opens. */
  defaultSection: string;
  /**
   * Whether revoked machines are listed. OFF by default: a revoked
   * registration is a credential that no longer works, and the standing
   * question the Machines section answers is which machines DO.
   */
  showRevoked: boolean;
}

export const FLEET_SETTINGS_KEY = "memql-os-fleet-v1";

export const DEFAULT_FLEET_SETTINGS: FleetSettings = {
  version: 1,
  // Machines first, per the manifest. Named here as well because this value
  // is what a corrupt or absent document falls back to, and falling back to
  // "whatever is first in an array" would move with an unrelated edit.
  defaultSection: "overview",
  showRevoked: false,
};

/**
 * Bring a parsed document back to invariants.
 *
 * Every field is checked INDEPENDENTLY and repaired to its default, rather
 * than the document being rejected wholesale on the first bad value: a
 * garbage `defaultSection` must not cost the operator their show-revoked
 * choice. A wrong `version` IS wholesale, because the field names cannot be
 * trusted at all then.
 *
 * A `defaultSection` naming a section this app no longer declares is
 * repaired, not kept: navigating to it would leave the window on the
 * manifest's first section with the nav highlighting nothing, which reads as
 * a broken app rather than as a stale preference.
 */
export function sanitizeFleetSettings(raw: unknown): FleetSettings {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ...DEFAULT_FLEET_SETTINGS };
  }
  const doc = raw as Partial<FleetSettings>;
  if (doc.version !== 1) return { ...DEFAULT_FLEET_SETTINGS };

  const defaultSection =
    typeof doc.defaultSection === "string" && FLEET_SECTION_IDS.includes(doc.defaultSection)
      ? doc.defaultSection
      : DEFAULT_FLEET_SETTINGS.defaultSection;

  return {
    version: 1,
    defaultSection,
    showRevoked:
      typeof doc.showRevoked === "boolean"
        ? doc.showRevoked
        : DEFAULT_FLEET_SETTINGS.showRevoked,
  };
}

export interface FleetSettingsStore {
  load(): FleetSettings;
  save(settings: FleetSettings): void;
}

export class LocalFleetSettingsStore implements FleetSettingsStore {
  /** Pass `null` for "no storage" (the default only replaces undefined),
   *  which is the shape the desktop store takes for the same reason: a
   *  private window and a full quota are normal cases, not failures. */
  constructor(
    private readonly storage: Pick<Storage, "getItem" | "setItem"> | null | undefined = globalThis.localStorage,
    private readonly key: string = FLEET_SETTINGS_KEY,
  ) {}

  load(): FleetSettings {
    try {
      const raw = this.storage?.getItem(this.key);
      if (!raw) return { ...DEFAULT_FLEET_SETTINGS };
      return sanitizeFleetSettings(JSON.parse(raw));
    } catch {
      // Unparseable JSON is exactly the corrupt case sanitize exists for;
      // it just never gets that far, so the fallback is repeated here.
      return { ...DEFAULT_FLEET_SETTINGS };
    }
  }

  save(settings: FleetSettings): void {
    try {
      this.storage?.setItem(this.key, JSON.stringify(settings));
    } catch {
      // Best-effort: a preference is not worth failing an interaction over.
    }
  }
}

/** The union of the section requirements above, for the Set up group. */
export const FLEET_REQUIRES: readonly ModuleId[] = Array.from(
  new Set(FLEET_SECTIONS.flatMap((s) => s.needs ?? [])),
);
export const FLEET_WANTS: readonly ModuleId[] = Array.from(
  new Set(FLEET_SECTIONS.flatMap((s) => s.wants ?? [])),
);
