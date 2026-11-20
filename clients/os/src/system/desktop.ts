// The desk surface: files, folders and widgets on a snap grid, one surface
// per desk (spec B). Pure and serializable -- the DesktopStore persists
// these directly. Grid geometry (columns/rows) is viewport-dependent and
// passed in by the chrome; positions saved on a larger grid re-place via
// nearestFreeCell on a smaller one.

export type ItemId = string;

export interface GridPos {
  col: number;
  row: number;
}

export interface GridSize {
  cols: number;
  rows: number;
}

export interface FileEntry {
  id: ItemId;
  artifactId: string;
  title: string;
  /** Library artifact kind (document / generated_output / file / ...). */
  fileKind: string;
  /** Library provenance source (uploaded / computer_use / ...). */
  source: string;
  producedByWorkerId?: string;
  /** The Library's recorded format, which names the glyph's kind when the
   *  title has no extension. Absent on shortcuts made before it was kept. */
  format?: string;
  /** Transient: present while an upload is in flight or has failed. */
  uploadState?: "uploading" | "failed";
}

export type DesktopItem =
  | ({ kind: "file" } & FileEntry)
  // A desk folder is a SHORTCUT to a Library folder (design D4): the popover
  // is a live view of the Library's rows, so the item itself holds nothing --
  // only the pointer and a denormalized name that refreshes opportunistically
  // from live rows. Removing it removes the shortcut and never archives.
  | { kind: "folder"; id: ItemId; folderId: string; name: string }
  | { kind: "widget"; id: ItemId; widgetId: string; w: number; h: number };

export interface DeskSurface {
  items: Record<ItemId, DesktopItem>;
  positions: Record<ItemId, GridPos>;
}

export function emptySurface(): DeskSurface {
  return { items: {}, positions: {} };
}

export function surfaceHasContent(surface: DeskSurface | undefined): boolean {
  return !!surface && Object.keys(surface.items).length > 0;
}

function spanOf(item: DesktopItem): { w: number; h: number } {
  return item.kind === "widget" ? { w: item.w, h: item.h } : { w: 1, h: 1 };
}

function cellsOf(item: DesktopItem, pos: GridPos): GridPos[] {
  const span = spanOf(item);
  const cells: GridPos[] = [];
  for (let dc = 0; dc < span.w; dc += 1) {
    for (let dr = 0; dr < span.h; dr += 1) {
      cells.push({ col: pos.col + dc, row: pos.row + dr });
    }
  }
  return cells;
}

function keyOf(pos: GridPos): string {
  return `${pos.col}:${pos.row}`;
}

export function occupiedCells(surface: DeskSurface, except?: ItemId): Set<string> {
  const occupied = new Set<string>();
  for (const [id, item] of Object.entries(surface.items)) {
    if (id === except) continue;
    const pos = surface.positions[id];
    if (!pos) continue;
    for (const cell of cellsOf(item, pos)) occupied.add(keyOf(cell));
  }
  return occupied;
}

function fits(item: DesktopItem, pos: GridPos, grid: GridSize, occupied: Set<string>): boolean {
  const span = spanOf(item);
  if (pos.col < 0 || pos.row < 0) return false;
  if (pos.col + span.w > grid.cols || pos.row + span.h > grid.rows) return false;
  return cellsOf(item, pos).every((cell) => !occupied.has(keyOf(cell)));
}

/**
 * The nearest free cell to `preferred`, scanning outward ring by ring
 * (column-major within a ring, deterministic). Null when the grid is full.
 */
export function nearestFreeCell(
  surface: DeskSurface,
  item: DesktopItem,
  preferred: GridPos,
  grid: GridSize,
  except?: ItemId,
  blocked: readonly GridPos[] = [],
): GridPos | null {
  const occupied = occupiedCells(surface, except);
  for (const cell of blocked) occupied.add(keyOf(cell));
  const clamp = (pos: GridPos): GridPos => ({
    col: Math.max(0, Math.min(grid.cols - spanOf(item).w, pos.col)),
    row: Math.max(0, Math.min(grid.rows - spanOf(item).h, pos.row)),
  });
  const start = clamp(preferred);
  const maxRadius = Math.max(grid.cols, grid.rows);
  for (let radius = 0; radius <= maxRadius; radius += 1) {
    for (let dc = -radius; dc <= radius; dc += 1) {
      for (let dr = -radius; dr <= radius; dr += 1) {
        if (Math.max(Math.abs(dc), Math.abs(dr)) !== radius) continue;
        const pos = { col: start.col + dc, row: start.row + dr };
        if (fits(item, pos, grid, occupied)) return pos;
      }
    }
  }
  return null;
}

/** Add an item at the nearest free cell to `preferred`. Null = grid full. */
export function addItem(
  surface: DeskSurface,
  item: DesktopItem,
  preferred: GridPos,
  grid: GridSize,
): DeskSurface | null {
  const pos = nearestFreeCell(surface, item, preferred, grid);
  if (!pos) return null;
  return {
    items: { ...surface.items, [item.id]: item },
    positions: { ...surface.positions, [item.id]: pos },
  };
}

