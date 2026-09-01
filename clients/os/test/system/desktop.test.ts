import { describe, expect, it } from "vitest";

import {
  addItem,
  emptySurface,
  moveItem,
  nearestFreeCell,
  removeItem,
  sortSurface,
  surfaceHasContent,
  updateFile,
  updateFolder,
  type DeskSurface,
  type DesktopItem,
} from "../../src/system/desktop";

const GRID = { cols: 8, rows: 5 };

function file(id: string, title = id): DesktopItem {
  return { kind: "file", id, artifactId: `art-${id}`, title, fileKind: "file", source: "uploaded" };
}

function widget(id: string, w = 2, h = 2): DesktopItem {
  return { kind: "widget", id, widgetId: `w-${id}`, w, h };
}

function place(surface: DeskSurface, item: DesktopItem, col: number, row: number): DeskSurface {
  const next = addItem(surface, item, { col, row }, GRID);
  if (!next) throw new Error("grid full in test setup");
  return next;
}

describe("desk surface grid", () => {
  it("places at the preferred cell when free", () => {
    const s = place(emptySurface(), file("a"), 2, 1);
    expect(s.positions.a).toEqual({ col: 2, row: 1 });
  });

  it("settles on the nearest free cell when the preferred is taken", () => {
    let s = place(emptySurface(), file("a"), 0, 0);
    s = place(s, file("b"), 0, 0);
    expect(s.positions.b!).not.toEqual({ col: 0, row: 0 });
    const d = Math.max(Math.abs(s.positions.b!.col), Math.abs(s.positions.b!.row));
    expect(d).toBe(1);
  });

  it("a widget occupies its full span for collision", () => {
    let s = place(emptySurface(), widget("w"), 0, 0); // covers 2x2
    s = place(s, file("a"), 1, 1); // inside the span -> pushed out
    const p = s.positions.a!;
    expect(p.col > 1 || p.row > 1).toBe(true);
  });

  it("clamps a widget so its span stays inside the grid", () => {
    const pos = nearestFreeCell(emptySurface(), widget("w", 3, 2), { col: 7, row: 4 }, GRID);
    expect(pos).not.toBeNull();
    expect(pos!.col + 3).toBeLessThanOrEqual(GRID.cols);
    expect(pos!.row + 2).toBeLessThanOrEqual(GRID.rows);
  });

  it("returns null when the grid is truly full", () => {
    let s = emptySurface();
    for (let c = 0; c < GRID.cols; c += 1)
      for (let r = 0; r < GRID.rows; r += 1) s = place(s, file(`f-${c}-${r}`), c, r);
    expect(addItem(s, file("overflow"), { col: 0, row: 0 }, GRID)).toBeNull();
  });

  it("moveItem relocates and refuses nothing (settles nearby)", () => {
    let s = place(emptySurface(), file("a"), 0, 0);
    s = place(s, file("b"), 3, 3);
    s = moveItem(s, "a", { col: 3, row: 3 }, GRID);
    expect(s.positions.a).not.toEqual({ col: 0, row: 0 });
    expect(s.positions.a).not.toEqual(s.positions.b);
  });

  it("updateFile patches file fields in place", () => {
    let s = place(emptySurface(), { ...file("a"), uploadState: "uploading" } as DesktopItem, 0, 0);
    s = updateFile(s, "a", { uploadState: undefined, artifactId: "art-real" });
    const item = s.items.a!;
    expect(item.kind === "file" && item.artifactId).toBe("art-real");
  });
});

describe("folders are shortcuts (design D4)", () => {
  const shortcut: DesktopItem = { kind: "folder", id: "dir", folderId: "f-lib-1", name: "Reports" };

  it("places like any one-cell item and removes without touching anything else", () => {
    let s = place(emptySurface(), file("a"), 0, 0);
    s = place(s, shortcut, 1, 0);
    expect(s.positions.dir).toEqual({ col: 1, row: 0 });
    // Remove-from-desk removes the SHORTCUT only -- there are no children to
    // return, because the contents live in the Library, not on the desk.
    s = removeItem(s, "dir");
    expect(s.items.dir).toBeUndefined();
    expect(s.items.a!.kind).toBe("file");
  });

  it("updateFolder refreshes the denormalized name opportunistically", () => {
    let s = place(emptySurface(), shortcut, 0, 0);
    s = updateFolder(s, "dir", { name: "Client reports" });
    const item = s.items.dir!;
    expect(item.kind === "folder" && item.name).toBe("Client reports");
    // A patch aimed at a non-folder is a no-op, not a corruption.
    s = place(s, file("a"), 1, 0);
    expect(updateFolder(s, "a", { name: "x" })).toBe(s);
  });
});

describe("sorting and content", () => {
  it("sortSurface packs files alphabetically before widgets", () => {
    let s = place(emptySurface(), file("z", "zeta"), 5, 4);
    s = place(s, file("a", "alpha"), 3, 3);
    s = place(s, widget("w"), 0, 0);
    s = sortSurface(s, GRID);
    expect(s.positions.a).toEqual({ col: 0, row: 0 });
    expect(s.positions.z).toEqual({ col: 0, row: 1 });
  });

  it("surfaceHasContent is false only for truly empty surfaces", () => {
    expect(surfaceHasContent(emptySurface())).toBe(false);
    expect(surfaceHasContent(undefined)).toBe(false);
    expect(surfaceHasContent(place(emptySurface(), file("a"), 0, 0))).toBe(true);
  });

  it("removeItem clears both the item and its position", () => {
    let s = place(emptySurface(), file("a"), 0, 0);
    s = removeItem(s, "a");
    expect(s.items.a).toBeUndefined();
    expect(s.positions.a).toBeUndefined();
  });
});
