import { describe, expect, it, vi } from "vitest";

import { planRestore, restoreNote, runRestore } from "../../src/apps/bin/restore";
import { binItemFromArtifact, binItemFromFolder } from "../../src/apps/bin/rows";
import { artifactFromRow, folderFromRow } from "../../src/apps/files/rows";
import { artifactRow, folderRow } from "../files/harness";

// Taking something back out of the Bin.
//
// The property that matters is the PAIR: archiving an artifact archives its
// backing file through an automation, and the mirror of that automation cannot
// exist without closing a cycle -- so the client has to run both writes, and a
// restore that ran only one leaves the two rows disagreeing about whether the
// thing is in the Bin.

function ports() {
  return {
    calls: [] as string[],
    restoreArtifact: vi.fn(),
    restoreFile: vi.fn(),
    restoreFolder: vi.fn(),
  };
}

describe("planning a restore", () => {
  it("names BOTH rows for a file, because the archive touched both", () => {
    const item = binItemFromArtifact(
      artifactFromRow(artifactRow({ id: "a-1", kind: "file", sourceConceptRef: "v1:library:file:f-1" })),
    );
    expect(planRestore(item)).toEqual({ artifactId: "a-1", fileId: "f-1", folderId: "" });
  });

  it("names only the index row for a kind with no backing file", () => {
    const item = binItemFromArtifact(
      artifactFromRow(
        artifactRow({ id: "a-2", kind: "document", sourceConceptRef: "v1:knowledge:document:d-1" }),
      ),
    );
    expect(planRestore(item)).toEqual({ artifactId: "a-2", fileId: "", folderId: "" });
  });

  it("names the folder mutation for a folder and neither of the others", () => {
    const item = binItemFromFolder(folderFromRow(folderRow({ id: "fo-1", name: "Clients" })));
    expect(planRestore(item)).toEqual({ artifactId: "", fileId: "", folderId: "fo-1" });
  });
});

describe("running a restore", () => {
  it("un-archives the INDEX first, so a refusal on the file leaves something visible to re-run", async () => {
    const p = ports();
    const order: string[] = [];
    p.restoreArtifact.mockImplementation(async () => void order.push("artifact"));
    p.restoreFile.mockImplementation(async () => void order.push("file"));
    await runRestore({ artifactId: "a-1", fileId: "f-1", folderId: "" }, p);
    expect(order).toEqual(["artifact", "file"]);
  });

  it("stops at the first refusal rather than pressing on", async () => {
    const p = ports();
    p.restoreArtifact.mockRejectedValue(new Error("refused"));
    await expect(
      runRestore({ artifactId: "a-1", fileId: "f-1", folderId: "" }, p),
    ).rejects.toThrow("refused");
    expect(p.restoreFile).not.toHaveBeenCalled();
  });

  it("touches nothing but the folder mutation for a folder", async () => {
    const p = ports();
    await runRestore({ artifactId: "", fileId: "", folderId: "fo-1" }, p);
    expect(p.restoreFolder).toHaveBeenCalledWith("fo-1");
    expect(p.restoreArtifact).not.toHaveBeenCalled();
    expect(p.restoreFile).not.toHaveBeenCalled();
  });
});

describe("what the surface promises before the click", () => {
  it("says a folder comes back EMPTY, because that is the surprising part", () => {
    const folder = binItemFromFolder(folderFromRow(folderRow({ id: "fo-1", name: "Clients" })));
    expect(restoreNote(folder)).toMatch(/stays here until you restore it too/);
  });

  it("says a file keeps everything, because that is the reassuring part", () => {
    const file = binItemFromArtifact(artifactFromRow(artifactRow({ id: "a-1" })));
    expect(restoreNote(file)).toMatch(/history, its labels and everywhere it came from/);
  });
});
