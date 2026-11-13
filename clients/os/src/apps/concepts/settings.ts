// The Concepts app's own settings, and the store that keeps them.
//
// A second versioned localStorage key rather than a corner of the desktop
// document, for the reason apps/fleet/settings.ts states at length: the
// shell's DesktopDocument rejects a version it does not know, so folding
// per-app preferences into it would mean a person loses their desks
// because an app learned a checkbox.

import type { OsAppSection } from "../../system/registry";

/**
 * The sections this app declares, in manifest order. Exported because the
 * manifest and the settings picker must offer the same set -- a preference
 * naming a section the manifest does not declare leaves the window on the
 * first section with the nav highlighting nothing.
 */
export const CONCEPTS_SECTIONS: OsAppSection[] = [
  { id: "registry", name: "Registry" },
  { id: "language", name: "Language", requires: "app:settings/language" },
  // The app's slice of the cluster's logs (epic memql#4895). Admin-floored
  // because every read on the log store is (spec L3), and this is the one
  // section whose floor is not this app's to choose.
  { id: "logs", name: "Logs", requires: "app:concepts/logs" },
  { id: "settings", name: "Settings" },
];

export const CONCEPTS_SECTION_IDS = CONCEPTS_SECTIONS.map((s) => s.id);

export interface ConceptsSettings {
  version: 1;
  /** The section the app navigates to when its window opens. */
  defaultSection: string;
  /**
   * Ten rows per page. Old saved preferences are normalized to the shared
   * resource navigation size when read.
   */
  pageSize: number;
  /**
   * Whether the schema column lists keys rows carry that the concept does
   * not declare.
   *
   * ON by default. An undeclared key is the finding this app can produce
   * that nothing else can, and hiding it by default would mean the surface
   * only tells you what you could already read in the DSL file.
   */
  showUndeclaredFields: boolean;
}

export const CONCEPTS_SETTINGS_KEY = "memql-os-concepts-v1";

export const DEFAULT_CONCEPTS_SETTINGS: ConceptsSettings = {
  version: 1,
  // Named here as well as in the manifest because this value is what a
  // corrupt or absent document falls back to, and falling back to a
  // section that does not exist is the failure sanitize repairs.
  defaultSection: "registry",
  pageSize: 10,
  showUndeclaredFields: true,
};

/** The page sizes the settings section offers. A closed set: an arbitrary
 *  number here is a way to ask the cluster for a page nobody wanted. */
export const CONCEPTS_PAGE_SIZES = [10] as const;

/**
 * An unreadable, wrong-version or partially-garbage document comes back as
 * the DEFAULTS -- never as a crash and never as half a document. Same
 * discipline as the desktop store's sanitizeDocument.
 */
export function sanitizeConceptsSettings(raw: unknown): ConceptsSettings {
  if (raw === null || typeof raw !== "object") return { ...DEFAULT_CONCEPTS_SETTINGS };
  const doc = raw as Partial<ConceptsSettings>;
  if (doc.version !== 1) return { ...DEFAULT_CONCEPTS_SETTINGS };

  const defaultSection =
    typeof doc.defaultSection === "string" && CONCEPTS_SECTION_IDS.includes(doc.defaultSection)
      ? doc.defaultSection
      : DEFAULT_CONCEPTS_SETTINGS.defaultSection;

  const pageSize =
    typeof doc.pageSize === "number" &&
    (CONCEPTS_PAGE_SIZES as readonly number[]).includes(doc.pageSize)
      ? doc.pageSize
      : DEFAULT_CONCEPTS_SETTINGS.pageSize;

  return {
    version: 1,
    defaultSection,
    pageSize,
    showUndeclaredFields:
      typeof doc.showUndeclaredFields === "boolean"
        ? doc.showUndeclaredFields
        : DEFAULT_CONCEPTS_SETTINGS.showUndeclaredFields,
  };
}

export interface ConceptsSettingsStore {
  load(): ConceptsSettings;
  save(next: ConceptsSettings): void;
}

/** Every storage touch in a try/catch: a browser with storage blocked gets
 *  the defaults and a working app, not an exception on first render. */
export class LocalConceptsSettingsStore implements ConceptsSettingsStore {
  load(): ConceptsSettings {
    try {
      const raw = window.localStorage.getItem(CONCEPTS_SETTINGS_KEY);
      if (raw === null) return { ...DEFAULT_CONCEPTS_SETTINGS };
      return sanitizeConceptsSettings(JSON.parse(raw));
    } catch {
      return { ...DEFAULT_CONCEPTS_SETTINGS };
    }
  }

  save(next: ConceptsSettings): void {
    try {
      window.localStorage.setItem(CONCEPTS_SETTINGS_KEY, JSON.stringify(next));
    } catch {
      // A full or blocked store loses the preference, not the session.
    }
  }
}
