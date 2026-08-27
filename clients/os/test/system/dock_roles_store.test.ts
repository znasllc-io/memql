import { describe, expect, it } from "vitest";

import { initialShell, openApp, resetIdsForTest } from "../../src/system/desks";
import { emptySurface, addItem } from "../../src/system/desktop";
import { dockOrder, emptyDock, isPinned, movePin, pin, unpin } from "../../src/system/dock";
import { roleAdmits, ROLE_LADDER } from "../../src/system/roles";
import {
  documentFromState,
  LocalDesktopStore,
  sanitizeDocument,
  type DesktopDocument,
} from "../../src/system/store";

describe("dock pins", () => {
  it("pin -> unpin round-trips and stays idempotent", () => {
    let d = pin(emptyDock(), "artifacts");
    d = pin(d, "artifacts");
    expect(d.pinned).toEqual(["artifacts"]);
    expect(isPinned(d, "artifacts")).toBe(true);
    d = unpin(d, "artifacts");
    expect(d.pinned).toEqual([]);
  });

  it("movePin reorders within bounds and ignores unpinned ids", () => {
    let d = pin(pin(pin(emptyDock(), "a"), "b"), "c");
    d = movePin(d, "c", 0);
    expect(d.pinned).toEqual(["c", "a", "b"]);
    d = movePin(d, "a", 99);
    expect(d.pinned).toEqual(["c", "b", "a"]);
    expect(movePin(d, "zz", 0)).toBe(d);
  });

  it("dockOrder lists pins first, then running-unpinned in open order", () => {
    const d = pin(pin(emptyDock(), "settings"), "fleet");
    expect(dockOrder(d, ["artifacts", "settings", "users"])).toEqual([
      "settings",
      "fleet",
      "artifacts",
      "users",
    ]);
  });
});

describe("role predicate", () => {
  it("admits along the ladder, inclusive at the minimum", () => {
    for (const [i, role] of ROLE_LADDER.entries()) {
      for (const [j, min] of ROLE_LADDER.entries()) {
        expect(roleAdmits(role, { min })).toBe(i >= j);
      }
    }
  });

  it("no requirement admits everyone; unknown roles unlock nothing gated", () => {
    expect(roleAdmits("", undefined)).toBe(true);
    expect(roleAdmits("mystery", undefined)).toBe(true);
    expect(roleAdmits("mystery", { min: "reader" })).toBe(false);
    expect(roleAdmits("", { min: "reader" })).toBe(false);
  });
});

describe("desktop store", () => {
  function memoryStorage(): Pick<Storage, "getItem" | "setItem"> & { data: Map<string, string> } {
    const data = new Map<string, string>();
    return {
      data,
      getItem: (k) => data.get(k) ?? null,
      setItem: (k, v) => void data.set(k, v),
    };
  }

  function sampleDocument(): DesktopDocument {
    resetIdsForTest();
    const shell = openApp(initialShell(), "artifacts").state;
    const surface = addItem(
      emptySurface(),
      { kind: "file", id: "f1", artifactId: "a1", title: "notes", fileKind: "file", source: "uploaded" },
      { col: 0, row: 0 },
      { cols: 8, rows: 5 },
    )!;
    return documentFromState(shell, { [shell.activeDeskId]: surface }, { pinned: ["artifacts"] }, "graphite");
  }

  it("round-trips a document through storage", () => {
    const storage = memoryStorage();
    const store = new LocalDesktopStore(storage);
    const doc = sampleDocument();
    store.save(doc);
    expect(store.load()).toEqual(doc);
  });

  it("returns null for absent, corrupt, or wrong-version payloads", () => {
    const storage = memoryStorage();
    const store = new LocalDesktopStore(storage);
    expect(store.load()).toBeNull();
    storage.setItem("memql-os-desktop-v1", "{not json");
    expect(store.load()).toBeNull();
    storage.setItem("memql-os-desktop-v1", JSON.stringify({ version: 99 }));
    expect(store.load()).toBeNull();
  });

  it("sanitize coerces uploading files to failed and drops orphans", () => {
    const doc = sampleDocument();
    const deskId = doc.activeDeskId;
    const raw = JSON.parse(JSON.stringify(doc)) as DesktopDocument;
    const fileItem = raw.surfaces[deskId].items.f1;
    if (fileItem.kind === "file") fileItem.uploadState = "uploading";
    (raw.surfaces[deskId].positions as Record<string, unknown>).ghost = { col: 1, row: 1 };
    const clean = sanitizeDocument(raw)!;
    const cleanFile = clean.surfaces[deskId].items.f1;
    expect(cleanFile.kind === "file" && cleanFile.uploadState).toBe("failed");
    expect(clean.surfaces[deskId].positions.ghost).toBeUndefined();
  });

  it("sanitize repairs a dangling activeDeskId", () => {
    const doc = sampleDocument();
    const raw = { ...doc, activeDeskId: "desk-elsewhere" };
    const clean = sanitizeDocument(raw)!;
    expect(clean.activeDeskId).toBe(doc.desks[0].id);
  });

  it("save never throws when storage is unavailable", () => {
    const store = new LocalDesktopStore(null);
    expect(() => store.save(sampleDocument())).not.toThrow();
    expect(store.load()).toBeNull();
  });
});
