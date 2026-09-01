// The Training app's own settings, and the store that keeps them.
//
// Same discipline as Fleet's, Users' and Deployables' (`apps/*/settings.ts`),
// and a separate key for the same reason: the SHELL's DesktopDocument rejects
// a version it does not know, so folding per-app preferences into it would
// mean somebody loses their desks because an app learned a checkbox.

import type { OsAppSection } from "../../system/registry";

/**
 * The sections this app declares, in manifest order.
 *
 * UPLOAD IS FIRST, and therefore the section a window opens on: this app is
 * for teaching MemQL from files, and the dropzone is the thing it is for.
 * Somebody who mostly reviews can point it at Review instead, which the
 * default-landing preference below does.
 *
 * NO SECTION CARRIES A ROLE, and the APP carries `writer`. That split is
 * deliberate: every surface here reads or writes the same two populations, so
 * there is no line inside the app where the answer changes -- a reader who
 * cannot train cannot use any of it, and a writer who can use one can use all
 * of them. The app-level gate is PRESENTATION (spec section E); the engine's
 * row admission remains the authority on every read, and
 * `setChunkValidationStatus` on every write.
 *
 * Exported rather than written twice, because the manifest and the settings
 * picker must offer the SAME set: a second literal is one that can disagree,
 * and a preference naming a section the manifest does not declare leaves the
 * window on the first section with the nav highlighting nothing -- which reads
 * as a broken app rather than as a stale preference.
 */
export const TRAINING_SECTIONS: OsAppSection[] = [
  { id: "upload", name: "Upload" },
  { id: "review", name: "Review" },
  { id: "domains", name: "Domains" },
  { id: "settings", name: "Settings" },
];

export const TRAINING_SECTION_IDS = TRAINING_SECTIONS.map((s) => s.id);

export interface TrainingSettings {
  version: 1;
  /** The section the app navigates to when its window opens. */
  defaultSection: string;
  /**
   * Whether a finished analysis takes the window to the review queue.
   *
   * OFF BY DEFAULT, and that is the conservative reading rather than the timid
   * one: an analysis finishing is not a reason to move somebody who is in the
   * middle of dropping the next five files. Somebody who works one file at a
   * time turns it on and the queue is waiting when the plan lands.
   *
   * It fires on a plan reaching `succeeded` and never on `failed`: there is
   * nothing to review after a failure, and navigating away from the error
   * message would hide the only account of what happened.
   */
  autoOpenReview: boolean;
}

export const TRAINING_SETTINGS_KEY = "memql-os-training-v1";

export const DEFAULT_TRAINING_SETTINGS: TrainingSettings = {
  version: 1,
  // Upload first, per the manifest. Named here as well because this value is
  // what a corrupt or absent document falls back to, and falling back to
  // "whatever is first in an array" would move with an unrelated edit.
  defaultSection: "upload",
  autoOpenReview: false,
};

/**
 * Bring a parsed document back to invariants.
 *
 * Every field is repaired INDEPENDENTLY rather than the document being
 * rejected on the first bad value: a garbage `defaultSection` must not cost
 * somebody their auto-open choice. A wrong `version` IS wholesale, because
 * then the field names cannot be trusted at all.
 */
export function sanitizeTrainingSettings(raw: unknown): TrainingSettings {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ...DEFAULT_TRAINING_SETTINGS };
  }
  const doc = raw as Partial<TrainingSettings>;
  if (doc.version !== 1) return { ...DEFAULT_TRAINING_SETTINGS };

  const defaultSection =
    typeof doc.defaultSection === "string" && TRAINING_SECTION_IDS.includes(doc.defaultSection)
      ? doc.defaultSection
      : DEFAULT_TRAINING_SETTINGS.defaultSection;

  const autoOpenReview =
    typeof doc.autoOpenReview === "boolean"
      ? doc.autoOpenReview
      : DEFAULT_TRAINING_SETTINGS.autoOpenReview;

  return { version: 1, defaultSection, autoOpenReview };
}

export interface TrainingSettingsStore {
  load(): TrainingSettings;
  save(settings: TrainingSettings): void;
}

export class LocalTrainingSettingsStore implements TrainingSettingsStore {
  /** `null` is "no storage" -- a private window and a full quota are normal
   *  cases, not failures. The default only replaces `undefined`. */
  constructor(
    private readonly storage:
      | Pick<Storage, "getItem" | "setItem">
      | null
      | undefined = globalThis.localStorage,
    private readonly key: string = TRAINING_SETTINGS_KEY,
  ) {}

  load(): TrainingSettings {
    try {
      const raw = this.storage?.getItem(this.key);
      if (!raw) return { ...DEFAULT_TRAINING_SETTINGS };
      return sanitizeTrainingSettings(JSON.parse(raw));
    } catch {
      // Unparseable JSON is the corrupt case sanitize exists for; it just
      // never gets that far, so the fallback is repeated here.
      return { ...DEFAULT_TRAINING_SETTINGS };
    }
  }

  save(settings: TrainingSettings): void {
    try {
      this.storage?.setItem(this.key, JSON.stringify(settings));
    } catch {
      // Best-effort: a preference is not worth failing an interaction over.
    }
  }
}
