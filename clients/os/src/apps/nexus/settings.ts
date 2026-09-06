import type { OsAppSection } from "../../system/registry";

// The Nexus app's own settings, kept in their own versioned store.
//
// A SEPARATE KEY FROM THE DESKTOP DOCUMENT, for the reason every app since
// Fleet has one: the shell's `DesktopDocument` rejects a version it does not
// know, so folding per-app preferences into it would mean somebody loses
// their desks because an app learned a checkbox.
//
// THE WORK APP'S KEY IS NOT MIGRATED, deliberately. `memql-os-work-v1` held
// two values -- a section id and a checkbox -- and this is pre-release, where
// the house rule is to delete what is no longer needed rather than carry a
// shim. Somebody who had pointed Work at Approvals re-picks it in a second,
// and a reader six months from now is not left wondering why an app called
// Nexus reads a key called work.

/**
 * The sections this app declares, in manifest order.
 *
 * GOALS IS FIRST and is therefore the section a window opens on: a goal is
 * what this app is for, and runs, automations and approvals are all things a
 * goal produced. The app's own settings can point a window elsewhere -- and
 * the one preference somebody actually wants here is Approvals, because a
 * parked run is stuck until a person acts and an operator who lives in this
 * app all day wants to land on the queue.
 *
 * RUNS STAYS, and it is a decision rather than a leftover (design record D3,
 * owner-answered). `v1:work:run.goalId` is EMPTY for an automation run that no
 * goal asked for -- a scheduled sweep, an event-triggered automation -- and in
 * a goal-only app those runs would have no home at all. A goal-born run also
 * opens from its goal; this is a second door on the same room.
 *
 * Exported rather than written twice, for the reason its ten siblings are: the
 * manifest and the settings picker must offer the SAME set, and a preference
 * naming a section the manifest does not declare leaves the window on Goals
 * with the nav highlighting nothing.
 */
export const NEXUS_SECTIONS: OsAppSection[] = [
  { id: "goals", name: "Goals" },
  { id: "runs", name: "Runs" },
  { id: "automations", name: "Automations" },
  { id: "approvals", name: "Approvals" },
  // Admin-floored because every read on the log store is (spec L3). The one
  // section whose floor is not this app's to choose.
  { id: "logs", name: "Logs", roles: { min: "admin" } },
  { id: "settings", name: "Settings" },
];

export const NEXUS_SECTION_IDS = NEXUS_SECTIONS.map((s) => s.id);

export interface NexusSettings {
  version: 1;
  /** The section the app navigates to when its window opens. */
  defaultSection: string;
  /**
   * Whether finished runs are listed. ON by default, unlike every
   * "show archived" preference in this shell -- and the difference is the
   * subject rather than an inconsistency.
   *
   * An archived thing is one somebody filed away; a finished run is the
   * ordinary end of every run there is, and the question this list answers
   * most often is "what did it do", which is a question about finished runs.
   * Turning it OFF is the narrower reading -- "what is in flight right now"
   * -- so it is the choice, not the default.
   */
  showFinishedRuns: boolean;
}

// THERE IS NO "SHOW DECIDED APPROVALS" PREFERENCE, and its absence is a
// decision rather than an omission. `workApprovalsForOwner` carries
// `decision==""` in the DSL, so a toggle over the rows this app holds could
// only ever reveal approvals decided WHILE THE WINDOW WAS OPEN -- a list that
// filled up as you worked and was empty again on reload. Showing decided
// approvals properly needs a second read, and offering a switch that half
// works is worse than not offering one.

export const NEXUS_SETTINGS_KEY = "memql-os-nexus-v1";

export const DEFAULT_NEXUS_SETTINGS: NexusSettings = {
  version: 1,
  // Named rather than read off NEXUS_SECTIONS[0]: this value is what a corrupt
  // or absent document falls back to, and falling back to "whatever is first
  // in an array" would move with an unrelated edit to that array.
  defaultSection: "goals",
  showFinishedRuns: true,
};

/**
 * Bring a parsed document back to invariants.
 *
 * Every field is repaired INDEPENDENTLY rather than the document being
 * rejected on the first bad value: a garbage `defaultSection` must not cost
 * somebody their approvals preference. A wrong `version` IS wholesale,
 * because then the field names cannot be trusted at all.
 */
export function sanitizeNexusSettings(raw: unknown): NexusSettings {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ...DEFAULT_NEXUS_SETTINGS };
  }
  const doc = raw as Partial<NexusSettings>;
  if (doc.version !== 1) return { ...DEFAULT_NEXUS_SETTINGS };

  const defaultSection =
    typeof doc.defaultSection === "string" && NEXUS_SECTION_IDS.includes(doc.defaultSection)
      ? doc.defaultSection
      : DEFAULT_NEXUS_SETTINGS.defaultSection;

  return {
    version: 1,
    defaultSection,
    showFinishedRuns:
      typeof doc.showFinishedRuns === "boolean"
        ? doc.showFinishedRuns
        : DEFAULT_NEXUS_SETTINGS.showFinishedRuns,
  };
}

export interface NexusSettingsStore {
  load(): NexusSettings;
  save(settings: NexusSettings): void;
}

export class LocalNexusSettingsStore implements NexusSettingsStore {
  /** `null` is "no storage" -- a private window and a full quota are normal
   *  cases, not failures. The default only replaces `undefined`. */
  constructor(
    private readonly storage:
      | Pick<Storage, "getItem" | "setItem">
      | null
      | undefined = globalThis.localStorage,
    private readonly key: string = NEXUS_SETTINGS_KEY,
  ) {}

  load(): NexusSettings {
    try {
      const raw = this.storage?.getItem(this.key);
      if (!raw) return { ...DEFAULT_NEXUS_SETTINGS };
      return sanitizeNexusSettings(JSON.parse(raw));
    } catch {
      return { ...DEFAULT_NEXUS_SETTINGS };
    }
  }

  save(settings: NexusSettings): void {
    try {
      this.storage?.setItem(this.key, JSON.stringify(settings));
    } catch {
      // Best-effort: a preference is not worth failing an interaction over.
    }
  }
}
