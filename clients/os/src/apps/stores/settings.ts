// The Stores app's own settings, and the store that keeps them.
//
// Same discipline as Fleet's and Deployables' (`apps/*/settings.ts`), and a
// separate key for the same reason: the SHELL's DesktopDocument rejects a
// version it does not know, so folding per-app preferences into it would mean
// somebody loses their desks because an app learned a checkbox.

import type { OsAppSection } from "../../system/registry";

/**
 * The sections this app declares, in manifest order.
 *
 * EVERY SECTION IS OWNER-FLOORED, NOT ADMIN, and that is the engine's reading
 * carried onto the surface rather than a caution of this app's own.
 * `v1:shopify:store` declares `@rowAuthz(clusterOwner)`, and both declared
 * reads carry `actor.isClusterOwner` as an explicit conjunct -- so below owner
 * a read comes back EMPTY, and `createStore` / `setStoreStatus` are refused.
 * A section offered to an admin would render an empty list and a form whose
 * every submission fails, which reads as a broken app rather than as a
 * boundary. The floor here is PRESENTATION over gates the engine holds; it is
 * never the boundary itself.
 *
 * The floor is written on the SECTIONS rather than on the app, deliberately:
 * `logs` carries the shell's own admin floor, and an app-level owner floor
 * would silently raise that one too -- the app would then be the one place in
 * the shell where an admin cannot follow a fault. Owner outranks admin, so
 * the two floors compose the way they read.
 */
export const STORES_SECTIONS: OsAppSection[] = [
  { id: "stores", name: "Stores", roles: { min: "owner" } },
  // The app's slice of the cluster's logs (epic memql#4895): the lines it
  // tagged and the lines about the things it owns. Admin-floored because
  // every read on the log store is (spec L3), and this is the ONE section
  // whose floor is not this app's to choose.
  { id: "logs", name: "Logs", roles: { min: "admin" } },
  { id: "settings", name: "Settings", roles: { min: "owner" } },
];

export const STORES_SECTION_IDS = STORES_SECTIONS.map((s) => s.id);

export interface StoresSettings {
  version: 1;
  /** The section the app navigates to when its window opens. */
  defaultSection: string;
  /**
   * Whether the per-domain sync table hides domains with nothing to say --
   * idle, no drift, no stale writes, nothing tombstoned.
   *
   * OFF by default, so the table is the complete diagnostic it claims to be.
   * It is a PREFERENCE rather than a checkbox over the table (DESIGN.md rules
   * 4 and 10): the mirror is 65 concepts, and an operator who only ever wants
   * the ones that are moving should say so once rather than re-say it on
   * every store page. The table's empty state points back here when hiding is
   * why it is empty.
   */
  hideQuietDomains: boolean;
}

export const STORES_SETTINGS_KEY = "memql-os-stores-v1";

export const DEFAULT_STORES_SETTINGS: StoresSettings = {
  version: 1,
  // Stores first, per the manifest. Named here as well because this value is
  // what a corrupt or absent document falls back to, and falling back to
  // "whatever is first in an array" would move with an unrelated edit.
  defaultSection: "stores",
  hideQuietDomains: false,
};

/**
 * Bring a parsed document back to invariants.
 *
 * Every field is checked INDEPENDENTLY and repaired to its default, rather
 * than the document being rejected wholesale on the first bad value: a
 * garbage `defaultSection` must not cost the operator their table choice. A
 * wrong `version` IS wholesale, because the field names cannot be trusted at
 * all then.
 */
export function sanitizeStoresSettings(raw: unknown): StoresSettings {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ...DEFAULT_STORES_SETTINGS };
  }
  const doc = raw as Partial<StoresSettings>;
  if (doc.version !== 1) return { ...DEFAULT_STORES_SETTINGS };

  const defaultSection =
    typeof doc.defaultSection === "string" && STORES_SECTION_IDS.includes(doc.defaultSection)
      ? doc.defaultSection
      : DEFAULT_STORES_SETTINGS.defaultSection;

  return {
    version: 1,
    defaultSection,
    hideQuietDomains:
      typeof doc.hideQuietDomains === "boolean"
        ? doc.hideQuietDomains
        : DEFAULT_STORES_SETTINGS.hideQuietDomains,
  };
}

export interface StoresSettingsStore {
  load(): StoresSettings;
  save(settings: StoresSettings): void;
}

export class LocalStoresSettingsStore implements StoresSettingsStore {
  /** Pass `null` for "no storage" (the default only replaces undefined),
   *  which is the shape the desktop store takes for the same reason: a
   *  private window and a full quota are normal cases, not failures. */
  constructor(
    private readonly storage: Pick<Storage, "getItem" | "setItem"> | null | undefined = globalThis.localStorage,
    private readonly key: string = STORES_SETTINGS_KEY,
  ) {}

  load(): StoresSettings {
    try {
      const raw = this.storage?.getItem(this.key);
      if (!raw) return { ...DEFAULT_STORES_SETTINGS };
      return sanitizeStoresSettings(JSON.parse(raw));
    } catch {
      // Unparseable JSON is exactly the corrupt case sanitize exists for; it
      // just never gets that far, so the fallback is repeated here.
      return { ...DEFAULT_STORES_SETTINGS };
    }
  }

  save(settings: StoresSettings): void {
    try {
      this.storage?.setItem(this.key, JSON.stringify(settings));
    } catch {
      // Best-effort: a preference is not worth failing an interaction over.
    }
  }
}
