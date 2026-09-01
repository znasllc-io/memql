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
): GridPos | null {
  const occupied = occupiedCells(surface, except);
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
