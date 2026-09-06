import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import type { MachinePresence, ProvenanceTone } from "../../items/provenance";
import { boolOr, flatten, stringsOf } from "../../kit/rows";
import { CONTENT_KINDS } from "./concepts";

// The wire rows the Files app renders, projected into the shapes its surfaces
// read. Pure and separate from every component, for the reason the sibling
// apps' rows.ts are: everything here is a function of a row, unit-testable
// with no browser, no cluster and no React -- which is what lets the list, the
// tree, the inspector and the desk popover be checked against the same
// fixtures and therefore against each other.

export interface ArtifactRow {
  id: string;
  ownerUserId: string;
  lens: string;
  /** file | document | generated_output here; the records kinds never render. */
  kind: string;
  /** The concept's provenance source (uploaded / computer_use / ...). */
  source: string;
  sourceConceptRef: string;
  title: string;
  summary: string;
  format: string;
  mimeType: string;
  labels: string[];
  /**
   * The clients this item is about (epic memql#4800, D5) -- a LIST, because
   * "one or two accounts" is the owner's own framing. Absent on every row
   * promoted before the field existed, which the fold reads as untied: the
   * `folderId` lesson applied to a list.
   */
  accountIds: string[];
  /** "" = root. Absent on every pre-folders row, which is the same answer. */
  folderId: string;
  archived: boolean;
  producedByPlanId: string;
  producedByWorkerId: string;
  producedByWorkerName: string;
  /** Documents carry the training pipeline's verdict; others answer "". */
  validationStatus: string;
  createdAt: string;
}

export function artifactFromRow(raw: Row): ArtifactRow {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    ownerUserId: rowString(row, "ownerUserId"),
    lens: rowString(row, "lens"),
    kind: rowString(row, "kind"),
    source: rowString(row, "source"),
    sourceConceptRef: rowString(row, "sourceConceptRef"),
    title: rowString(row, "title"),
    summary: rowString(row, "summary"),
    format: rowString(row, "format"),
    mimeType: rowString(row, "mimeType"),
    labels: stringsOf(row, "labels"),
    accountIds: stringsOf(row, "accountIds"),
    folderId: rowString(row, "folderId"),
    // ABSENT IS NOT ARCHIVED: every artifact promoted before memql#4340 has no
    // `archived` member at all, and a fold that read absence as true would
    // empty the list on the first event that did not touch the field.
    archived: boolOr(row, "archived", false),
    producedByPlanId: rowString(row, "producedByPlanId"),
    producedByWorkerId: rowString(row, "producedByWorkerId"),
    producedByWorkerName: rowString(row, "producedByWorkerName"),
    validationStatus: rowString(row, "validationStatus"),
    createdAt: rowString(row, "createdAt"),
  };
}

export interface FolderRow {
  id: string;
  name: string;
  /** "" = a root folder. */
  parentFolderId: string;
  archived: boolean;
  /**
   * The other disposition: a folder whose subtree held no file at all, which
   * the archive walk deletes rather than archives (there is nothing inside it
   * for anybody to get back). Distinct from `archived` -- nothing archived it,
   * and no restore brings it back.
   *
   * PROJECTED HERE BECAUSE THE LIVE PATH NEEDS IT. Every folder read carries
   * `isNotDeleted`, so a seed never delivers one -- but a live re-read is the
   * raw `concept == … && id == …` fetch, which honours row authz and no query
   * filter at all. Without this field the delete would land, the row would
   * come back on its own update, and the folder would sit in the tree until
   * somebody reloaded: the deletion looking exactly like a no-op.
   */
  deleted: boolean;
}

export function folderFromRow(raw: Row): FolderRow {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    name: rowString(row, "name"),
    parentFolderId: rowString(row, "parentFolderId"),
    archived: boolOr(row, "archived", false),
    // ABSENT IS NOT DELETED, for the reason the artifact fold records one
    // field up: every folder written before this field existed has no member
    // at all, and reading absence as true would empty the tree on the first
    // event that did not touch it.
    deleted: boolOr(row, "deleted", false),
  };
}

/**
 * A composition, projected to the little this app needs of it (epic
 * memql#4981, #4983).
 *
 * FILES READS THE RECORD AND NEVER WRITES IT. The Materializer app owns
 * `v1:compose:composition`; this projection exists so the Files rail can
 * answer "which of my files were made in the Materializer" and the inspector
 * can offer one handoff. Everything the record is ABOUT -- the sources, the
 * template, the models that contributed, the provenance -- is deliberately
 * absent, because restating it here would be a second reading of one row that
 * is free to disagree with the app whose subject it is.
 */
