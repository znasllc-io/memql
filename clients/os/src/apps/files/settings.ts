import type { OsAppSection } from "../../system/registry";

// The Files app's own settings, and the store that keeps them (design D4).
//
// A separate versioned key with the desktop store's discipline -- sanitized on
// read, every storage touch in a try/catch, corrupt or wrong-version values
// silently reset to defaults -- for the reason fleet/settings.ts states: an
// app learning a checkbox must never cost anyone their desks.

/** The sections this app declares, in manifest order. Exported because the
 *  manifest and the gear must offer the same set. */
export const FILES_SECTIONS: OsAppSection[] = [
  { id: "browse", name: "Browse" },
  // Backups lives in Files rather than in Fleet, and the split is about what
  // the thing IS. Fleet is machines and how work is routed to them; a backup
  // is a folder, its destination is a Library folder two panes away, and the
  // per-file states it rolls up are the ones the browse already renders.
  { id: "backups", name: "Backups" },
  // The app's slice of the cluster's logs (epic memql#4895): the lines it
  // tagged and the lines about the things it owns. Admin-floored because
  // every read on the log store is (spec L3), and this is the ONE section
  // whose floor is not this app's to choose.
  { id: "logs", name: "Logs", roles: { min: "admin" } },
  { id: "settings", name: "Settings" },
];

export type DefaultSort = "newest" | "oldest";

export interface FilesSettings {
  version: 1;
  /** The order the list opens in. The toolbar toggle changes the session,
   *  not this. */
  defaultSort: DefaultSort;
  /**
   * Whether archive asks first (default ON). Consumed by the archive flows --
   * a single file's confirm and the folder walk's count-naming confirm alike.
   */
  confirmBeforeArchive: boolean;
}

export const FILES_SETTINGS_KEY = "memql-os-files-v1";

export const DEFAULT_FILES_SETTINGS: FilesSettings = {
  version: 1,
  defaultSort: "newest",
  confirmBeforeArchive: true,
};

/**
 * Bring a parsed document back to invariants. Every field is checked
 * INDEPENDENTLY and repaired to its default -- a garbage sort must not cost
 * the confirm-before-archive choice. A wrong `version` is wholesale, because
 * the field names cannot be trusted at all then.
 */
export function sanitizeFilesSettings(raw: unknown): FilesSettings {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ...DEFAULT_FILES_SETTINGS };
  }
  const doc = raw as Partial<FilesSettings>;
  if (doc.version !== 1) return { ...DEFAULT_FILES_SETTINGS };
  return {
    version: 1,
    defaultSort:
      doc.defaultSort === "newest" || doc.defaultSort === "oldest"
        ? doc.defaultSort
        : DEFAULT_FILES_SETTINGS.defaultSort,
    confirmBeforeArchive:
      typeof doc.confirmBeforeArchive === "boolean"
        ? doc.confirmBeforeArchive
        : DEFAULT_FILES_SETTINGS.confirmBeforeArchive,
  };
}

export interface FilesSettingsStore {
  load(): FilesSettings;
  save(settings: FilesSettings): void;
}

export class LocalFilesSettingsStore implements FilesSettingsStore {
  /** Pass `null` for "no storage" (the default only replaces undefined) --
   *  a private window and a full quota are normal cases, not failures. */
  constructor(
    private readonly storage: Pick<Storage, "getItem" | "setItem"> | null | undefined = globalThis.localStorage,
    private readonly key: string = FILES_SETTINGS_KEY,
  ) {}

  load(): FilesSettings {
    try {
      const raw = this.storage?.getItem(this.key);
      if (!raw) return { ...DEFAULT_FILES_SETTINGS };
      return sanitizeFilesSettings(JSON.parse(raw));
    } catch {
      return { ...DEFAULT_FILES_SETTINGS };
    }
  }

  save(settings: FilesSettings): void {
    try {
      this.storage?.setItem(this.key, JSON.stringify(settings));
    } catch {
      // Best-effort: a preference is not worth failing an interaction over.
    }
  }
}