/** Move an item toward `to`, settling on the nearest free cell. */
export function moveItem(
  surface: DeskSurface,
  id: ItemId,
  to: GridPos,
  grid: GridSize,
): DeskSurface {
  const item = surface.items[id];
  if (!item) return surface;
  const pos = nearestFreeCell(surface, item, to, grid, id);
  if (!pos) return surface;
  return { ...surface, positions: { ...surface.positions, [id]: pos } };
}

/**
 * A Library file dropped on this desk at `cell` (dragged out of Files). A desk
 * icon is a shortcut, so a file that already has one here is never given a
 * second: that icon moves to the drop instead (Jesús, Nov 20 2026). Otherwise
 * a new icon takes the nearest free cell. `hidden` are cells a window or the
 * dock covers right now: an icon is never dropped where nobody can see it
 * land. Null = no free cell in view. The drag's landing spot reads the same
 * answer, so the spot is where the icon lands.
 */
export function placeFileAt(
  surface: DeskSurface,
  entry: FileEntry,
  cell: GridPos,
  grid: GridSize,
  hidden: readonly GridPos[] = [],
): { surface: DeskSurface; itemId: ItemId } | null {
  const existing = Object.values(surface.items).find(
    (i) => i.kind === "file" && entry.artifactId !== "" && i.artifactId === entry.artifactId,
  );
  const item: DesktopItem = existing ?? { kind: "file", ...entry };
  const pos = nearestFreeCell(surface, item, cell, grid, item.id, hidden);
  if (!pos) return null;
  return {
    surface: { items: { ...surface.items, [item.id]: item }, positions: { ...surface.positions, [item.id]: pos } },
    itemId: item.id,
  };
}

export function removeItem(surface: DeskSurface, id: ItemId): DeskSurface {
  if (!surface.items[id]) return surface;
  const items = { ...surface.items };
  const positions = { ...surface.positions };
  delete items[id];
  delete positions[id];
  return { items, positions };
}

export function updateFile(
  surface: DeskSurface,
  id: ItemId,
  patch: Partial<Omit<FileEntry, "id">>,
): DeskSurface {
  const item = surface.items[id];
  if (!item || item.kind !== "file") return surface;
  return { ...surface, items: { ...surface.items, [id]: { ...item, ...patch, kind: "file", id } } };
}

// ---- folders (shortcuts to Library folders, spec D4) ----

/** Refresh a folder shortcut's denormalized fields (the name, from live
 *  rows). Removal is plain `removeItem`: a shortcut holds nothing to return
 *  to the grid, and removing it never touches the Library. */
export function updateFolder(
  surface: DeskSurface,
  id: ItemId,
  patch: Partial<Pick<Extract<DesktopItem, { kind: "folder" }>, "name">>,
): DeskSurface {
  const item = surface.items[id];
  if (!item || item.kind !== "folder") return surface;
  if (patch.name === undefined || patch.name === item.name) return surface;
  return { ...surface, items: { ...surface.items, [id]: { ...item, name: patch.name } } };
}

/**
 * The surface as THIS grid can hold it: every item whose saved spot fits stays
 * put, and every one that does not (saved on a wider grid -- a wider window, or
 * before the desktop scaled up, item 16) moves to the nearest free spot that
 * does. Returns the same object when everything fits. The shell draws and edits
 * through this, so an item is never painted past the screen edge; an edit then
 * saves the spots it was edited at.
 */
export function fitSurface(surface: DeskSurface, grid: GridSize): DeskSurface {
  const outside = Object.entries(surface.items).filter(([id, item]) => {
    const pos = surface.positions[id];
    return pos && !fits(item, pos, grid, new Set());
  });
  if (outside.length === 0) return surface;
  let fitted: DeskSurface = { items: surface.items, positions: { ...surface.positions } };
  // Placed against the items that stay, so a moved item never covers one.
  for (const [id] of outside) delete fitted.positions[id];
  for (const [id, item] of outside) {
    const to = nearestFreeCell(fitted, item, surface.positions[id]!, grid);
    // ponytail: a full grid leaves the item unplaced (not drawn) until room frees up.
    if (to) fitted = { ...fitted, positions: { ...fitted.positions, [id]: to } };
  }
  return fitted;
}

/** Repack every item top-left, files and folders sorted by title/name. */
export function sortSurface(surface: DeskSurface, grid: GridSize): DeskSurface {
  const entries = Object.values(surface.items);
  const label = (item: DesktopItem): string =>
    item.kind === "file" ? item.title : item.kind === "folder" ? item.name : `~widget-${item.widgetId}`;
  const ordered = [...entries].sort((a, b) => {
    if ((a.kind === "widget") !== (b.kind === "widget")) return a.kind === "widget" ? 1 : -1;
    return label(a).localeCompare(label(b));
  });
  let next: DeskSurface = emptySurface();
  for (const item of ordered) {
    const placed = addItem(next, item, { col: 0, row: 0 }, grid);
    if (!placed) return surface;
    next = placed;
  }
  return next;
}
