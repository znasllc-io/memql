// DesktopStore (spec D11): the persistence seam. v1 is versioned
// localStorage; GraphDesktopStore (system/graphStore.ts) implements this
// same interface over a v1:os:desktop row with local kept as the offline
// cache. The document carries desks, surfaces (items/folders/widget
// placements), dock pins and the theme pack -- never windows.

import type { Desk, ShellState } from "./desks";
import type { DeskSurface } from "./desktop";
import type { DockState } from "./dock";
import type { DeskId } from "./windows";

export interface DesktopDocument {
  version: 1;
  desks: Array<Pick<Desk, "id" | "createdBy">>;
  activeDeskId: DeskId;
  surfaces: Record<DeskId, DeskSurface>;
  dock: DockState;
  themePack: string;
}

/**
 * Something the store learned that the shell did not do.
 *
 * `document` carries a desktop the shell must take on: `hydrate` is the
 * first one to resolve after boot (the shell was showing the local cache or
 * a seed), `remote` is a later one, which means another machine saved.
 * Only `remote` is worth telling the person about.
 *
 * `stale` says the stored desktop is from a NEWER version of this app than
 * the one running here, so this session has stopped writing to the graph
 * rather than overwrite a document it cannot read.
 */
export type DesktopStoreEvent =
  | { kind: "document"; document: DesktopDocument; origin: "hydrate" | "remote" }
  | { kind: "stale" };

export interface DesktopStore {
  /** Null = nothing usable stored (absent, corrupt, or wrong version). */
  load(): DesktopDocument | null;
  save(doc: DesktopDocument): void;
  /**
   * Optional: the direction a store cannot express through `load()`, which
   * is synchronous and answers once. A store with a remote half announces
   * documents it did not receive from `save()` here. Returns an unsubscribe.
   *
   * `load()` STAYS SYNCHRONOUS, and that is the reason this exists rather
   * than an async load: the shell has to paint a desktop on the first frame,
   * offline included, and awaiting the cluster to do it would make every
   * boot wait on a round trip that may never complete.
   */
  subscribe?(listener: (event: DesktopStoreEvent) => void): () => void;
  /** Optional: send anything held back by a debounce, now. */
  flush?(): void;
}

export const DESKTOP_STORE_KEY = "memql-os-desktop-v1";

export function documentFromState(
  shell: ShellState,
  surfaces: Record<DeskId, DeskSurface>,
  dock: DockState,
  themePack: string,
): DesktopDocument {
  return {
    version: 1,
    desks: shell.desks.map((d) => ({ id: d.id, createdBy: d.createdBy })),
    activeDeskId: shell.activeDeskId,
    surfaces,
    dock,
    themePack,
  };
}

/**
 * Bring a parsed document back to invariants: at least one desk, a valid
 * active desk, no orphan surfaces or positions, and no "uploading" file --
 * an upload cannot survive a reload, so it comes back as "failed" rather
 * than as a spinner that spins forever.
 */
export function sanitizeDocument(raw: unknown): DesktopDocument | null {
  if (raw === null || typeof raw !== "object") return null;
  const doc = raw as Partial<DesktopDocument>;
  if (doc.version !== 1) return null;
  if (!Array.isArray(doc.desks) || doc.desks.length === 0) return null;

  const desks = doc.desks
    .filter((d): d is Pick<Desk, "id" | "createdBy"> => !!d && typeof d.id === "string")
    .map((d) => ({ id: d.id, createdBy: d.createdBy === "auto" ? ("auto" as const) : ("user" as const) }));
  if (desks.length === 0) return null;

  const first = desks[0];
  if (!first) return null;
  const deskIds = new Set(desks.map((d) => d.id));
  const activeDeskId = deskIds.has(doc.activeDeskId ?? "") ? (doc.activeDeskId as DeskId) : first.id;

  const surfaces: Record<DeskId, DeskSurface> = {};
  for (const [deskId, surface] of Object.entries(doc.surfaces ?? {})) {
    if (!deskIds.has(deskId) || !surface || typeof surface !== "object") continue;
    const items: DeskSurface["items"] = {};
    for (const [id, item] of Object.entries(surface.items ?? {})) {
      if (!item || typeof item !== "object" || (item as { id?: unknown }).id !== id) continue;
      if (item.kind === "file" && item.uploadState === "uploading") {
        items[id] = { ...item, uploadState: "failed" };
      } else if (item.kind === "file" || item.kind === "folder" || item.kind === "widget") {
        items[id] = item;
      }
    }
    const positions: DeskSurface["positions"] = {};
    for (const [id, pos] of Object.entries(surface.positions ?? {})) {
      if (!items[id] || !pos || typeof pos.col !== "number" || typeof pos.row !== "number") continue;
      positions[id] = { col: pos.col, row: pos.row };
    }
    surfaces[deskId] = { items, positions };
  }

  const pinned = Array.isArray(doc.dock?.pinned)
    ? doc.dock.pinned.filter((id): id is string => typeof id === "string")
    : [];

  return {
    version: 1,
    desks,
    activeDeskId,
    surfaces,
    dock: { pinned },
    themePack: typeof doc.themePack === "string" && doc.themePack ? doc.themePack : "graphite",
  };
}

export class LocalDesktopStore implements DesktopStore {
  /** Pass `null` for "no storage" (the default only replaces undefined). */
  constructor(
    private readonly storage: Pick<Storage, "getItem" | "setItem"> | null | undefined = globalThis.localStorage,
    private readonly key: string = DESKTOP_STORE_KEY,
  ) {}

  load(): DesktopDocument | null {
    try {
      const raw = this.storage?.getItem(this.key);
      if (!raw) return null;
      return sanitizeDocument(JSON.parse(raw));
    } catch {
      return null;
    }
  }

  save(doc: DesktopDocument): void {
    try {
      this.storage?.setItem(this.key, JSON.stringify(doc));
    } catch {
      // Best-effort: a desktop layout is not worth failing an interaction
      // over (private windows and full quotas are normal cases).
    }
  }
}
