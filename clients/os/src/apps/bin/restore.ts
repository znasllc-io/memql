import type { BinItem } from "./rows";

// Taking something back out of the Bin (memql#4784).
//
// ===========================================================================
// A RESTORE IS A CLIENT-DRIVEN PAIR, FOR THE REASON THE ARCHIVE WALK IS ONE
// ===========================================================================
// Archiving an artifact archives its backing file through an automation
// (`archiveFileOnArtifactArchive`), and the mirror of that automation cannot
// exist: it would ride `node.updated` filtered on `archived == false`, which
// is essentially EVERY artifact update, and together with the archive
// automation already in place the two close a cycle -- each write publishes an
// event the other subscribes to, and both being idempotent does not stop the
// events. The engine says so in that automation's own header.
//
// So the client runs both writes, exactly as it runs the recursive archive
// walk. A plan is computed from LIVE rows and carries no state, which is what
// makes an interrupted restore idempotent: re-running plans again, and
// whatever already landed is simply absent from the next plan.

export interface RestorePlan {
  /** The index row to un-archive. Always present. */
  artifactId: string;
  /** The backing v1:library:file to un-archive with it, or "" for a kind that
   *  has none. The two rows must not disagree about whether the thing is in
   *  the Bin. */
  fileId: string;
  /** The folder to un-archive, or "" -- set only for a folder item. */
  folderId: string;
}

export function planRestore(item: BinItem): RestorePlan {
  if (item.kind === "folder") {
    return { artifactId: "", fileId: "", folderId: item.id };
  }
  return { artifactId: item.id, fileId: item.fileId, folderId: "" };
}

export interface RestorePorts {
  restoreArtifact: (artifactId: string) => Promise<void>;
  restoreFile: (fileId: string) => Promise<void>;
  restoreFolder: (folderId: string) => Promise<void>;
}

/**
 * Run the plan, stopping at the first refusal so the caller can render it.
 *
 * THE INDEX GOES FIRST. The Library list reads the index, so restoring it
 * first is what makes the item visible again at the earliest possible moment;
 * a refusal on the file afterwards leaves a visible row whose bytes are still
 * flagged, which is a state somebody can see and re-run. The other order
 * leaves an un-archived file behind an index row still in the Bin -- nothing
 * on any surface changes, and there is nothing to re-run from.
 */
export async function runRestore(plan: RestorePlan, ports: RestorePorts): Promise<void> {
  if (plan.folderId !== "") {
    await ports.restoreFolder(plan.folderId);
    return;
  }
  if (plan.artifactId !== "") await ports.restoreArtifact(plan.artifactId);
  if (plan.fileId !== "") await ports.restoreFile(plan.fileId);
}

/**
 * What restoring this will and will not do, as a sentence.
 *
 * A folder's is the one worth writing down: a folder comes back EMPTY, because
 * every item that was inside it is its own row and its own decision. Saying so
 * before the click is the difference between a feature and a surprise.
 */
export function restoreNote(item: BinItem): string {
  if (item.kind === "folder") {
    return "The folder comes back where it was. Anything that was inside it stays here until you restore it too -- each item is its own decision.";
  }
  return "It goes back to the folder it was filed in, with its history, its labels and everywhere it came from intact.";
}
