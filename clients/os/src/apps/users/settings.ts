// The Users app's own settings, and the store that keeps them.
//
// Same discipline as the Fleet's (apps/fleet/settings.ts) and a separate key
// for the same reason: the SHELL's DesktopDocument rejects a version it does
// not know, so folding per-app preferences into it would mean a person loses
// their desks because an app learned a checkbox.

import type { ModuleId } from "../../system/modules";
import type { OsAppSection } from "../../system/registry";

/**
 * The sections this app declares, in manifest order.
 *
 * Exported because the manifest and the settings picker must offer the SAME
 * set. A second literal is one that can disagree, and a preference naming a
 * section the manifest does not declare leaves the window on the first
 * section with the nav highlighting nothing -- which reads as a broken app
 * rather than as a stale preference.
 */
export const USERS_SECTIONS: OsAppSection[] = [
  { id: "people", name: "People" },
  { id: "groups", name: "Groups" },
  { id: "roles", name: "Roles" },
  // The app's slice of the cluster's logs (epic memql#4895): the lines it
  // tagged and the lines about the things it owns. Admin-floored because
  // every read on the log store is (spec L3), and this is the ONE section
  // whose floor is not this app's to choose.
  { id: "logs", name: "Logs", requires: "app:users/logs" },
  { id: "settings", name: "Settings" },
];

export const USERS_SECTION_IDS = USERS_SECTIONS.map((s) => s.id);

/**
 * How the People list is ordered. A preference, per rule 3 and rule 4: sort is
 * quiet text on the scope line and its DEFAULT lives here.
 *
 * Two orderings and no more. "Name" is the one a person reads a directory by;
 * "last seen" is the one an operator reads it by when they are looking for
 * somebody who has stopped arriving. A third would be a column nobody sorts on.
 */
export type UsersSort = "name" | "lastSeen";

export const USERS_SORTS: readonly UsersSort[] = ["name", "lastSeen"];

export interface UsersSettings {
  version: 1;
  /** The section the app navigates to when its window opens. */
  defaultSection: string;
  /**
   * Whether deactivated people are listed. OFF by default, for the reason the
   * Fleet hides revoked machines: the standing question a People list answers
   * is who can currently reach this cluster, and a list padded with
   * deactivated accounts makes the live ones harder to see.
   *
   * It is a FILTER and not a second read -- `searchUsers` takes an optional
   * `active` argument, and passing it would make this toggle re-seed the
   * collection and re-baseline every arrival cue. Seeding unfiltered and
   * narrowing on the read side is what lets the toggle be instant and quiet.
   */
  showDeactivated: boolean;
  /**
   * Whether archived groups are listed, the Groups section's counterpart to
   * `showDeactivated` and off for the same reason: an archived group grants
   * nothing, so the standing question the list answers is which groups are
   * placing people right now.
   *
   * SEPARATE FROM `showDeactivated` rather than one "show retired things"
   * flag, because the two are different questions asked in different sections
   * and folding them would make turning one on turn the other on.
   */
  showArchivedGroups: boolean;
  /** The People list's default ordering. */
  sort: UsersSort;
}

export const USERS_SETTINGS_KEY = "memql-os-users-v1";

export const DEFAULT_USERS_SETTINGS: UsersSettings = {
  version: 1,
  // People first, per the manifest. Named here as well because this value is
  // what a corrupt or absent document falls back to, and falling back to
  // "whatever is first in an array" would move with an unrelated edit.
  defaultSection: "people",
  showDeactivated: false,
  showArchivedGroups: false,
  sort: "name",
};

/**
 * Bring a parsed document back to invariants.
 *
 * Every field is repaired INDEPENDENTLY rather than the document being
 * rejected on the first bad value: a garbage `defaultSection` must not cost
 * somebody their show-deactivated choice. A wrong `version` IS wholesale,
 * because then the field names cannot be trusted at all.
 */
export function sanitizeUsersSettings(raw: unknown): UsersSettings {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ...DEFAULT_USERS_SETTINGS };
  }
  const doc = raw as Partial<UsersSettings>;
  if (doc.version !== 1) return { ...DEFAULT_USERS_SETTINGS };

  const defaultSection =
    typeof doc.defaultSection === "string" && USERS_SECTION_IDS.includes(doc.defaultSection)
      ? doc.defaultSection
      : DEFAULT_USERS_SETTINGS.defaultSection;

  return {
    version: 1,
    defaultSection,
    showDeactivated:
      typeof doc.showDeactivated === "boolean"
        ? doc.showDeactivated
        : DEFAULT_USERS_SETTINGS.showDeactivated,
    // EACH FIELD ON ITS OWN, still. A `sort` value from a build that offered a
    // third ordering must not cost somebody the two booleans beside it.
    showArchivedGroups:
      typeof doc.showArchivedGroups === "boolean"
        ? doc.showArchivedGroups
        : DEFAULT_USERS_SETTINGS.showArchivedGroups,
    sort:
      doc.sort === "name" || doc.sort === "lastSeen" ? doc.sort : DEFAULT_USERS_SETTINGS.sort,
  };
}

export interface UsersSettingsStore {
  load(): UsersSettings;
  save(settings: UsersSettings): void;
}

export class LocalUsersSettingsStore implements UsersSettingsStore {
  /** `null` is "no storage" -- a private window and a full quota are normal
   *  cases, not failures. The default only replaces `undefined`. */
  constructor(
    private readonly storage:
      | Pick<Storage, "getItem" | "setItem">
      | null
      | undefined = globalThis.localStorage,
    private readonly key: string = USERS_SETTINGS_KEY,
  ) {}

  load(): UsersSettings {
    try {
      const raw = this.storage?.getItem(this.key);
      if (!raw) return { ...DEFAULT_USERS_SETTINGS };
      return sanitizeUsersSettings(JSON.parse(raw));
    } catch {
      // Unparseable JSON is the corrupt case sanitize exists for; it just
      // never gets that far, so the fallback is repeated here.
      return { ...DEFAULT_USERS_SETTINGS };
    }
  }

  save(settings: UsersSettings): void {
    try {
      this.storage?.setItem(this.key, JSON.stringify(settings));
    } catch {
      // Best-effort: a preference is not worth failing an interaction over.
    }
  }
}

/** The union of the section requirements above, for the Set up group. */
export const USERS_REQUIRES: readonly ModuleId[] = Array.from(
  new Set(USERS_SECTIONS.flatMap((s) => s.needs ?? [])),
);
export const USERS_WANTS: readonly ModuleId[] = Array.from(
  new Set(USERS_SECTIONS.flatMap((s) => s.wants ?? [])),
);
