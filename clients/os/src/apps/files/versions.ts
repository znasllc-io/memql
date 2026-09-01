import { rowNumber, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import type { MachinePresence, ProvenanceTone } from "../../items/provenance";
import { flatten } from "../../kit/rows";

// A file's version history, projected (epic memql#4806).
//
// Pure and separate from the panel, for the reason rows.ts is: everything here
// is a function of rows, unit-testable with no browser, no cluster and no
// React -- which is what lets the fold be checked against the same fixtures
// the rendering is.
//
// THE HEAD IS NOT IN THE HISTORY READ, and that is the shape of the whole
// feature rather than an accident of the query. The newest version IS the
// v1:library:file row, so the artifact index, the content route and the
// analysis pass all go on reading exactly one row; the version rows are the
// superseded ones. This module is where the two are folded into the single
// stack a person actually reads.

/** The head: the v1:library:file row, holding the newest bytes. */
export interface FileHead {
  id: string;
  name: string;
  mimeType: string;
  size: number;
  /** "" when nothing has measured it yet -- never "no hash exists". */
  sha256: string;
  format: string;
  status: string;
  summary: string;
  /** ABSENT IS 1. Every file uploaded before versions existed has no member. */
  versionNumber: number;
  /** When THESE bytes arrived -- deliberately not the row's createdAt. */
  versionUploadedAt: string;
  uploadedFromWorkerId: string;
  uploadedFromWorkerName: string;
  uploadedFromPath: string;
}

/** One superseded version. */
export interface FileVersion {
  id: string;
  fileId: string;
  versionNumber: number;
  name: string;
  mimeType: string;
  size: number;
  sha256: string;
  format: string;
  summary: string;
  uploadedFromWorkerId: string;
  uploadedFromWorkerName: string;
  uploadedFromPath: string;
  uploadedAt: string;
  supersededAt: string;
}

/**
 * The head's version number, with the absent case named.
 *
 * ABSENT IS 1, never 0. Every file uploaded before this field existed carries
 * no member at all, and a reader that turned that into 0 would label most of
 * the Library "v0". The server holds the same rule at its own point of use;
 * both sides say why, because a fold that quietly disagreed with the writer
 * would renumber files nobody touched.
 */
export function headVersionNumber(raw: number): number {
  return Number.isFinite(raw) && raw >= 1 ? Math.floor(raw) : 1;
}

export function fileHeadFromRow(raw: Row): FileHead {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    name: rowString(row, "name"),
    mimeType: rowString(row, "mimeType"),
    size: rowNumber(row, "size"),
    sha256: rowString(row, "sha256"),
    format: rowString(row, "format"),
    status: rowString(row, "status"),
    summary: rowString(row, "summary"),
    versionNumber: headVersionNumber(rowNumber(row, "versionNumber")),
    versionUploadedAt: rowString(row, "versionUploadedAt"),
    uploadedFromWorkerId: rowString(row, "uploadedFromWorkerId"),
    uploadedFromWorkerName: rowString(row, "uploadedFromWorkerName"),
    uploadedFromPath: rowString(row, "uploadedFromPath"),
  };
}

export function fileVersionFromRow(raw: Row): FileVersion {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    fileId: rowString(row, "fileId"),
    versionNumber: rowNumber(row, "versionNumber"),
    name: rowString(row, "name"),
    mimeType: rowString(row, "mimeType"),
    size: rowNumber(row, "size"),
    sha256: rowString(row, "sha256"),
    format: rowString(row, "format"),
    summary: rowString(row, "summary"),
    uploadedFromWorkerId: rowString(row, "uploadedFromWorkerId"),
    uploadedFromWorkerName: rowString(row, "uploadedFromWorkerName"),
    uploadedFromPath: rowString(row, "uploadedFromPath"),
    uploadedAt: rowString(row, "uploadedAt"),
    supersededAt: rowString(row, "supersededAt"),
  };
}

/**
 * One version's provenance, in the language the inspector's header already
 * speaks. Deliberately narrower than `fileStory`: a version row records how
 * THESE bytes arrived and nothing else, so there is no plan reading and no
 * computer-use reading to give. Inventing one would be inventing provenance.
 *
 * THE DOT NEVER GUESSES, exactly as the header's does: a version that names a
 * machine the fleet has nothing to say about renders "unknown", which the kit
 * draws as no dot at all.
 */
export interface VersionStory {
  sentence: string;
  tone: ProvenanceTone;
}

