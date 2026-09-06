import type { FolderTree } from "../fold";
import { subtreeFolderIds } from "../fold";
import type { ArtifactRow } from "../rows";

// The folder-archive walk (design B5, D11): client-driven, children first,
// idempotent under interruption.
//
// The client holds the owner's whole (small) tree and the engine has no loop
// construct, so the walk runs here: `archiveArtifact` per contained artifact
// (the existing artifact->file automation archives backing bytes), then
// `archiveLibraryFolder` leaves inward. A re-run recomputes the plan from
// LIVE rows, so whatever already landed is simply absent from the next plan
// -- that is the idempotency, and it is why the plan carries no state.
//
// ===========================================================================
// TWO DISPOSITIONS, AND WHICH ONE A FOLDER TAKES IS NOT A PREFERENCE
// ===========================================================================
// Archiving is how somebody gets a thing back later. A folder with no file
// anywhere beneath it has nothing to get back, so archiving one leaves a row
// in the Files rail's Bin and in the Bin app that answers no question anybody
// asked,
// sitting next to the files that genuinely are waiting there. Those folders
// are DELETED instead (`deleteLibraryFolder` -- still a soft delete, but one
// every folder read excludes), and the difference is one a person can see: an
// archived folder is somewhere they can go and look, a deleted one is gone
// from every surface at once.
//
// The predicate is over ALL artifacts, ARCHIVED INCLUDED. An archived file
// beneath a folder is still a file somebody can restore, so the folder above
// it is not empty. That is also what keeps a resumed walk consistent, because
// the artifact phase runs first and leaves exactly that shape behind.
//
// The root folder being acted on is subject to the same rule as every folder
// under it. Archiving an entirely empty tree therefore deletes all of it and
// archives nothing, which is the whole point.

/** Every folder in `folderIds` whose subtree holds at least one artifact.
 *
 *  One pass, because `folderIds` arrives children-first: by the time a parent
 *  is reached each of its children has already been decided, so "holds one
 *  directly, or has a child that holds one" is the complete answer. */
function foldersHoldingArtifacts(
  tree: FolderTree,
  rows: readonly ArtifactRow[],
  folderIds: readonly string[],
): Set<string> {
  // Archived rows count. This is the line that decides it, so it is the line
  // to read before anybody changes the rule.
  const holdsDirectly = new Set(rows.map((r) => r.folderId));
  const holds = new Set<string>();
  for (const id of folderIds) {
    const children = tree.byId.get(id)?.children ?? [];
    if (holdsDirectly.has(id) || children.some((c) => holds.has(c.folder.id))) holds.add(id);
  }
  return holds;
}

/**
 * Does this folder's subtree hold any artifact at all, at any depth?
 *
 * Exported so a confirm can say the honest sentence rather than offering to
 * archive something that is about to be deleted. Pure, and it answers from
 * the same function the plan partitions on -- one definition of "empty", not
 * two that drift apart the first time either is edited.
 */
export function subtreeHoldsArtifact(
  tree: FolderTree,
  rows: readonly ArtifactRow[],
  folderId: string,
): boolean {
  const ids = subtreeFolderIds(tree, folderId);
  const inScope = new Set(ids);
  return foldersHoldingArtifacts(
    tree,
    rows.filter((r) => inScope.has(r.folderId)),
    ids,
  ).has(folderId);
}

export interface ArchivePlan {
  /** What the confirm names: live, un-archived items inside the subtree. */
  itemCount: number;
  /** Deepest folders' contents first, so an interruption never leaves a
   *  child visible under an archived parent longer than its own batch. */
  artifactIds: string[];
  /** Every folder in the subtree, children before parents, the folder itself
   *  last -- the WALK ORDER, with both dispositions interleaved. Children
   *  first has to hold ACROSS the two: a folder that archives can sit above
   *  one that is deleted and the other way round, and running the two as
   *  separate phases would leave a live child under a parent already gone. */
  folderIds: string[];
  /** The subset of `folderIds` whose subtree holds at least one artifact. */
  archiveFolderIds: string[];
  /** The subset whose subtree holds none, at any depth. AUTHORITATIVE: the
   *  walk reads this set and treats every other id in `folderIds` as an
   *  archive, so the two halves can never disagree about one folder's fate.
   *  `archiveFolderIds` is its complement, carried so a caller can name both
   *  halves in a sentence without recomputing either. */
  deleteFolderIds: string[];
}

export function planArchive(
  tree: FolderTree,
  rows: readonly ArtifactRow[],
  folderId: string,
): ArchivePlan {
  const folderIds = subtreeFolderIds(tree, folderId);
  const inScope = new Set(folderIds);
  const order = new Map(folderIds.map((id, i) => [id, i]));
  const live = rows.filter((r) => !r.archived && inScope.has(r.folderId));
  live.sort(
    (a, b) => (order.get(a.folderId) ?? 0) - (order.get(b.folderId) ?? 0) || a.id.localeCompare(b.id),
  );
  // Scoped to the subtree deliberately: an artifact filed somewhere else says
  // nothing about whether THIS branch is empty.
  const holds = foldersHoldingArtifacts(
    tree,
    rows.filter((r) => inScope.has(r.folderId)),
    folderIds,
  );
  return {
    itemCount: live.length,
    artifactIds: live.map((r) => r.id),
    folderIds,
    archiveFolderIds: folderIds.filter((id) => holds.has(id)),
    deleteFolderIds: folderIds.filter((id) => !holds.has(id)),
  };
}

/** Past this many contained items, progress renders between batches. */
export const ARCHIVE_BATCH_SIZE = 25;

export interface ArchiveWalkPorts {
  archiveArtifact: (artifactId: string) => Promise<void>;
  archiveFolder: (folderId: string) => Promise<void>;
  /** REQUIRED, not optional. An omitted port could only fall back to
   *  archiving, which is the exact outcome the delete disposition exists to
   *  prevent -- and it would fail quietly, as a Bin filling up with empty
   *  folders nobody chose to keep. */
  deleteFolder: (folderId: string) => Promise<void>;
  /** (done, total) across artifacts + folders, called as the walk advances. */
  onProgress: (done: number, total: number) => void;
}

/**
 * Run the plan, sequentially, stopping at the first refusal. The caller
 * renders the refusal in surface; a re-run plans again from live rows and
 * archives only the remainder.
 *
 * Both folder dispositions ride the ONE ordered pass over `plan.folderIds`,
 * which is what keeps children-first true across the split.
 *
 * ONE WRINKLE A RESUMED WALK CAN SHOW, written down rather than left to be
 * discovered: the re-run plans from the LIVE tree, so a folder whose only
 * artifact-bearing child the interrupted run already archived reads as empty
 * the second time and takes the delete disposition. Nothing is lost -- that
 * child and its files are in the Bin, where the fold renders a row whose
 * parent did not arrive at root, exactly as it already does for every other
 * orphan. The alternative is remembering the shape the tree had before the
 * run, which is precisely the state this plan refuses to carry.
 */
export async function runArchiveWalk(plan: ArchivePlan, ports: ArchiveWalkPorts): Promise<void> {
  const total = plan.artifactIds.length + plan.folderIds.length;
  const toDelete = new Set(plan.deleteFolderIds);
  let done = 0;
  for (const artifactId of plan.artifactIds) {
    await ports.archiveArtifact(artifactId);
    done += 1;
    ports.onProgress(done, total);
  }
  for (const folderId of plan.folderIds) {
    if (toDelete.has(folderId)) await ports.deleteFolder(folderId);
    else await ports.archiveFolder(folderId);
    done += 1;
    ports.onProgress(done, total);
  }
}
