import type { FolderRow } from "./rows";

// The tree fold (design B4): a pure function from the folders snapshot to the
// picture the rail draws.
//
// ===========================================================================
// THE PICTURE DEGRADES, IT NEVER BREAKS
// ===========================================================================
// Cycle refusal and the depth cap are enforced client-side at write time in
// v1 (design D11), so a hostile or buggy writer CAN produce a snapshot that
// violates both. This fold tolerates whatever arrives: a folder whose ancestry
// never reaches the root renders AT the root with a marker saying why, its own
// subtree intact beneath it, and every folder renders exactly once. A fold
// that threw -- or looped -- on bad ancestry would let one corrupt row take
// the whole app down.
//
// ===========================================================================
// STABLE ORDER, EVERYWHERE
// ===========================================================================
// The collection folds events in the order the cluster sent them, so a tree
// that depended on input order would reshuffle while somebody is watching it.
// Every sibling list sorts by (name, id) -- id second, because sibling name
// duplicates are ALLOWED (folders are collections, not namespaces).

export const MAX_FOLDER_DEPTH = 12;

export type Placement = "nested" | "orphan" | "cycle" | "deep";

export interface TreeNode {
  folder: FolderRow;
  placement: Placement;
  depth: number;
  children: TreeNode[];
}

export interface FolderTree {
  roots: TreeNode[];
  byId: Map<string, TreeNode>;
}

function byNameThenId(a: FolderRow, b: FolderRow): number {
  return a.name.localeCompare(b.name) || a.id.localeCompare(b.id);
}

export function foldFolderTree(folders: readonly FolderRow[]): FolderTree {
  const rows = folders.filter((f) => f.id !== "");
  const byRowId = new Map(rows.map((f) => [f.id, f]));
  const childrenOf = new Map<string, FolderRow[]>();
  for (const row of rows) {
    const list = childrenOf.get(row.parentFolderId) ?? [];
    list.push(row);
    childrenOf.set(row.parentFolderId, list);
  }
  for (const list of childrenOf.values()) list.sort(byNameThenId);

  const placed = new Map<string, TreeNode>();
  const roots: TreeNode[] = [];

  // Attach a folder and, breadth-down, everything reachable beneath it. The
  // `placed` check is what makes a cycle terminate: the second time a loop
  // reaches a member, it is already on screen and the walk stops.
  function attach(row: FolderRow, depth: number, placement: Placement): TreeNode {
    const node: TreeNode = { folder: row, placement, depth, children: [] };
    placed.set(row.id, node);
    for (const child of childrenOf.get(row.id) ?? []) {
      if (placed.has(child.id)) continue;
      if (depth + 1 > MAX_FOLDER_DEPTH) {
        // Past the cap: the child re-roots with the marker rather than
        // nesting into an unreadable margin. Its own subtree restarts at
        // depth 0 beneath it.
        roots.push(attach(child, 0, "deep"));
      } else {
        node.children.push(attach(child, depth + 1, "nested"));
      }
    }
    return node;
  }

  // Pass 1: the declared roots.
  for (const row of (childrenOf.get("") ?? []).filter((r) => !placed.has(r.id))) {
    roots.push(attach(row, 0, "nested"));
  }
  // Pass 2: orphans -- a parent id that names no folder in the snapshot
  // (archived away, or never arrived). The subtree stays intact beneath them.
  for (const row of [...rows].sort(byNameThenId)) {
    if (placed.has(row.id)) continue;
    if (row.parentFolderId !== "" && !byRowId.has(row.parentFolderId)) {
      roots.push(attach(row, 0, "orphan"));
    }
  }
  // Pass 3: whatever remains sits on a cycle. Break each loop at its first
  // member in sort order, deterministically, and nest the rest beneath it.
  for (const row of [...rows].sort(byNameThenId)) {
    if (!placed.has(row.id)) roots.push(attach(row, 0, "cycle"));
  }

  roots.sort((a, b) => byNameThenId(a.folder, b.folder));
  return { roots, byId: placed };
}

/**
 * The folder and every descendant, CHILDREN FIRST -- the order the archive
 * walk needs (design B5: archive contents, then folders, leaves inward). An
 * id the tree does not hold answers itself alone, so a caller acting on a
 * just-archived folder still gets a coherent (empty) walk.
 */
export function subtreeFolderIds(tree: FolderTree, folderId: string): string[] {
  const node = tree.byId.get(folderId);
  if (!node) return [folderId];
  const ids: string[] = [];
  const walk = (n: TreeNode) => {
    for (const child of n.children) walk(child);
    ids.push(n.folder.id);
  };
  walk(node);
  return ids;
}