export function versionStory(
  facts: { uploadedFromWorkerId: string; uploadedFromWorkerName: string },
  machine: MachinePresence | null,
): VersionStory {
  if (facts.uploadedFromWorkerId.trim() === "") {
    // A browser cannot name a machine and sends nothing. That is a fact about
    // the upload, not a gap in the record.
    return { sentence: "Uploaded here", tone: "reachable" };
  }
  const name = facts.uploadedFromWorkerName.trim() || machine?.name || "one of your machines";
  return {
    sentence: `Uploaded from ${name}`,
    tone: machine === null ? "unknown" : machine.online ? "reachable" : "unreachable",
  };
}

/** One row of the rendered stack: a version, ready to draw. */
export interface VersionEntry {
  key: string;
  versionNumber: number;
  /** True for exactly one entry: the head, the version the file holds now. */
  current: boolean;
  name: string;
  size: number;
  /** "" means not measured; the panel renders a dash, never an error. */
  sha256: string;
  summary: string;
  /** When these bytes arrived. */
  uploadedAt: string;
  /** When this version stopped being current; "" for the head. */
  supersededAt: string;
  uploadedFromWorkerId: string;
  uploadedFromWorkerName: string;
}

export interface VersionHistory {
  entries: VersionEntry[];
  /** How many versions this file HAS, according to the head. */
  total: number;
  /** How many this fold could show. */
  shown: number;
  /** True when the history read could not reach back to version 1. */
  truncated: boolean;
}

/**
 * Fold the head and the superseded rows into one newest-first stack.
 *
 * Three decisions worth stating:
 *
 *   - THE HEAD WINS ON A COLLISION. A supersede is two writes with no
 *     transaction across them, and the order is chosen so a crash between them
 *     duplicates a version rather than losing one -- so a version row whose
 *     number equals the head's is that interrupted write, and the head is the
 *     truth. Rendering both would show one upload twice.
 *   - TOTAL COMES FROM THE HEAD, NOT FROM A COUNT. The head says which version
 *     it is, so a history read that returned fewer rows than that is a read
 *     that did not reach back -- which the panel says out loud rather than
 *     showing a prefix as if it were everything.
 *   - NO HEAD MEANS NO STACK. A file row that could not be read is not a file
 *     with no history; returning an empty history for it would be an answer we
 *     do not have.
 */
export function foldVersions(head: FileHead | null, versions: FileVersion[]): VersionHistory {
  if (head === null) return { entries: [], total: 0, shown: 0, truncated: false };

  const byNumber = new Map<number, VersionEntry>();
  for (const v of versions) {
    if (!Number.isFinite(v.versionNumber) || v.versionNumber < 1) continue;
    if (v.versionNumber >= head.versionNumber) continue;
    byNumber.set(v.versionNumber, {
      key: v.id || `v${v.versionNumber}`,
      versionNumber: v.versionNumber,
      current: false,
      name: v.name,
      size: v.size,
      sha256: v.sha256,
      summary: v.summary,
      uploadedAt: v.uploadedAt,
      supersededAt: v.supersededAt,
      uploadedFromWorkerId: v.uploadedFromWorkerId,
      uploadedFromWorkerName: v.uploadedFromWorkerName,
    });
  }
  byNumber.set(head.versionNumber, {
    key: head.id || `v${head.versionNumber}`,
    versionNumber: head.versionNumber,
    current: true,
    name: head.name,
    size: head.size,
    sha256: head.sha256,
    summary: head.summary,
    uploadedAt: head.versionUploadedAt,
    supersededAt: "",
    uploadedFromWorkerId: head.uploadedFromWorkerId,
    uploadedFromWorkerName: head.uploadedFromWorkerName,
  });

  const entries = [...byNumber.values()].sort((a, b) => b.versionNumber - a.versionNumber);
  return {
    entries,
    total: head.versionNumber,
    shown: entries.length,
    truncated: entries.length < head.versionNumber,
  };
}

/**
 * A hash a person can compare at a glance: the first and last six characters.
 *
 * "--" for absent, because absent means NOT MEASURED and a dash is what this
 * shell renders for a value it does not have. The full digest goes on the
 * element's title, so nothing is hidden -- only shortened.
 */
export function shortDigest(sha256: string): string {
  const digest = sha256.trim().toLowerCase();
  if (digest === "") return "--";
  if (digest.length <= 16) return digest;
  return `${digest.slice(0, 6)}...${digest.slice(-6)}`;
}
