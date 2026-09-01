import { describe, expect, it } from "vitest";

import {
  MAX_FOLDER_DEPTH,
  foldFolderTree,
  subtreeFolderIds,
  type TreeNode,
} from "../../src/apps/files/fold";
import type { FolderRow } from "../../src/apps/files/rows";

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
