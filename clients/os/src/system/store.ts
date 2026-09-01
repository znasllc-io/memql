// DesktopStore (spec D11): the persistence seam. v1 is versioned
// localStorage; GraphDesktopStore (system/graphStore.ts) implements this
// same interface over a v1:os:desktop row with local kept as the offline
// cache. The document carries desks, surfaces (items/folders/widget
// placements), dock pins and the theme pack -- never windows.

import type { Desk, ShellState } from "./desks";
import type { DeskSurface, FileEntry, GridPos } from "./desktop";
import type { DockState } from "./dock";
import type { DeskId } from "./windows";
import { isBuiltInId } from "../themes/builtins";
import { validateThemePack, type OsThemePack } from "../themes/pack";

export interface DesktopDocument {
  version: 1;
  desks: Array<Pick<Desk, "id" | "createdBy">>;
  activeDeskId: DeskId;
  surfaces: Record<DeskId, DeskSurface>;
  dock: DockState;
  themePack: string;
  /**
   * Theme packs installed on this desktop (epic memql#4745). BUILT-INS ARE
   * NEVER IN HERE -- the bundle already carries them, and a stored copy that
   * outlived a release would be a stale theme nobody could update.
   *
   * `version` is deliberately NOT bumped for this field, which is the
   * precedent the legacy icon-group lift above set: a bump makes
   * `sanitizeDocument` reject every document written by an older bundle, and
   * somebody would lose their desks because a theme list arrived. A new field
   * is added, sanitized to a default, and read.
   *
   * It lives in the DESKTOP document rather than a per-surface store because
   * it has to roam: graphStore strips only `activeDeskId` from what it shares,
   * so a theme installed on a laptop is on the desktop at work -- and a stored
   * `themePack` naming a pack that did not travel with it would resolve back
   * to graphite on arrival, which reads as the choice being forgotten.
   */
  installedPacks: OsThemePack[];
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
  installedPacks: OsThemePack[] = [],
): DesktopDocument {
  return {
    version: 1,
    desks: shell.desks.map((d) => ({ id: d.id, createdBy: d.createdBy })),
    activeDeskId: shell.activeDeskId,
    surfaces,
    dock,
    themePack,
    installedPacks,
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
    // Children of LEGACY icon-group folders, waiting for a cell near their
    // group's old position (the design D4 migration -- see below).
    const lifts: Array<{ entry: FileEntry; near: GridPos }> = [];
    const rawPositions = (surface.positions ?? {}) as Record<string, { col?: unknown; row?: unknown }>;
    const posOf = (id: string): GridPos => {
      const pos = rawPositions[id];
      return pos && typeof pos.col === "number" && typeof pos.row === "number"
        ? { col: pos.col, row: pos.row }
        : { col: 0, row: 0 };
    };
    for (const [id, item] of Object.entries(surface.items ?? {})) {
      if (!item || typeof item !== "object" || (item as { id?: unknown }).id !== id) continue;
      if (item.kind === "file" && item.uploadState === "uploading") {
        items[id] = { ...item, uploadState: "failed" };
      } else if (item.kind === "folder") {
        // TWO SHAPES UNDER ONE KIND (design D4). The unified shortcut carries
        // `folderId` and holds nothing; the foundation's icon-group carried
        // its files INSIDE `children`. The migration lifts those children
        // back onto the grid as plain shortcuts -- the `deleteFolder` shape
        // -- so nobody loses a shortcut to the rename. A folder that is
        // neither shape is dropped: keeping it would render a control whose
        // popover can show nothing.
        const legacy = item as unknown as { children?: unknown; folderId?: unknown; name?: unknown };
        if (typeof legacy.folderId === "string" && legacy.folderId !== "" && typeof legacy.name === "string") {
          items[id] = { kind: "folder", id, folderId: legacy.folderId, name: legacy.name };
        } else if (Array.isArray(legacy.children)) {
          const near = posOf(id);
          for (const child of legacy.children) {
            if (!child || typeof child !== "object") continue;
            const c = child as Partial<FileEntry> & { id?: unknown };
            if (typeof c.id !== "string" || c.id === "") continue;
            lifts.push({
              near,
              entry: {
                id: c.id,
                artifactId: typeof c.artifactId === "string" ? c.artifactId : "",
                title: typeof c.title === "string" && c.title !== "" ? c.title : c.id,
                fileKind: typeof c.fileKind === "string" ? c.fileKind : "file",
                source: typeof c.source === "string" ? c.source : "",
                ...(typeof c.producedByWorkerId === "string" && c.producedByWorkerId !== ""
                  ? { producedByWorkerId: c.producedByWorkerId }
                  : {}),
                // An upload cannot survive a reload in either shape.
                ...(c.uploadState ? { uploadState: "failed" as const } : {}),
              },
            });
          }
        }
      } else if (item.kind === "file" || item.kind === "widget") {
        items[id] = item;
      }
    }
    const positions: DeskSurface["positions"] = {};
    for (const [id, pos] of Object.entries(surface.positions ?? {})) {
      if (!items[id] || !pos || typeof pos.col !== "number" || typeof pos.row !== "number") continue;
      positions[id] = { col: pos.col, row: pos.row };
    }
    // Place every lifted child: an item with no position renders nowhere,
    // which would be exactly the lost shortcut this pass exists to prevent.
    // Ring scan from the group's old cell; the grid's true size is unknown
    // here (viewport-dependent), so only non-negativity bounds the scan.
    const occupied = new Set(Object.values(positions).map((p) => `${p.col}:${p.row}`));
    for (const lift of lifts) {
      if (items[lift.entry.id]) continue;
      let placed: GridPos | null = null;
      for (let radius = 0; placed === null && radius <= 64; radius += 1) {
        for (let dc = -radius; dc <= radius && placed === null; dc += 1) {
          for (let dr = -radius; dr <= radius; dr += 1) {
            if (Math.max(Math.abs(dc), Math.abs(dr)) !== radius) continue;
            const col = lift.near.col + dc;
            const row = lift.near.row + dr;
            if (col < 0 || row < 0 || occupied.has(`${col}:${row}`)) continue;
            placed = { col, row };
            break;
          }
        }
      }
      if (placed === null) continue;
      occupied.add(`${placed.col}:${placed.row}`);
      items[lift.entry.id] = { kind: "file", ...lift.entry };
      positions[lift.entry.id] = placed;
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
    // Each pack is re-validated on the way IN, not trusted because it was
    // stored: this document arrives from localStorage on one machine and from
    // a graph row on the next, and a pack that was valid when it was written
    // may have been written by a bundle whose token set has since grown. One
    // unreadable pack is dropped; the rest of the desktop is untouched.
    installedPacks: Array.isArray(doc.installedPacks)
      ? doc.installedPacks
          .map((pack) => validateThemePack(pack))
          .flatMap((load) => (load.ok && !isBuiltInId(load.pack.id) ? [load.pack] : []))
      : [],
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
