import type { OsAppSection } from "../../system/registry";

// The Work app's own settings, kept in their own versioned store.
//
// A SEPARATE KEY FROM THE DESKTOP DOCUMENT, for the reason every app since
// Fleet has one: the shell's `DesktopDocument` rejects a version it does not
// know, so folding per-app preferences into it would mean somebody loses
// their desks because an app learned a checkbox.

/**
 * The sections this app declares, in manifest order.
 *
 * GOALS IS FIRST and is therefore the section a window opens on: a goal is
 * what this app is for, and runs, steps and approvals are all things a goal
 * produced. The app's own settings can point a window elsewhere -- and the
 * one preference somebody actually wants here is Approvals, because a parked
 * run is stuck until a person acts and an operator who lives in this app all
 * day wants to land on the queue.
 *
 * Exported rather than written twice, for the reason its eight siblings are:
 * the manifest and the settings picker must offer the SAME set, and a
 * preference naming a section the manifest does not declare leaves the window
 * on Goals with the nav highlighting nothing.
 */
export const WORK_SECTIONS: OsAppSection[] = [
  { id: "goals", name: "Goals" },
  { id: "runs", name: "Runs" },
  { id: "approvals", name: "Approvals" },
  // Admin-floored because every read on the log store is (spec L3). The one
  // section whose floor is not this app's to choose.
  { id: "logs", name: "Logs", roles: { min: "admin" } },
  { id: "settings", name: "Settings" },
];

export const WORK_SECTION_IDS = WORK_SECTIONS.map((s) => s.id);

export interface WorkSettings {
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

export const WORK_SETTINGS_KEY = "memql-os-work-v1";

export const DEFAULT_WORK_SETTINGS: WorkSettings = {
  version: 1,
  // Named rather than read off WORK_SECTIONS[0]: this value is what a corrupt
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
export function sanitizeWorkSettings(raw: unknown): WorkSettings {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ...DEFAULT_WORK_SETTINGS };
  }
  const doc = raw as Partial<WorkSettings>;
  if (doc.version !== 1) return { ...DEFAULT_WORK_SETTINGS };

  const defaultSection =
    typeof doc.defaultSection === "string" && WORK_SECTION_IDS.includes(doc.defaultSection)
      ? doc.defaultSection
      : DEFAULT_WORK_SETTINGS.defaultSection;

  return {
    version: 1,
    defaultSection,
    showFinishedRuns:
      typeof doc.showFinishedRuns === "boolean"
        ? doc.showFinishedRuns
        : DEFAULT_WORK_SETTINGS.showFinishedRuns,
  };
}

export interface WorkSettingsStore {
  load(): WorkSettings;
  save(settings: WorkSettings): void;
}

export class LocalWorkSettingsStore implements WorkSettingsStore {
  /** `null` is "no storage" -- a private window and a full quota are normal
   *  cases, not failures. The default only replaces `undefined`. */
  constructor(
    private readonly storage:
      | Pick<Storage, "getItem" | "setItem">
      | null
      | undefined = globalThis.localStorage,
    private readonly key: string = WORK_SETTINGS_KEY,
  ) {}

  load(): WorkSettings {
    try {
      const raw = this.storage?.getItem(this.key);
      if (!raw) return { ...DEFAULT_WORK_SETTINGS };
      return sanitizeWorkSettings(JSON.parse(raw));
    } catch {
      return { ...DEFAULT_WORK_SETTINGS };
    }
  }

  save(settings: WorkSettings): void {
    try {
      this.storage?.setItem(this.key, JSON.stringify(settings));
    } catch {
      // Best-effort: a preference is not worth failing an interaction over.
    }
  }
}
