import type { ModuleId } from "../../system/modules";
import type { OsAppSection } from "../../system/registry";

// The Materializer's own settings, in their own versioned store.
//
// A SEPARATE KEY FROM THE DESKTOP DOCUMENT, for the reason every app
// since Fleet has one: the shell's `DesktopDocument` rejects a version it
// does not know, so folding per-app preferences into it would mean
// somebody loses their desks because an app learned a checkbox.

/** Manifest order also supplies the names shown in preferences. */
export const MATERIALIZER_SECTIONS: OsAppSection[] = [
  { id: "materialized", name: "Materialized" },
  { id: "composer", name: "Make a file", parent: "materialized" },
  { id: "templates", name: "Templates" },
  { id: "recipes", name: "Recipes" },
  // Admin-floored because every read on the log store is (spec L3). The
  // one section whose floor is not this app's to choose.
  { id: "logs", name: "Logs", requires: "app:materializer/logs" },
  { id: "settings", name: "Settings" },
];

export const MATERIALIZER_OPENING_SECTIONS = [
  "materialized",
  "composer",
  "templates",
  "recipes",
];

export const MATERIALIZER_SECTION_IDS = MATERIALIZER_SECTIONS.map((s) => s.id);

export interface MaterializerSettings {
  version: 1;
  /** The section the app navigates to when its window opens. */
  defaultSection: string;
  /** The format a new file starts on. */
  defaultFormat: string;
  /**
   * Whether archived compositions are listed (rule 4: a micro-preference
   * lives in Settings, never as a checkbox over content).
   *
   * OFF by default, unlike Work's "show finished runs" and for the
   * opposite reason: a finished run is the ordinary end of every run
   * there is, while an archived composition is one somebody deliberately
   * filed away. Hiding what was filed away is what filing it away MEANT.
   */
  showArchived: boolean;
  /**
   * Whether the ingredient tray offers concepts that declare no
   * `@composable`.
   *
   * OFF by default, and the default is the point: the mark exists so a
   * person meets the handful of concepts worth composing from rather
   * than the cluster's whole schema. On, the unmarked ones follow the
   * marked ones and say which they are -- because the mark is a ranking
   * and a hint, never a gate.
   */
  showUnmarkedConcepts: boolean;
}

export const DEFAULT_MATERIALIZER_SETTINGS: MaterializerSettings = {
  version: 1,
  defaultSection: "materialized",
  defaultFormat: "markdown",
  showArchived: false,
  showUnmarkedConcepts: false,
};

export interface MaterializerSettingsStore {
  load(): MaterializerSettings;
  save(next: MaterializerSettings): void;
}

// HYPHENATED, like every other storage key in this shell -- `memql-os-theme`,
// `memql-os-pending-auth`, `memql-os-logs-session-v1`. The dotted form this
// started as was the only one in the tree, and the shape is what made
// gitleaks' generic-api-key heuristic read `const STORAGE_KEY = "..."` as a
// token. It was a false positive either way, and the cheap fix is also the
// consistent one: stop looking like a secret rather than allowlist a finding.
const STORAGE_KEY = "memql-os-materializer-v1";

/**
 * localStorage, sanitised on the way IN rather than trusted because it
 * was stored.
 *
 * Every read and write is wrapped: a private window, cleared site data
 * and a browser set to block site data each make the accessor throw, and
 * an app that could not read its preferences must still render.
 */
export interface SettingsStorage {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
}

export class LocalMaterializerSettingsStore implements MaterializerSettingsStore {
  // INJECTABLE FOR TESTS, which is the whole reason the parameter exists
  // -- nothing in the shell passes one. It is the shape every app's own
  // store since Fleet takes.
  constructor(private readonly storage?: SettingsStorage) {}

  private get store(): SettingsStorage | null {
    if (this.storage) return this.storage;
    try {
      return window.localStorage;
    } catch {
      return null;
    }
  }

  load(): MaterializerSettings {
    try {
      const raw = this.store?.getItem(STORAGE_KEY) ?? null;
      if (!raw) return { ...DEFAULT_MATERIALIZER_SETTINGS };
      return sanitizeSettings(JSON.parse(raw));
    } catch {
      return { ...DEFAULT_MATERIALIZER_SETTINGS };
    }
  }

  save(next: MaterializerSettings): void {
    try {
      this.store?.setItem(STORAGE_KEY, JSON.stringify(sanitizeSettings(next)));
    } catch {
      // A preference that could not be saved is a preference that does
      // not persist. It is not a reason to fail the interaction that
      // produced it.
    }
  }
}

/**
 * Repairs a stored document into a usable one.
 *
 * A `defaultSection` naming a section this build does not declare falls
 * back rather than being kept: a preference pointing at a section the
 * manifest lacks navigates the window nowhere, and that failure is
 * silent.
 */
export function sanitizeSettings(raw: unknown): MaterializerSettings {
  const out = { ...DEFAULT_MATERIALIZER_SETTINGS };
  if (typeof raw !== "object" || raw === null) return out;
  const doc = raw as Record<string, unknown>;

  const section =
    typeof doc["defaultSection"] === "string" ? doc["defaultSection"] : "";
  if (MATERIALIZER_OPENING_SECTIONS.includes(section))
    out.defaultSection = section;

  const format =
    typeof doc["defaultFormat"] === "string" ? doc["defaultFormat"] : "";
  if (FORMAT_IDS.includes(format)) out.defaultFormat = format;

  if (typeof doc["showArchived"] === "boolean")
    out.showArchived = doc["showArchived"];
  if (typeof doc["showUnmarkedConcepts"] === "boolean") {
    out.showUnmarkedConcepts = doc["showUnmarkedConcepts"];
  }
  return out;
}

const FORMAT_IDS = ["markdown", "html", "txt", "csv", "json", "docx", "pdf"];

/** Storage is needed for every file; Inference is needed only for a composed file. */
export const MATERIALIZER_REQUIRES: readonly ModuleId[] = ["storage"];
export const MATERIALIZER_WANTS: readonly ModuleId[] = ["ai"];
