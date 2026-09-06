import { describe, expect, it } from "vitest";

import {
  MAX_FOLDER_DEPTH,
  foldBinRail,
  foldFolderTree,
  subtreeFolderIds,
  type TreeNode,
} from "../../src/apps/files/fold";
import type { ArtifactRow, FolderRow } from "../../src/apps/files/rows";

// The tree fold (design B4): a pure function from the folders snapshot to the
// picture the rail draws. Cycle-tolerant, orphan-tolerant, depth-capped,
// stable-sorted -- the collection folds events in cluster order, and a picture
// that depends on input order reshuffles while somebody is watching it.

function folder(over: Partial<FolderRow> & { id: string }): FolderRow {
  return {
    id: over.id,
    name: over.name ?? over.id,
    parentFolderId: over.parentFolderId ?? "",
    archived: over.archived ?? false,
    deleted: over.deleted ?? false,
  };
}

function names(nodes: TreeNode[]): string[] {
  return nodes.map((n) => n.folder.name);
}

function byName(nodes: TreeNode[], name: string): TreeNode {
  const hit = nodes.find((n) => n.folder.name === name);
  if (!hit) throw new Error(`no node named ${name} among ${names(nodes).join(", ")}`);
  return hit;
}

describe("foldFolderTree", () => {
  it("nests children under parents, sorted by name then id at every level", () => {
    const tree = foldFolderTree([
      folder({ id: "f-b", name: "Beta" }),
      folder({ id: "f-a", name: "Alpha" }),
      folder({ id: "f-a2", name: "Nested B", parentFolderId: "f-a" }),
      folder({ id: "f-a1", name: "Nested A", parentFolderId: "f-a" }),
    ]);
    expect(names(tree.roots)).toEqual(["Alpha", "Beta"]);
    const alpha = byName(tree.roots, "Alpha");
    expect(names(alpha.children)).toEqual(["Nested A", "Nested B"]);
    expect(alpha.children.every((c) => c.placement === "nested")).toBe(true);
  });

  it("orders sibling name duplicates by id, so the picture never reshuffles", () => {
    const rows = [
      folder({ id: "f-2", name: "Client" }),
      folder({ id: "f-1", name: "Client" }),
    ];
    const forward = foldFolderTree(rows);
    const reversed = foldFolderTree([...rows].reverse());
    expect(forward.roots.map((n) => n.folder.id)).toEqual(["f-1", "f-2"]);
    expect(reversed.roots.map((n) => n.folder.id)).toEqual(["f-1", "f-2"]);
  });

  it("renders a folder whose parent is missing at root with the orphan marker", () => {
    const tree = foldFolderTree([
      folder({ id: "f-lost", name: "Lost", parentFolderId: "f-archived-away" }),
      folder({ id: "f-ok", name: "Ok" }),
    ]);
    expect(names(tree.roots).sort()).toEqual(["Lost", "Ok"]);
    expect(byName(tree.roots, "Lost").placement).toBe("orphan");
    expect(byName(tree.roots, "Ok").placement).toBe("nested");
  });

  it("keeps an orphan's own subtree nested beneath it", () => {
    const tree = foldFolderTree([
      folder({ id: "f-lost", name: "Lost", parentFolderId: "gone" }),
      folder({ id: "f-child", name: "Child", parentFolderId: "f-lost" }),
    ]);
    const lost = byName(tree.roots, "Lost");
    expect(names(lost.children)).toEqual(["Child"]);
    expect(byName(lost.children, "Child").placement).toBe("nested");
  });

  it("breaks a cycle to root with the cycle marker, every folder rendered exactly once", () => {
    const tree = foldFolderTree([
      folder({ id: "f-a", name: "A", parentFolderId: "f-b" }),
      folder({ id: "f-b", name: "B", parentFolderId: "f-a" }),
      folder({ id: "f-c", name: "C", parentFolderId: "f-b" }),
    ]);
    // One of the pair is displaced to root (deterministically the first by
    // name/id); the rest of the loop nests beneath it.
    const a = byName(tree.roots, "A");
    expect(a.placement).toBe("cycle");
    expect(names(a.children)).toEqual(["B"]);
    expect(names(byName(a.children, "B").children)).toEqual(["C"]);
    const rendered: string[] = [];
    const walk = (nodes: TreeNode[]) => {
      for (const n of nodes) {
        rendered.push(n.folder.id);
        walk(n.children);
      }
    };
    walk(tree.roots);
    expect(rendered.sort()).toEqual(["f-a", "f-b", "f-c"]);
  });

  it("displaces a folder past the depth cap to root with the deep marker", () => {
    const chain: FolderRow[] = [folder({ id: "f-0", name: "d0" })];
    for (let i = 1; i <= MAX_FOLDER_DEPTH + 1; i += 1) {
      chain.push(folder({ id: `f-${i}`, name: `d${i}`, parentFolderId: `f-${i - 1}` }));
    }
    const tree = foldFolderTree(chain);
    // Depths 0..MAX nest; the one past the cap re-roots with the marker.
    const deep = byName(tree.roots, `d${MAX_FOLDER_DEPTH + 1}`);
    expect(deep.placement).toBe("deep");
    let node = byName(tree.roots, "d0");
    for (let i = 1; i <= MAX_FOLDER_DEPTH; i += 1) node = byName(node.children, `d${i}`);
    expect(node.children).toEqual([]);
  });

  it("folds the empty snapshot to an empty tree", () => {
    expect(foldFolderTree([]).roots).toEqual([]);
  });
});

