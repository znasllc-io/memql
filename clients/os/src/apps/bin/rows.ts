import type { ArtifactRow, FolderRow } from "../files/rows";
import { artifactName } from "../files/rows";

// What the Bin renders, projected from the two feeds it retains.
//
// Pure and separate from every component, for the reason the sibling apps'
// rows.ts are: everything here is a function of a row, so the list, the detail
// panel and the restore plan can be checked against one set of fixtures and
// therefore against each other.

export type BinItemKind = "artifact" | "folder";

export interface BinItem {
  id: string;
  kind: BinItemKind;
  name: string;
  /** The artifact's own kind (file / document / generated_output), or "" for
   *  a folder. */
  contentKind: string;
  /** The artifact's provenance source, or "" for a folder. */
  source: string;
  /** v1:library:file id behind a file artifact, or "" -- what the detail
   *  panel reads for size, path and link state, and what the restore pair
   *  needs for its second half. */
  fileId: string;
  producedByWorkerId: string;
  producedByWorkerName: string;
  producedByPlanId: string;
  labels: string[];
  /** The folder this was filed in when it was archived. "" = the Library
   *  root, which is also what a row promoted before folders existed says. */
  folderId: string;
  /**
   * The moment the row was last written.
   *
   * MemQL is append-only, so a row's createdAt is the moment of its LAST
   * write -- which for something in the Bin is the archive itself, unless
   * somebody edited it afterwards. That is why the list orders by it and why
   * it is labelled "Last changed" rather than "Archived": the ordering is
   * right and the stronger claim is not one this field can make.
   */
  changedAt: string;
}

export function binItemFromArtifact(row: ArtifactRow): BinItem {
  return {
    id: row.id,
    kind: "artifact",
    name: artifactName(row),
    contentKind: row.kind,
    source: row.source,
    // The backing file id, taken from the artifact's own source ref. Blank for
    // every non-file kind, which reads nothing: a note has no bytes and no
    // machine, and offering an empty machine block for one answers a question
    // nobody asked.
    fileId: row.kind === "file" ? (row.sourceConceptRef.split(":").pop() ?? "") : "",
    producedByWorkerId: row.producedByWorkerId,
    producedByWorkerName: row.producedByWorkerName,
    producedByPlanId: row.producedByPlanId,
    labels: row.labels,
    folderId: row.folderId,
    changedAt: row.createdAt,
  };
}

export function binItemFromFolder(row: FolderRow): BinItem {
  return {
    id: row.id,
    kind: "folder",
    name: row.name.trim() !== "" ? row.name : row.id,
    contentKind: "",
    source: "",
    fileId: "",
    producedByWorkerId: "",
    producedByWorkerName: "",
    producedByPlanId: "",
    labels: [],
    folderId: row.parentFolderId,
    changedAt: "",
  };
}

/**
 * What counts as a CHANGE to something in the Bin -- the arrival cue's
 * contract.
 *
 * A rename, a re-filing, a label edit: things a person did. DELIBERATELY NOT
 * `changedAt`, and the reason is sharper here than anywhere else in the shell.
 * Every archive re-versions its row, so `createdAt` moves for every item that
 * arrives -- naming it would make each arrival announce itself twice, once as
 * a new row and again as an update, and every later touch a person cannot see
 * would ring a bell in a window nobody is looking at.
 */
export function binFingerprint(item: BinItem): string {
  return [item.name, item.folderId, item.labels.join(""), item.contentKind].join(" ");
}

/**
 * The Bin's order: most recently changed first, which for an archived row is
 * most recently archived first.
 *
 * Sorted CLIENT-SIDE over both feeds together, because folders and artifacts
 * arrive on separate reads and a Bin that listed all the folders and then all
 * the files would put the thing somebody just threw away in the middle. The
 * tie-break is the id, so the order is total and never reshuffles under
 * somebody watching it -- a folder carries no timestamp at all and every one
 * of them would otherwise be free to swap places on any update.
 */
export function orderBinItems(items: readonly BinItem[]): BinItem[] {
  return [...items].sort((a, b) => {
    if (a.changedAt !== b.changedAt) return a.changedAt < b.changedAt ? 1 : -1;
    return a.id.localeCompare(b.id);
  });
}

/** Narrow the Bin to what a search box asked for. Matches the name, the
 *  labels and the machine, because "which laptop was that from" is a question
 *  somebody standing in a Bin actually asks. */
export function filterBinItems(items: readonly BinItem[], search: string): BinItem[] {
  const needle = search.trim().toLowerCase();
  if (needle === "") return [...items];
  return items.filter((item) =>
    [item.name, item.producedByWorkerName, ...item.labels]
      .join(" ")
      .toLowerCase()
      .includes(needle),
  );
}
