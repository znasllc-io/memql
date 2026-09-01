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

export interface ArchivePlan {
  /** What the confirm names: live, un-archived items inside the subtree. */
  itemCount: number;
  /** Deepest folders' contents first, so an interruption never leaves a
   *  child visible under an archived parent longer than its own batch. */
  artifactIds: string[];
  /** Children before parents, the folder itself last. */
  folderIds: string[];
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
  return {
    itemCount: live.length,
    artifactIds: live.map((r) => r.id),
    folderIds,
  };
}

/** Past this many contained items, progress renders between batches. */
export const ARCHIVE_BATCH_SIZE = 25;

export interface ArchiveWalkPorts {
  archiveArtifact: (artifactId: string) => Promise<void>;
  archiveFolder: (folderId: string) => Promise<void>;
  /** (done, total) across artifacts + folders, called as the walk advances. */
  onProgress: (done: number, total: number) => void;
}

/**
 * Run the plan, sequentially, stopping at the first refusal. The caller
 * renders the refusal in surface; a re-run plans again from live rows and
 * archives only the remainder.
 */
export async function runArchiveWalk(plan: ArchivePlan, ports: ArchiveWalkPorts): Promise<void> {
  const total = plan.artifactIds.length + plan.folderIds.length;
  let done = 0;
  for (const artifactId of plan.artifactIds) {
    await ports.archiveArtifact(artifactId);
    done += 1;
    ports.onProgress(done, total);
  }
  for (const folderId of plan.folderIds) {
    await ports.archiveFolder(folderId);
    done += 1;
    ports.onProgress(done, total);
  }
}