describe("subtreeFolderIds", () => {
  it("returns the folder and its descendants, children before parents", () => {
    const tree = foldFolderTree([
      folder({ id: "f-top", name: "Top" }),
      folder({ id: "f-mid", name: "Mid", parentFolderId: "f-top" }),
      folder({ id: "f-leaf", name: "Leaf", parentFolderId: "f-mid" }),
      folder({ id: "f-other", name: "Other" }),
    ]);
    const ids = subtreeFolderIds(tree, "f-top");
    expect(ids).toEqual(["f-leaf", "f-mid", "f-top"]);
  });

  it("answers just the folder itself for an unknown or leaf id", () => {
    const tree = foldFolderTree([folder({ id: "f-solo", name: "Solo" })]);
    expect(subtreeFolderIds(tree, "f-solo")).toEqual(["f-solo"]);
    expect(subtreeFolderIds(tree, "f-nope")).toEqual(["f-nope"]);
  });
});

// ---------------------------------------------------------------------------
// The Bin's fold (epic memql#4981)
// ---------------------------------------------------------------------------

function archivedFile(over: Partial<ArtifactRow> & { id: string }): ArtifactRow {
  return {
    id: over.id,
    ownerUserId: "u-me",
    lens: "artifact",
    kind: over.kind ?? "file",
    source: "uploaded",
    sourceConceptRef: "",
    title: over.title ?? over.id,
    summary: "",
    format: "",
    mimeType: "",
    labels: [],
    accountIds: [],
    folderId: over.folderId ?? "",
    archived: true,
    producedByPlanId: "",
    producedByWorkerId: "",
    producedByWorkerName: "",
    validationStatus: "",
    createdAt: over.createdAt ?? "2026-08-20T10:00:00Z",
  };
}

describe("foldBinRail", () => {
  it("files each archived row under the archived folder it was filed in", () => {
    const bin = foldBinRail(
      [
        archivedFile({ id: "a-1", title: "cut-03.mov", folderId: "fo-1" }),
        archivedFile({ id: "a-2", title: "brief.pdf" }),
      ],
      [folder({ id: "fo-1", name: "Old campaign", archived: true })],
    );
    expect(bin.folders.map((n) => n.folder.id)).toEqual(["fo-1"]);
    expect(bin.folders.map((n) => n.files.map((r) => r.id))).toEqual([["a-1"]]);
    expect(bin.loose.map((r) => r.id)).toEqual(["a-2"]);
  });

  it("treats a file archived out of a LIVE folder as loose", () => {
    // Its folder is not in the Bin, so there is nowhere in the Bin to draw it
    // under. Filing it beneath a folder that is not there would invent a
    // location; showing it at the place's own level is the honest answer to
    // "where does this sit in the Bin" -- nowhere in particular.
    const bin = foldBinRail(
      [archivedFile({ id: "a-1", title: "cut-03.mov", folderId: "fo-live" })],
      [],
    );
    expect(bin.folders).toEqual([]);
    expect(bin.loose.map((r) => r.id)).toEqual(["a-1"]);
  });

  it("keeps a folder whose files were all restored, with nothing under it", () => {
    const bin = foldBinRail([], [folder({ id: "fo-1", name: "Old campaign", archived: true })]);
    expect(bin.folders.map((n) => n.folder.id)).toEqual(["fo-1"]);
    expect(bin.folders.map((n) => n.files)).toEqual([[]]);
    expect(bin.folderCount).toBe(1);
    expect(bin.fileCount).toBe(0);
  });

  it("orders folders and files by name then id, never by arrival", () => {
    // The collection folds events in the order the cluster sent them, so an
    // order that depended on input order would reshuffle under somebody
    // reading it. Ties break on the id because sibling name duplicates are
    // allowed -- folders are collections, not namespaces.
    const bin = foldBinRail(
      [
        archivedFile({ id: "a-z", title: "zebra.txt" }),
        archivedFile({ id: "a-b", title: "apple.txt" }),
        archivedFile({ id: "a-a", title: "apple.txt" }),
      ],
      [
        folder({ id: "fo-2", name: "Vault", archived: true }),
        folder({ id: "fo-1", name: "Archive box", archived: true }),
      ],
    );
    expect(bin.folders.map((n) => n.folder.name)).toEqual(["Archive box", "Vault"]);
    expect(bin.loose.map((r) => r.id)).toEqual(["a-a", "a-b", "a-z"]);
  });

  it("counts files and folders separately, because the Bin holds both", () => {
    const bin = foldBinRail(
      [
        archivedFile({ id: "a-1", folderId: "fo-1" }),
        archivedFile({ id: "a-2", folderId: "fo-1" }),
        archivedFile({ id: "a-3" }),
      ],
      [folder({ id: "fo-1", archived: true })],
    );
    // A folder and the files inside it are three separate things a person can
    // take back, which is exactly how the Bin app lists them.
    expect(bin.fileCount).toBe(3);
    expect(bin.folderCount).toBe(1);
  });

  it("sorts an untitled file under the name the rail will actually show", () => {
    // `artifactName` falls back to the id, and the fold orders by that same
    // function rather than by the raw title -- otherwise every untitled row
    // would sort under an empty string and sit at the top of the group, in a
    // position its visible name does not explain.
    const bin = foldBinRail(
      [archivedFile({ id: "zz-9", title: "" }), archivedFile({ id: "b-1", title: "aa.txt" })],
      [],
    );
    expect(bin.loose.map((r) => r.id)).toEqual(["b-1", "zz-9"]);
  });
});
