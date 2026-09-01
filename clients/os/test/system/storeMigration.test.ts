import { describe, expect, it } from "vitest";

import { sanitizeDocument, type DesktopDocument } from "../../src/system/store";

// The desk-folder migration (design D4): the foundation's local icon-groups
// held file entries INSIDE the folder item; the unified model makes a desk
// folder a shortcut to a Library folder and holds nothing. `sanitizeDocument`
// lifts legacy children back onto the grid as plain shortcuts -- the
// `deleteFolder` shape -- so nobody loses a shortcut to the rename.

function doc(over: Partial<DesktopDocument>): unknown {
  return {
    version: 1,
    desks: [{ id: "desk-1", createdBy: "user" }],
    activeDeskId: "desk-1",
    surfaces: {},
    dock: { pinned: [] },
    themePack: "graphite",
    ...over,
  };
}

const FILE_A = { kind: "file", id: "a", artifactId: "art-a", title: "a.pdf", fileKind: "file", source: "uploaded" };
const FILE_B = { kind: "file", id: "b", artifactId: "art-b", title: "b.pdf", fileKind: "file", source: "uploaded" };

describe("sanitizeDocument -- legacy icon-group migration", () => {
  it("lifts a legacy folder's children onto the grid and drops the group", () => {
    const legacy = doc({
      surfaces: {
        "desk-1": {
          items: {
            // The FOUNDATION's shape, which the current DesktopItem type no
            // longer admits -- that is the point of this fixture.
            dir: { kind: "folder", id: "dir", name: "Reports", children: [FILE_A, FILE_B] },
          },
          positions: { dir: { col: 2, row: 1 } },
        },
      },
    } as never);
    const out = sanitizeDocument(legacy);
    expect(out).not.toBeNull();
    const surface = out!.surfaces["desk-1"]!;
    expect(surface.items.dir).toBeUndefined();
    expect(surface.items.a).toMatchObject({ kind: "file", artifactId: "art-a" });
    expect(surface.items.b).toMatchObject({ kind: "file", artifactId: "art-b" });
    // Every lifted child gets a position -- an item with no position renders
    // nowhere, which would be the lost shortcut this migration exists to
    // prevent. Near the folder's own cell, no two on the same cell.
    expect(surface.positions.a).toBeDefined();
    expect(surface.positions.b).toBeDefined();
    expect(surface.positions.a).not.toEqual(surface.positions.b);
  });

  it("keeps a unified folder shortcut as it is", () => {
    const modern = doc({
      surfaces: {
        "desk-1": {
          items: {
            dir: { kind: "folder", id: "dir", folderId: "f-1", name: "Reports" },
          },
          positions: { dir: { col: 0, row: 0 } },
        },
      },
    });
    const out = sanitizeDocument(modern);
    expect(out!.surfaces["desk-1"]!.items.dir).toMatchObject({
      kind: "folder",
      folderId: "f-1",
      name: "Reports",
    });
  });

  it("drops a folder item that is neither shape rather than keeping garbage", () => {
    const broken = doc({
      surfaces: {
        "desk-1": {
          items: { dir: { kind: "folder", id: "dir", name: "??" } },
          positions: { dir: { col: 0, row: 0 } },
        },
      },
    } as never);
    const out = sanitizeDocument(broken);
    expect(out!.surfaces["desk-1"]!.items.dir).toBeUndefined();
  });
});
