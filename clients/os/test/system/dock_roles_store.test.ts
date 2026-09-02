import { beforeEach, describe, expect, it } from "vitest";

import { initialShell, openApp, resetIdsForTest } from "../../src/system/desks";
import { emptySurface, addItem } from "../../src/system/desktop";
import { dockOrder, emptyDock, isPinned, movePin, pin, unpin } from "../../src/system/dock";
import { roleAdmits, roleLadder, setRoleLadder } from "../../src/system/roles";
import { SEEDED_LADDER } from "../seededLadder";
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
  // The ladder is CLUSTER STATE now (epic memql#4832, D1), so every case here
  // installs it first. Vitest isolates per FILE rather than per test, so the
  // install has to be per-test: a case that ran after one clearing the ladder
  // would otherwise measure an empty one and pass by admitting nothing.
  beforeEach(() => setRoleLadder(SEEDED_LADDER));

  it("admits along the ladder, inclusive at the minimum", () => {
    const rungs = roleLadder();
    expect(rungs.length).toBeGreaterThan(0);
    for (const [i, actor] of rungs.entries()) {
      for (const [j, floor] of rungs.entries()) {
        expect(roleAdmits(actor.slug, { min: floor.slug })).toBe(i >= j);
      }
    }
  });

  // THE DEFECT THIS EPIC EXISTS TO CLOSE, pinned as a case rather than left
  // to the matrix above -- which would pass against EITHER ordering, since it
  // only checks the ladder is consistent with itself.
  //
  // The shell used to rank admin above developer; the engine ranks developer
  // (300) above admin (200). While the only consumer was a launcher that
  // mis-sorted an app. Under rank-based row visibility the same request gets
  // opposite answers depending on which side answers it.
  it("ranks developer ABOVE admin, as the engine does", () => {
    expect(roleAdmits("developer", { min: "admin" })).toBe(true);
    expect(roleAdmits("admin", { min: "developer" })).toBe(false);
  });

  // The legacy slugs on a user row are not the slugs the catalog seeds, and
  // the mapping is DATA on the rung rather than a table in the client.
  it("resolves the legacy slugs through their rung's aliases", () => {
    expect(roleAdmits("writer", { min: "user" })).toBe(true);
    expect(roleAdmits("reader", { min: "viewer" })).toBe(true);
    expect(roleAdmits("reader", { min: "user" })).toBe(false);
  });

  it("no requirement admits everyone; unknown roles unlock nothing gated", () => {
    expect(roleAdmits("", undefined)).toBe(true);
    expect(roleAdmits("mystery", undefined)).toBe(true);
    expect(roleAdmits("mystery", { min: "viewer" })).toBe(false);
    expect(roleAdmits("", { min: "viewer" })).toBe(false);
  });

  // A floor naming a role the cluster does not have admits NOBODY. The
  // obvious alternative -- treat an unresolvable floor as 0 -- reads
  // correctly and fails OPEN, which is the same trap the engine's own
  // rankFloorAdmits is written against.
  it("a requirement naming an unknown role admits nobody", () => {
    expect(roleAdmits("owner", { min: "nosuchrole" })).toBe(false);
    expect(roleAdmits("reader", { min: "nosuchrole" })).toBe(false);
  });

  // Before the cluster read lands there is no ordering, and every gated
  // surface stays hidden. That is the same answer an unreported actor role
  // already gets, and it must not be confused with a decision about access.
  it("admits nothing gated before the ladder loads", () => {
    setRoleLadder([]);
    expect(roleAdmits("owner", { min: "viewer" })).toBe(false);
    expect(roleAdmits("owner", undefined)).toBe(true);
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
    const fileItem = raw.surfaces[deskId]!.items.f1!;
    if (fileItem.kind === "file") fileItem.uploadState = "uploading";
    (raw.surfaces[deskId]!.positions as Record<string, unknown>).ghost = { col: 1, row: 1 };
    const clean = sanitizeDocument(raw)!;
    const cleanFile = clean.surfaces[deskId]!.items.f1!;
    expect(cleanFile.kind === "file" && cleanFile.uploadState).toBe("failed");
    expect(clean.surfaces[deskId]!.positions.ghost).toBeUndefined();
  });

  it("sanitize repairs a dangling activeDeskId", () => {
    const doc = sampleDocument();
    const raw = { ...doc, activeDeskId: "desk-elsewhere" };
    const clean = sanitizeDocument(raw)!;
    expect(clean.activeDeskId).toBe(doc.desks[0]!.id);
  });

  it("save never throws when storage is unavailable", () => {
    const store = new LocalDesktopStore(null);
    expect(() => store.save(sampleDocument())).not.toThrow();
    expect(store.load()).toBeNull();
  });
});
