import { artifactName, type ArtifactRow, type FolderRow } from "./rows";

// The rail's folds: pure functions from the snapshots to the picture the rail
// draws. The Library tree (design B4) is the first; the Bin's own fold, at the
// bottom of this file, is the second (epic memql#4981).
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

// ===========================================================================
// THE BIN'S FOLD (epic memql#4981) -- AND WHY FILES APPEAR IN THE RAIL HERE
// ===========================================================================
// The standing rule is that the rail lists folders and the list lists files:
// a second copy of the same rows in a 184px column is the same rows twice,
// narrower. That rule is about the LIBRARY, where opening a place scopes the
// list to exactly the files the rail would have shown, and it does not
// survive contact with the Bin for three reasons.
//
// 1. THE BIN HAS ALMOST NO NAVIGATION TO LIST. Archived folders are a flat,
//    ancestry-less set (their parents are a mix of live and archived, so a
//    tree over them would lie one way or the other), and most archived files
//    are loose. A folders-only Bin is a rail group that is usually empty.
//
// 2. FOLDERS-ONLY MADE THE RAIL SAY SOMETHING FALSE. A Bin holding forty
//    files and no folders expanded to "Nothing archived." -- while the list
//    beside it was showing those forty rows at that moment.
//
// 3. IT SAYS WHAT THE LIST CANNOT. The list is flat and scoped to one folder
//    at a time; the rail shows which archived files were filed in which
//    archived folder without navigating into each one. That is a different
//    fact, not a narrower copy of the same one.
//
// ORDER IS ALPHABETICAL, NOT CHRONOLOGICAL, and that is the deliberate
// disagreement with the list beside it. The list is the chronological view --
// most recently archived first, which answers "what did I just throw away".
// The rail is an INDEX: somebody scans it for a name. Sorting it by time
// would also make it reshuffle every time the list's own sort control is
// flipped, under somebody reading it.

/** One archived folder in the Bin, and the archived files filed in it. */
export interface BinFolderNode {
  folder: FolderRow;
  files: ArtifactRow[];
}

export interface BinRail {
  folders: BinFolderNode[];
  /**
   * Archived files filed nowhere that is itself in the Bin: the root, a row
   * from before folders existed, or a file archived out of a folder that is
   * still live. All three are the same answer to "where does this sit in the
   * Bin" -- nowhere in particular -- so all three sit at the place's level.
   */
  loose: ArtifactRow[];
  fileCount: number;
  folderCount: number;
}

function byArtifactNameThenId(a: ArtifactRow, b: ArtifactRow): number {
  return artifactName(a).localeCompare(artifactName(b)) || a.id.localeCompare(b.id);
}

/**
 * The Bin's rail picture, from the two populations the app already holds.
 *
 * Both arguments are ALREADY the archived sets -- this fold does not decide
 * what is archived, because the Bin app decides that from its own two reads
 * and the two surfaces have to agree about what is in the Bin. What is left
 * here is arrangement and counting.
 */
export function foldBinRail(
  archivedFiles: readonly ArtifactRow[],
  archivedFolders: readonly FolderRow[],
): BinRail {
  const folders = [...archivedFolders].sort(byNameThenId);
  const inBin = new Set(folders.map((f) => f.id));
  const filesByFolder = new Map<string, ArtifactRow[]>();
  const loose: ArtifactRow[] = [];
  for (const file of archivedFiles) {
    if (file.folderId !== "" && inBin.has(file.folderId)) {
      const list = filesByFolder.get(file.folderId) ?? [];
      list.push(file);
      filesByFolder.set(file.folderId, list);
    } else {
      loose.push(file);
    }
  }
  for (const list of filesByFolder.values()) list.sort(byArtifactNameThenId);
  loose.sort(byArtifactNameThenId);
  return {
    folders: folders.map((folder) => ({ folder, files: filesByFolder.get(folder.id) ?? [] })),
    loose,
    fileCount: archivedFiles.length,
    folderCount: folders.length,
  };
}
