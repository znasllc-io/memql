// The Logs app's own settings, and the store that keeps them.
//
// Same discipline as every app before it (`apps/*/settings.ts`), and a
// separate key for the same reason: the SHELL's DesktopDocument rejects a
// version it does not know, so folding per-app preferences into it would
// mean somebody loses their desks because an app learned a checkbox.

import { LEVEL_FLOORS, type LevelFloor } from "../../logs/filters";
import type { OsAppSection } from "../../system/registry";

/**
 * The sections this app declares, in manifest order.
 *
 * STREAM IS FIRST, and therefore the section a window opens on: the store,
 * following. Search is the same store asked about a window; Settings is the
 * app's own. NO section carries a floor, because the APP does: every read on
 * the log store is admin-and-above in the engine (spec L3), so the manifest
 * carries `roles: { min: "admin" }` and `logsSection` names the Stream.
 *
 * Exported rather than written twice, because the manifest and the settings
 * picker must offer the SAME set.
 */
export const LOGS_SECTIONS: OsAppSection[] = [
  { id: "stream", name: "Stream" },
  { id: "search", name: "Search" },
  { id: "settings", name: "Settings" },
];

export const LOGS_SECTION_IDS = LOGS_SECTIONS.map((s) => s.id);

/** The sections a window may OPEN on. Settings is not one: a Logs window
 *  that opened on its own settings would be a window about itself. */
export type LogsDefaultSection = "stream" | "search";

export const LOGS_DEFAULT_SECTIONS: readonly LogsDefaultSection[] = ["stream", "search"];

/** Rows per screen, as a reading choice rather than a data one. */
export type LogsDensity = "comfortable" | "compact";

export const LOGS_DENSITIES: readonly LogsDensity[] = ["comfortable", "compact"];

/** How far back the Stream reads and offers sources for. Short on purpose:
 *  a stream is the last little while; a month is a search. */
export type StreamWindow = "15m" | "1h" | "6h";

export const STREAM_WINDOWS: readonly StreamWindow[] = ["15m", "1h", "6h"];

export interface LogsSettings {
  version: 1;
  /** The section the app navigates to when its window opens. */
  defaultSection: LogsDefaultSection;
  /** A VIEW setting, not a filter: it changes how tightly the rows pack and
   *  nothing about which lines are read or shown. */
  density: LogsDensity;
  /** The level floor a Stream or Search starts on. The session's own Refine
   *  steers afterwards. */
  levelFloor: LevelFloor;
  /** The Stream's window. */
  streamWindow: StreamWindow;
}

export const LOGS_SETTINGS_KEY = "memql-os-logs-v1";

export const DEFAULT_LOGS_SETTINGS: LogsSettings = {
  version: 1,
  // Stream first, per the manifest. Named here as well because this value is
  // what a corrupt or absent document falls back to, and falling back to
  // "whatever is first in an array" would move with an unrelated edit.
  defaultSection: "stream",
  density: "comfortable",
  levelFloor: "all",
  streamWindow: "15m",
};

function oneOf<T extends string>(value: unknown, allowed: readonly T[], fallback: T): T {
  return typeof value === "string" && (allowed as readonly string[]).includes(value) ? (value as T) : fallback;
}

/**
 * Bring a parsed document back to invariants.
 *
 * Every field is repaired INDEPENDENTLY rather than the document being
 * rejected on the first bad value: a garbage `defaultSection` must not cost
 * somebody their density choice. A wrong `version` IS wholesale, because then
 * the field names cannot be trusted at all.
 */
export function sanitizeLogsSettings(raw: unknown): LogsSettings {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ...DEFAULT_LOGS_SETTINGS };
  }
  const doc = raw as Partial<LogsSettings>;
  if (doc.version !== 1) return { ...DEFAULT_LOGS_SETTINGS };
  return {
    version: 1,
    defaultSection: oneOf(doc.defaultSection, LOGS_DEFAULT_SECTIONS, DEFAULT_LOGS_SETTINGS.defaultSection),
    density: oneOf(doc.density, LOGS_DENSITIES, DEFAULT_LOGS_SETTINGS.density),
    levelFloor: oneOf(doc.levelFloor, LEVEL_FLOORS, DEFAULT_LOGS_SETTINGS.levelFloor),
    streamWindow: oneOf(doc.streamWindow, STREAM_WINDOWS, DEFAULT_LOGS_SETTINGS.streamWindow),
  };
}

export interface LogsSettingsStore {
  load(): LogsSettings;
  save(settings: LogsSettings): void;
}

export class LocalLogsSettingsStore implements LogsSettingsStore {
  /** `null` is "no storage" -- a private window and a full quota are normal
   *  cases, not failures. The default only replaces `undefined`. */
  constructor(
    private readonly storage:
      | Pick<Storage, "getItem" | "setItem">
      | null
      | undefined = globalThis.localStorage,
    private readonly key: string = LOGS_SETTINGS_KEY,
  ) {}

  load(): LogsSettings {
    try {
      const raw = this.storage?.getItem(this.key);
      if (!raw) return { ...DEFAULT_LOGS_SETTINGS };
      return sanitizeLogsSettings(JSON.parse(raw));
    } catch {
      // Unparseable JSON is the corrupt case sanitize exists for; it just never
      // gets that far, so the fallback is repeated here.
      return { ...DEFAULT_LOGS_SETTINGS };
    }
  }

  save(settings: LogsSettings): void {
    try {
      this.storage?.setItem(this.key, JSON.stringify(settings));
    } catch {
      // Best-effort: a preference is not worth failing an interaction over.
    }
  }
}
