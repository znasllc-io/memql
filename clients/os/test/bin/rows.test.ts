import { describe, expect, it } from "vitest";

import {
  binFingerprint,
  binItemFromArtifact,
  binItemFromFolder,
  filterBinItems,
  orderBinItems,
  type BinItem,
} from "../../src/apps/bin/rows";
import { artifactFromRow, folderFromRow } from "../../src/apps/files/rows";
import { artifactRow, folderRow } from "../files/harness";

// The Bin's projection and ordering, on fixtures. Pure, so the list, the
// detail panel and the restore plan are all checked against the same rows.

describe("projecting into the Bin", () => {
  it("takes the backing file id off a file artifact and nothing off the others", () => {
    const file = binItemFromArtifact(
      artifactFromRow(artifactRow({ id: "a-1", kind: "file", sourceConceptRef: "v1:library:file:f-1" })),
    );
    expect(file.fileId).toBe("f-1");

    // A note has no bytes and no machine. An empty id here is what stops the
    // detail panel offering a machine block for something that never had one.
    const doc = binItemFromArtifact(
      artifactFromRow(
        artifactRow({ id: "a-2", kind: "document", sourceConceptRef: "v1:knowledge:document:d-1" }),
      ),
    );
    expect(doc.fileId).toBe("");
  });

  it("names a folder by its name, and falls back to the id rather than blank", () => {
    expect(binItemFromFolder(folderFromRow(folderRow({ id: "fo-1", name: "Clients" }))).name).toBe("Clients");
    // A nameless row is indistinguishable from a row that failed to render.
    expect(binItemFromFolder(folderFromRow(folderRow({ id: "fo-2", name: "" }))).name).toBe("fo-2");
  });
});

describe("the Bin's order", () => {
  const item = (over: Partial<BinItem> & { id: string }): BinItem => ({
    kind: "artifact",
    name: over.id,
    contentKind: "file",
    source: "uploaded",
    fileId: "",
    producedByWorkerId: "",
    producedByWorkerName: "",
    producedByPlanId: "",
    labels: [],
    folderId: "",
    changedAt: "",
    ...over,
  });

  it("puts the most recently changed first, across BOTH feeds", () => {
    // Folders and artifacts arrive on separate reads. A Bin that listed every
    // folder and then every file would bury the thing somebody just threw
    // away in the middle of it.
    const ordered = orderBinItems([
      item({ id: "old-file", changedAt: "2026-08-01T10:00:00Z" }),
      item({ id: "a-folder", kind: "folder", changedAt: "" }),
      item({ id: "new-file", changedAt: "2026-09-01T10:00:00Z" }),
    ]);
    expect(ordered.map((i) => i.id)).toEqual(["new-file", "old-file", "a-folder"]);
  });

  it("is TOTAL, so a folder never swaps places under somebody watching", () => {
    // Folders carry no timestamp at all -- every one of them ties, and without
    // the id tie-break they would be free to reorder on any update.
    const a = orderBinItems([item({ id: "b", kind: "folder" }), item({ id: "a", kind: "folder" })]);
    const b = orderBinItems([item({ id: "a", kind: "folder" }), item({ id: "b", kind: "folder" })]);
    expect(a.map((i) => i.id)).toEqual(b.map((i) => i.id));
  });
});

describe("the Bin's fingerprint", () => {
  const base = binItemFromArtifact(artifactFromRow(artifactRow({ id: "a-1", title: "cut.mov" })));

  it("does not name the timestamp, so an arrival does not also announce itself as an update", () => {
    // Every archive re-versions its row, so changedAt moves for everything
    // that lands here. Naming it would ring twice for one event.
    const later = { ...base, changedAt: "2026-09-02T10:00:00Z" };
    expect(binFingerprint(later)).toBe(binFingerprint(base));
  });

  it("does name a rename and a re-filing, which are things a person did", () => {
    expect(binFingerprint({ ...base, name: "final cut.mov" })).not.toBe(binFingerprint(base));
    expect(binFingerprint({ ...base, folderId: "fo-9" })).not.toBe(binFingerprint(base));
  });
});

describe("searching the Bin", () => {
  const rows = [
    binItemFromArtifact(
      artifactFromRow(artifactRow({ id: "a-1", title: "cut-03.mov", producedByWorkerName: "MacBook-Pro" })),
    ),
    binItemFromArtifact(artifactFromRow(artifactRow({ id: "a-2", title: "invoice.pdf", labels: ["acme"] }))),
  ];

  it("matches the machine as well as the name -- 'which laptop was that from' is the question", () => {
    expect(filterBinItems(rows, "macbook").map((i) => i.id)).toEqual(["a-1"]);
    expect(filterBinItems(rows, "acme").map((i) => i.id)).toEqual(["a-2"]);
    expect(filterBinItems(rows, "invoice").map((i) => i.id)).toEqual(["a-2"]);
  });

  it("returns everything for a blank search rather than nothing", () => {
    expect(filterBinItems(rows, "   ")).toHaveLength(2);
  });
});