export interface CompositionRow {
  id: string;
  name: string;
  /** The v1:library:file this composition produced. "" while it has none --
   *  a draft, or a run that failed before storing anything. */
  outputFileId: string;
  format: string;
  status: string;
  archived: boolean;
}

export function compositionFromRow(raw: Row): CompositionRow {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    name: rowString(row, "name"),
    outputFileId: rowString(row, "outputFileId"),
    format: rowString(row, "format"),
    status: rowString(row, "status"),
    // ABSENT IS NOT ARCHIVED, the rule this file already applies twice.
    archived: boolOr(row, "archived", false),
  };
}

/** What to call a file. NEVER blank: a nameless row is indistinguishable from
 *  a row that failed to render. */
export function artifactName(row: ArtifactRow): string {
  const title = row.title.trim();
  return title !== "" ? title : row.id;
}

/** The records lens stays out of this app (design D2). */
export function isContentKind(kind: string): boolean {
  return (CONTENT_KINDS as readonly string[]).includes(kind);
}

/**
 * What counts as a CHANGE to a file -- the arrival cue's contract.
 *
 * Fingerprints what a person would call a change: a rename, a move, an
 * archive, a label edit, an analysis result landing (summary / format), a
 * validation flip. DELIBERATELY NOT `updatedAt`: the analysis pass re-stamps
 * it on every touch, and naming it would strobe the whole list on churn no
 * person can see.
 */
// The two separators, written as ESCAPES rather than as the raw bytes they
// used to be. Semantically identical, and the difference is entirely about
// tooling: a raw NUL makes grep classify this file as binary and skip it
// SILENTLY, so every search for anything defined here -- FolderRow,
// folderFromRow, fileStory -- answered "referenced nowhere". The sibling
// uploadTree.ts already writes its own separator as `\u001F` for this reason;
// this brings the two into line.
//
// They stay control characters because that is what makes them safe: a
// separator has to be a byte no title, label or folder id can contain, or two
// different rows could fingerprint the same.
const LABEL_SEP = "\u0001";
const FIELD_SEP = "\u0000";

export function artifactFingerprint(row: ArtifactRow): string {
  return [
    row.title,
    row.folderId,
    row.archived ? "archived" : "",
    row.labels.join(LABEL_SEP),
    row.validationStatus,
    row.summary,
    row.format,
  ].join(FIELD_SEP);
}

/**
 * The file's provenance story (design D1): one sentence and the dot to say it
 * with. THE DOT NEVER GUESSES -- a machine-linked file with no fleet facts yet
 * renders "unknown", which the kit draws as no dot at all.
 */
export interface FileStory {
  sentence: string;
  tone: ProvenanceTone;
  /** True when the index names a producing machine -- the dot's own gate. */
  machineNamed: boolean;
}

const SOURCE_SENTENCES: Record<string, string> = {
  uploaded: "Uploaded here",
  exported: "Exported from an artifact",
  workbench_generated: "Made on the workbench",
  agent_generated: "Made by an agent",
  derived: "Derived from an artifact",
  user_created: "Written here",
  live: "Live source",
};

export function fileStory(row: ArtifactRow, machine: MachinePresence | null): FileStory {
  if (row.producedByWorkerId !== "") {
    // A named machine outranks every other reading: it is the physical fact,
    // and its presence is what the dot is about (design D5 -- producedByWorker*
    // means "a machine is known", computer-use or upload alike).
    const name = row.producedByWorkerName.trim() || machine?.name || "one of your machines";
    const sentence =
      row.source === "computer_use" ? `Made on ${name} by computer use` : `Uploaded from ${name}`;
    const tone: ProvenanceTone =
      machine === null ? "unknown" : machine.online ? "reachable" : "unreachable";
    return { sentence, tone, machineNamed: true };
  }
  if (row.source === "computer_use") {
    // Made by computer use, machine unrecorded: honest and dot-less.
    return { sentence: "Made by computer use", tone: "unknown", machineNamed: false };
  }
  if (row.producedByPlanId !== "") {
    // THE ID IS NOT IN THE SENTENCE, and that is the whole of the fix.
    //
    // This read `Produced by plan ${id}`, which put a 32-character opaque
    // token in the one line the inspector leads with -- three wrapped lines of
    // hex above the file's own summary, saying nothing a person can act on,
    // and repeating the `Plan` fact four rows below it (DESIGN.md rule 7).
    // The fact is where an id belongs: it is monospaced, truncated, and has a
    // button that copies the whole thing. The story is for the sentence only
    // this platform can say.
    return { sentence: "Produced by a plan", tone: "reachable", machineNamed: false };
  }
  const sentence = SOURCE_SENTENCES[row.source];
  if (sentence !== undefined) return { sentence, tone: "reachable", machineNamed: false };
  return { sentence: "In the Library", tone: "unknown", machineNamed: false };
}
