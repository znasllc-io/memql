// The Deployables app's own settings, and the store that keeps them.
//
// Same discipline as Fleet's and Users' (`apps/*/settings.ts`), and a separate
// key for the same reason: the SHELL's DesktopDocument rejects a version it
// does not know, so folding per-app preferences into it would mean somebody
// loses their desks because an app learned a checkbox.

import type { OsAppSection } from "../../system/registry";
import { TRAFFIC_WINDOWS, type TrafficWindow } from "./traffic";

/**
 * The sections this app declares, in manifest order.
 *
 * THREE (epic memql#4885, design D1): Map, Deployables, Settings. Actions,
 * Sites and Packages retired together, because creating a deployable, giving
 * it an address and reading where it came from took three sections and two
 * mental models; one list and one page replaced them.
 *
 * MAP IS FIRST, and therefore the section a window opens on. It is the app's
 * signature surface -- the whole reason the earlier epic exists is that "what
 * serves where" is a shape rather than a table -- and the default-landing
 * preference below is what takes somebody who disagrees straight to the list
 * instead.
 *
 * NO SECTION CARRIES A ROLE. `v1:platform:site` and `v1:platform:package`
 * declare the composite tier, so every signed-in person has deployables and
 * sources of their own to read and the engine decides how far the list
 * reaches. The write half -- New deployable, and every act on the page -- is
 * gated INSIDE the section at rank >= 200, exactly as Sites gated publishing
 * rather than the list; that gate is presentation over the Go hostname policy
 * and the engine's own write guards, never the boundary.
 *
 * Exported rather than written twice, because the manifest and the settings
 * picker must offer the SAME set: a second literal is one that can disagree,
 * and a preference naming a section the manifest does not declare leaves the
 * window on the first section with the nav highlighting nothing -- which reads
 * as a broken app rather than as a stale preference.
 */
export const DEPLOYABLES_SECTIONS: OsAppSection[] = [
  { id: "map", name: "Map" },
  { id: "deployables", name: "Deployables" },
  // The app's slice of the cluster's logs (epic memql#4895): the lines it
  // tagged and the lines about the things it owns. Admin-floored because
  // every read on the log store is (spec L3), and this is the ONE section
  // whose floor is not this app's to choose -- which is also why the compose
  // restructure (epic memql#4885) kept it while retiring Sites, Packages and
  // Actions: those three were this app's own reading of its subject and were
  // replaced by one, and this one is a shell convention every app carries.
  { id: "logs", name: "Logs", roles: { min: "admin" } },
  { id: "settings", name: "Settings" },
];

export const DEPLOYABLES_SECTION_IDS = DEPLOYABLES_SECTIONS.map((s) => s.id);

/**
 * The sections that no longer exist, and where each one's readers went.
 *
 * A person's stored `defaultSection` may still name one of these, and the
 * sanitiser maps it rather than dropping it: a preference naming a section
 * that is gone must not open that section (the window would land on the
 * first one with the nav highlighting nothing), and it must not be silently
 * reset to the map either -- somebody who asked for the list is still asking
 * for the list, and the list is now Deployables. All three map there because
 * all three were readings of what Deployables now holds: the sites, the
 * sources they came from, and the writes that make one.
 */
export const RETIRED_SECTIONS: Readonly<Record<string, string>> = {
  sites: "deployables",
  packages: "deployables",
  actions: "deployables",
};

/** Rows per screen, as a reading choice rather than a data one. */
export type ListDensity = "comfortable" | "compact";

export const LIST_DENSITIES: readonly ListDensity[] = ["comfortable", "compact"];

export interface DeployablesSettings {
  version: 1;
  /** The section the app navigates to when its window opens. */
  defaultSection: string;
  /**
   * How tightly the Deployables list packs.
   *
   * A VIEW setting and not a filter: it changes nothing about which rows are
   * read or shown, so flipping it re-baselines no arrival cue and costs no
   * round trip. That is why it is safe to be instant.
   */
  density: ListDensity;
  /**
   * The traffic window a deployable's page opens on.
   *
   * ONE CHOICE FOR EVERY DEPLOYABLE, not one per site. Somebody troubleshooting
   * moves between deployables asking the same question, and a per-site memory
   * would make them re-pick the window on each one -- which is the clicking
   * this field exists to stop.
   */
  trafficWindow: TrafficWindow;
  /**
   * The source groups a person has opened, by group id.
   *
   * THE OPEN SET RATHER THAN THE CLOSED ONE, because closed is the default: a
   * list of what is shut would have to name every source that has ever
   * existed, and a source added tomorrow would arrive open.
   */
  expandedSources: string[];
}

export const DEPLOYABLES_SETTINGS_KEY = "memql-os-deployables-v1";

export const DEFAULT_DEPLOYABLES_SETTINGS: DeployablesSettings = {
  version: 1,
  // Map first, per the manifest. Named here as well because this value is what
  // a corrupt or absent document falls back to, and falling back to "whatever
  // is first in an array" would move with an unrelated edit.
  defaultSection: "map",
  density: "comfortable",
  // THE HOUR, not the day. A person opening a deployable is nearly always
  // checking on it -- "is it up", which is the hour's question -- rather than
  // measuring it. The day and the week stay one click away, and a click is
  // remembered.
  trafficWindow: "hour",
  // Nothing open. With sources in their own section, a closed group is one
  // line naming the source and how many apps it carries, which is the shape
  // the list is for.
  expandedSources: [],
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

  // A retired section is mapped BEFORE the membership check, so a document
  // written before the restructure lands on the list rather than on the map.
  const named =
    typeof doc.defaultSection === "string" ? (RETIRED_SECTIONS[doc.defaultSection] ?? doc.defaultSection) : "";
  const defaultSection = DEPLOYABLES_SECTION_IDS.includes(named)
    ? named
    : DEFAULT_DEPLOYABLES_SETTINGS.defaultSection;

  const density =
    doc.density !== undefined && LIST_DENSITIES.includes(doc.density as ListDensity)
      ? (doc.density as ListDensity)
      : DEFAULT_DEPLOYABLES_SETTINGS.density;

  const trafficWindow =
    doc.trafficWindow !== undefined && TRAFFIC_WINDOWS.includes(doc.trafficWindow as TrafficWindow)
      ? (doc.trafficWindow as TrafficWindow)
      : DEFAULT_DEPLOYABLES_SETTINGS.trafficWindow;

  // Entries are filtered rather than the list being rejected: a stored id is
  // a key groups are looked up by, so a stray number never matches anything
  // and presents as "expanding does not stick" rather than as a bad document.
  const expandedSources = Array.isArray(doc.expandedSources)
    ? (doc.expandedSources as unknown[]).filter((v): v is string => typeof v === "string")
    : [...DEFAULT_DEPLOYABLES_SETTINGS.expandedSources];

  return { version: 1, defaultSection, density, trafficWindow, expandedSources };
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
