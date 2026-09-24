// The Deployables app's own settings, and the store that keeps them.
//
// Same discipline as Fleet's and Users' (`apps/*/settings.ts`), and a separate
// key for the same reason: the SHELL's DesktopDocument rejects a version it
// does not know, so folding per-app preferences into it would mean somebody
// loses their desks because an app learned a checkbox.

import type { ModuleId } from "../../system/modules";
import type { OsAppSection } from "../../system/registry";
import { TRAFFIC_WINDOWS, type TrafficWindow } from "./traffic";

/** Overview is the fixed landing section; Settings and Logs stay in the window chrome. */
export const DEPLOYABLES_SECTIONS: OsAppSection[] = [
  { id: "map", name: "Overview" },
  { id: "deployables", name: "Deployables" },
  { id: "sources", name: "Sources" },
  // The app's slice of the cluster's logs (epic memql#4895): the lines it
  // tagged and the lines about the things it owns. Admin-floored because
  // every read on the log store is (spec L3), and this is the ONE section
  // whose floor is not this app's to choose -- which is also why the compose
  // restructure (epic memql#4885) kept it while retiring Sites, Packages and
  // Actions: those three were this app's own reading of its subject and were
  // replaced by one, and this one is a shell convention every app carries.
  { id: "logs", name: "Logs", requires: "app:deployables/logs" },
  { id: "settings", name: "Settings" },
];

export const DEPLOYABLES_SECTION_IDS = DEPLOYABLES_SECTIONS.map((s) => s.id);

export interface DeployablesSettings {
  version: 1;
  /**
   * The traffic window a deployable's page opens on.
   *
   * ONE CHOICE FOR EVERY DEPLOYABLE, not one per site. Somebody troubleshooting
   * moves between deployables asking the same question, and a per-site memory
   * would make them re-pick the window on each one -- which is the clicking
   * this field exists to stop.
   */
  trafficWindow: TrafficWindow;
  /** Legacy expanded-source preference, retained for document compatibility. */
  expandedSources: string[];
  /** Composition shows relationships by default; explicit collapses persist. */
  collapsedSources?: string[];
}

export const DEPLOYABLES_SETTINGS_KEY = "memql-os-deployables-v1";

export const DEFAULT_DEPLOYABLES_SETTINGS: DeployablesSettings = {
  version: 1,
  // THE HOUR, not the day. A person opening a deployable is nearly always
  // checking on it -- "is it up", which is the hour's question -- rather than
  // measuring it. The day and the week stay one click away, and a click is
  // remembered.
  trafficWindow: "hour",
  // Composition shows source branches until explicitly collapsed.
  expandedSources: [],
};

/** Keep only current view preferences; removed controls do not survive in saved documents. */
export function sanitizeDeployablesSettings(raw: unknown): DeployablesSettings {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ...DEFAULT_DEPLOYABLES_SETTINGS };
  }
  const doc = raw as Partial<DeployablesSettings>;
  if (doc.version !== 1) return { ...DEFAULT_DEPLOYABLES_SETTINGS };

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

  return { version: 1, trafficWindow, expandedSources,
    ...(Array.isArray(doc.collapsedSources) ? { collapsedSources: doc.collapsedSources.filter((v): v is string => typeof v === "string") } : {}) };
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

/** Personal GitHub connections determine the app's Settings mark. Cluster
 * infrastructure remains diagnosed by the operation that needs it. */
export const DEPLOYABLES_REQUIRES: readonly ModuleId[] = [];
export const DEPLOYABLES_WANTS: readonly ModuleId[] = [];
