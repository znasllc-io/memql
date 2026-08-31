// The Deployables app's own settings, and the store that keeps them.
//
// Same discipline as Fleet's and Users' (`apps/*/settings.ts`), and a separate
// key for the same reason: the SHELL's DesktopDocument rejects a version it
// does not know, so folding per-app preferences into it would mean somebody
// loses their desks because an app learned a checkbox.

import type { OsAppSection } from "../../system/registry";

/**
 * The sections this app declares, in manifest order.
 *
 * MAP IS FIRST, and therefore the section a window opens on. It is the app's
 * signature surface -- the whole reason the epic exists is that "what serves
 * where" is a shape rather than a table -- and the default-landing preference
 * below is what takes somebody who disagrees straight to the list instead.
 *
 * ACTIONS CARRIES A ROLE, and that role is PRESENTATION (spec section E). The
 * engine's composite tier on `v1:platform:site`, the Go hostname policy and
 * `sitePublishFromArtifact`'s own validation remain the authority on every
 * write this app makes; hiding the section is a courtesy to a reader who
 * cannot use it, never the boundary.
 *
 * Exported rather than written twice, because the manifest and the settings
 * picker must offer the SAME set: a second literal is one that can disagree,
 * and a preference naming a section the manifest does not declare leaves the
 * window on the first section with the nav highlighting nothing -- which reads
 * as a broken app rather than as a stale preference.
 */
export const DEPLOYABLES_SECTIONS: OsAppSection[] = [
  { id: "map", name: "Map" },
  { id: "sites", name: "Sites" },
  { id: "actions", name: "Actions", roles: { min: "admin" } },
  { id: "settings", name: "Settings" },
];

export const DEPLOYABLES_SECTION_IDS = DEPLOYABLES_SECTIONS.map((s) => s.id);

/** Rows per screen, as a reading choice rather than a data one. */
export type ListDensity = "comfortable" | "compact";

export const LIST_DENSITIES: readonly ListDensity[] = ["comfortable", "compact"];

export interface DeployablesSettings {
  version: 1;
  /** The section the app navigates to when its window opens. */
  defaultSection: string;
  /**
   * How tightly the Sites list packs.
   *
   * A VIEW setting and not a filter: it changes nothing about which rows are
   * read or shown, so flipping it re-baselines no arrival cue and costs no
   * round trip. That is why it is safe to be instant.
   */
  density: ListDensity;
}

export const DEPLOYABLES_SETTINGS_KEY = "memql-os-deployables-v1";

export const DEFAULT_DEPLOYABLES_SETTINGS: DeployablesSettings = {
  version: 1,
  // Map first, per the manifest. Named here as well because this value is what
  // a corrupt or absent document falls back to, and falling back to "whatever
  // is first in an array" would move with an unrelated edit.
  defaultSection: "map",
  density: "comfortable",
};

/**
 * Bring a parsed document back to invariants.
 *
 * Every field is repaired INDEPENDENTLY rather than the document being
 * rejected on the first bad value: a garbage `defaultSection` must not cost
 * somebody their density choice. A wrong `version` IS wholesale, because then
 * the field names cannot be trusted at all.
 */
export function sanitizeDeployablesSettings(raw: unknown): DeployablesSettings {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ...DEFAULT_DEPLOYABLES_SETTINGS };
  }
  const doc = raw as Partial<DeployablesSettings>;
  if (doc.version !== 1) return { ...DEFAULT_DEPLOYABLES_SETTINGS };

  const defaultSection =
    typeof doc.defaultSection === "string" && DEPLOYABLES_SECTION_IDS.includes(doc.defaultSection)
      ? doc.defaultSection
      : DEFAULT_DEPLOYABLES_SETTINGS.defaultSection;

  const density =
    doc.density !== undefined && LIST_DENSITIES.includes(doc.density as ListDensity)
      ? (doc.density as ListDensity)
      : DEFAULT_DEPLOYABLES_SETTINGS.density;

  return { version: 1, defaultSection, density };
}

export interface DeployablesSettingsStore {
  load(): DeployablesSettings;
  save(settings: DeployablesSettings): void;
}

export class LocalDeployablesSettingsStore implements DeployablesSettingsStore {
  /** `null` is "no storage" -- a private window and a full quota are normal
   *  cases, not failures. The default only replaces `undefined`. */
  constructor(
    private readonly storage:
      | Pick<Storage, "getItem" | "setItem">
      | null
      | undefined = globalThis.localStorage,
    private readonly key: string = DEPLOYABLES_SETTINGS_KEY,
  ) {}

  load(): DeployablesSettings {
    try {
      const raw = this.storage?.getItem(this.key);
      if (!raw) return { ...DEFAULT_DEPLOYABLES_SETTINGS };
      return sanitizeDeployablesSettings(JSON.parse(raw));
    } catch {
      // Unparseable JSON is the corrupt case sanitize exists for; it just never
      // gets that far, so the fallback is repeated here.
      return { ...DEFAULT_DEPLOYABLES_SETTINGS };
    }
  }

  save(settings: DeployablesSettings): void {
    try {
      this.storage?.setItem(this.key, JSON.stringify(settings));
    } catch {
      // Best-effort: a preference is not worth failing an interaction over.
    }
  }
}
