// The one way a client creates a file (design:
// docs/superpowers/specs/2026-10-11-materializer-front-door-design.md, section 2).
//
// Transport-neutral. The OS passes a function over query.executeNamed; the
// editors pass one over EditorConnectionAPI.execute(lease, ...). This module
// never imports a transport, so everything it does reaches the cluster through
// the `execute` it is handed, as the person who is signed in.
//
// ===========================================================================
// A REQUEST ID IS FRESH FOR EVERY PERSON ACTION
// ===========================================================================
// `requestId` is how a repeat of one request finds the composition the first
// one made, so a double click, or a retry after a dropped reply, makes ONE file.
// Mint a new id (crypto.randomUUID()) each time the person asks for a file, and
// reuse it only to resume that same ask.
//
// The other face of the same rule: a repeat with the same id after a FAILED
// first attempt returns that FAILED composition, by design. "Try again" after a
// failure is a new ask and needs a new id.
//
// The server looks the id up and then writes; it does not claim it. Two starts
// in flight at the same moment can still make two files, so a caller disables
// its control until startMaterialize settles.
//
// ===========================================================================
// A DESCRIBED MARKDOWN FILE NEEDS ONE MORE STEP, AND IT IS THE CLIENT'S
// ===========================================================================
// A Markdown file asked for by describing it (isDescribedMarkdown) is filed
// EMPTY by the server, with the description kept on the composition. Writing it
// is the Library's revision workflow, and the Library refuses a revision request
// made from inside a work run, so the person's own client hands the description
// over: handOffDescription posts it as a document-level comment (the
// library_file sources as attachments) and asks for the revision.
//
// materialize does that once the file has its Files entry. followComposition
// alone never does: the OS follows on its own live feed and calls
// handOffDescription itself.
//
// A DATA SOURCE KEEPS THE COMPOSER. The revision workflow reads Markdown and
// images; it cannot read the graph. So a Markdown request with a concept_row or
// query source is NOT described: the server keeps the composer for it (which
// reads the records and writes the file), isDescribedMarkdown says false, and
// materialize hands nothing on. Only Library files travel with a description.
//
// REFERENCES THE LIBRARY CANNOT ATTACH ARE REPORTED, NOT FATAL. The Library
// takes only Markdown (up to 32 KiB) and images as review attachments, and
// refuses a comment that carries anything else. So the hand-off attaches the
// library_file sources the Library accepts, opens the revision with them, and
// returns every other reference (a PDF, a file it cannot find, one past the
// Library's limits) by label in `unattached`, for the caller to name. The label
// is all an entry carries: it does not say why that reference was left off.
//
// ===========================================================================
// A REFUSAL REACHES A PERSON AS ONE SENTENCE
// ===========================================================================
// Both texts this module hands a caller to show -- the refusal startMaterialize
// throws, and a hand-off's revisionFailure -- are the cluster's sentence with
// the call's name, the engine's frame and any Go package name taken off
// (refusalSentence, beside executeNamed in query.ts). A person who typed a
// slash reads "Enter a name without slashes.", not three layers of plumbing.
//
// handOffDescription derives its two Library request ids from the request id
// (`<id>-comment`, `<id>-revision`), so a retry returns the first receipts. The
// Library accepts only letters, digits, "-" and "_" (8 to 120 characters) as a
// request id, so a request id used with a description is at most 111 of them;
// startMaterialize refuses a described request whose id is not, before the file
// is made, because the file would exist and its description could not be sent.
//
// A RETRY OF THE HAND-OFF MUST SEND THE SAME INPUTS. The Library answers a repeat
// of a request id with the first receipt only when the repeat says the same
// thing; a changed description or reference list under an old id is refused
// ("this feedback request was already used with different content"). Changed
// inputs are a new ask and need a new request id.
//
// A STOP NEVER DELETES A FILE THAT WAS ALREADY MADE: it stays in Files. From the
// moment the composition is ready the file exists, and a stop has nothing left to
// cancel. So materialize returns the file when it is already in Files at the
// moment of the stop: it skips the hand-off and, for a described request, says so
// in `revisionFailure` ("Stopped before the description was sent."), so the
// caller can open the empty file and offer the description again. That includes a
// stop that raced the composition finishing: the cluster cancels nothing that has
// finished and says so, and the composition is read again before the file is
// given up. The one stop that still throws after the file exists is one that
// lands while its Files entry is not there yet (see followComposition): without
// the entry there is no artifact to hand back, and the file is in Files a moment
// later.
import {
  buildComposeCancel,
  buildComposeMaterialize,
  buildLibraryAddDocumentComment,
  buildLibraryRequestDocumentRevision,
  type ComposeMaterializeArgs,
} from "./generated_builtins.js";
import {
  buildCompositionById,
  buildLibraryArtifactForFile,
  buildLibraryFileById,
  buildWorkRunForOwner,
} from "./generated_queries.js";
import { sameEntityId } from "./id.js";
import { refusalSentence } from "./query.js";
import { rowNumber, rowString, type Row } from "./types.js";

/** Runs one named construct as the signed-in person and returns its rows. `call` is the rendered call string. */
export type MaterializeExecute = (name: string, call: string) => Promise<Record<string, unknown>[]>;
export type MaterializeFormat = "markdown" | "html" | "txt" | "csv" | "json" | "docx" | "pdf";
export interface MaterializeSource { kind: "concept_row" | "library_file" | "query"; ref: string; label: string; content?: boolean; includeImages?: boolean }
export interface MaterializeRequest { name: string; format: MaterializeFormat; requestId: string; statement?: string; sources?: MaterializeSource[]; templateId?: string; folderId?: string; outputKind?: "email_template"; accountIds?: string[]; recipeId?: string; deployableKind?: string }
/**
 * What the helpers need from their caller. `signal` stops the follow and
 * cancels the composition; `onStatus` hears every poll's status; `intervalMs`
 * is the poll interval (default two seconds). The first look comes sooner, 300
 * ms after the start (or the interval, when that is shorter).
 */
export interface MaterializePorts { execute: MaterializeExecute; signal?: AbortSignal; onStatus?: (status: string) => void; intervalMs?: number }
/**
 * The file that was made. `revisionId` (the Library request that is writing the
 * document) or `revisionFailure` (why its description could not be sent) is set
 * only by materialize, and only for a described Markdown request. So is
 * `unattached`, the labels of the references that went without being attached;
 * it is absent when there are none.
 */
export interface MaterializeResult { compositionId: string; fileId: string; artifactId: string; name: string; revisionId?: string; revisionFailure?: string; unattached?: string[] }
/** The person (or something else) stopped the composition. Callers can ignore it. */
export class MaterializeCancelled extends Error {}
/** The composition failed, or its run stopped before finishing it. The message is for the person. */
export class MaterializeFailed extends Error {}

const DEFAULT_INTERVAL_MS = 2000;
// A blank file can be ready, with its Files entry, well before one interval has
// passed, and the upload New Markdown used before was instant. So the first look
// comes this long after the start; the rest keep the interval, so a composition
// that takes a while is not read more often than before.
const FIRST_LOOK_MS = 300;
const STOPPED = "Stopped";
const STOP_REASON = "Stopped by the person";
const RUN_STOPPED = "The run stopped before the file was finished.";
// A run in one of these states will never finish its composition on its own.
const DEAD_RUN = new Set(["failed", "cancelled", "abandoned"]);

// The file the Materializer has just made is the Library's first revision of it.
// The hand-off names that revision rather than reading the file's current one,
// so a retry after the first attempt succeeded (and the document has since
// moved on) still finds the original receipts instead of a mismatch.
const FIRST_VERSION = 1;
const FIRST_REVISION = "file:1";
const WHOLE_DOCUMENT = { kind: "document", quote: "Entire document" };
const HAND_OFF_ID = /^[A-Za-z0-9_-]{1,111}$/;
const HAND_OFF_FAILED = "The description could not be sent for writing: ";
const STOPPED_BEFORE_HAND_OFF = "Stopped before the description was sent.";
const HAND_OFF_ID_RULE = "A request id for a file that is asked for by describing it uses letters, digits, - and _ (at most 111 characters), because its description is sent under it.";

// What the Library takes as a review attachment (integrations/library/
// review_attachments.go). A comment carrying anything else is refused whole, so
// the hand-off applies the rules that need only a file's name and size. The
// Library still decides what only the bytes can show: Markdown that is UTF-8, a
// picture it can decode within 24 million pixels.
const MAX_ATTACHMENTS = 8; // reviewAttachmentCount
const MAX_ATTACHMENT_BYTES = 16 << 20; // reviewAttachmentBytes: all the attachments together
const MAX_MARKDOWN_BYTES = 32 << 10; // reviewMarkdownBytes: one Markdown file
const MAX_MARKDOWN_TOTAL_BYTES = 64 << 10; // all the Markdown together
const MAX_IMAGE_BYTES = 8 << 20; // one image
const IMAGE_EXTENSIONS = new Set([".png", ".jpeg", ".gif", ".webp"]); // ".jpg" is read as ".jpeg"
// Every refusal the Library words about an attachment says so ("attach a ...",
// "an attachment changed ...", "attachments exceed ..."). One about the document
// or the comment does not, and sending that again without attachments would only
// repeat it.
const ATTACHMENT_REFUSAL = /attach/i;
const UNNAMED = "Unnamed reference";

const first = (rows: Row[]): Row | null => rows[0] ?? null;
// The server trims these, so a value of only spaces is no value at all.
const has = (value: string | undefined): boolean => (value ?? "").trim() !== "";

/**
 * Whether the server will file this request EMPTY and keep its statement, for
 * the person's client to hand to the Library's revision workflow. Mirrors
 * `describedMarkdown()` in integrations/compose/materialize.go: Markdown, with
 * a statement, and nothing of its own to compose from -- no template, recipe or
 * special output, and no source that is not a Library file (the revision
 * workflow cannot read a query or a row, so a request with one keeps the
 * composer). A draft or section keys also rule it out; startMaterialize never
 * sends either. The two move together.
 */
export function isDescribedMarkdown(request: Pick<MaterializeRequest, "format" | "statement" | "templateId" | "recipeId" | "outputKind" | "deployableKind" | "sources">): boolean {
  return request.format === "markdown" && has(request.statement) &&
    !has(request.templateId) && !has(request.recipeId) && !has(request.outputKind) && !has(request.deployableKind) &&
    (request.sources ?? []).every(source => source.kind === "library_file");
}

// What a person may call a new file, in the words they read where they type it,
// each saying what is actually wrong. VS Code's New Markdown and Create Email from
// Examples and the MemQL OS composer check a name with fileNameProblem as it is
// typed, so they say the same thing about the same name. The cluster refuses the
// names it cannot file in its own words, for a client that sends one anyway.
const NO_NAME = "Enter a file name.";
const NOT_A_FILE_NAME = "Enter a file name, without a folder path.";
const NAME_TOO_LONG = "Use a name up to 150 characters.";
const NO_NAME_BEFORE_EXTENSION = "Enter a name before the extension.";
const MAX_NAME_LENGTH = 150;
// The suffixes that already name a format: its file extension and, for Markdown,
// its own name too (formatNameExtensions in integrations/compose/materialize.go).
const formatExtensions = (format: MaterializeFormat): string[] => (format === "markdown" ? ["md", "markdown"] : [format]);

/**
 * The name a file of this format is filed under: the name without ONE trailing
 * extension that already names the format, because the cluster adds the
 * extension once when it files the file ("Plan.md" is filed as Plan.md, never
 * Plan.md.md). An extension of another format is part of the name. Mirrors
 * cleanFileName in integrations/compose/materialize.go.
 */
export function fileNameStem(name: string, format: MaterializeFormat): string {
  const trimmed = name.trim();
  for (const extension of formatExtensions(format)) {
    const suffix = `.${extension}`;
    if (trimmed.length >= suffix.length && trimmed.slice(-suffix.length).toLowerCase() === suffix) return trimmed.slice(0, -suffix.length).trim();
  }
  return trimmed;
}

/**
 * What is wrong with a name typed for a new file of this format, or undefined
 * when it can be used, in that order: there is no name; it is a place (a slash,
 * a backslash or a control character -- folderId says where a file goes); it is
 * over 150 characters; or there is nothing before the extension, because ".md"
 * on its own would file a document with no name.
 */
export function fileNameProblem(name: string, format: MaterializeFormat): string | undefined {
  if (!name.trim()) return NO_NAME;
  if (/[\\/\u0000-\u001f]/.test(name)) return NOT_A_FILE_NAME;
  if (name.length > MAX_NAME_LENGTH) return NAME_TOO_LONG;
  if (!fileNameStem(name, format)) return NO_NAME_BEFORE_EXTENSION;
  return undefined;
}

/**
 * Opens the composition and returns its id. Sends only the fields
 * MaterializeRequest declares, and omits the ones left empty, so what the
 * server sees is what isDescribedMarkdown judged. A refusal is thrown as the
 * cluster's own sentence and nothing else (see the header).
 */
export async function startMaterialize(execute: MaterializeExecute, request: MaterializeRequest): Promise<string> {
  if (!has(request.requestId)) throw new Error("A request id is required, so that a repeated request makes one file.");
  // The server trims it, and the hand-off must ask under the id the file was made under.
  const requestId = request.requestId.trim();
  if (isDescribedMarkdown(request) && !HAND_OFF_ID.test(requestId)) throw new Error(HAND_OFF_ID_RULE);
  const args: ComposeMaterializeArgs = { name: request.name, format: request.format, requestId };
  if (has(request.statement)) args.statement = request.statement;
  if (request.sources?.length) args.sources = request.sources.map(source => ({ ...source }));
  if (has(request.templateId)) args.templateId = request.templateId;
  if (has(request.folderId)) args.folderId = request.folderId;
  if (has(request.outputKind)) args.outputKind = request.outputKind;
  if (request.accountIds?.length) args.accountIds = [...request.accountIds];
  if (has(request.recipeId)) args.recipeId = request.recipeId;
  if (has(request.deployableKind)) args.deployableKind = request.deployableKind;
  const reply = first(await inPlainWords(execute)("composeMaterialize", buildComposeMaterialize(args)));
  const compositionId = rowString(reply, "compositionId");
  if (!compositionId) throw new Error("Materializer did not confirm a composition. Check its history before retrying.");
  return compositionId;
}

// Waits one poll interval. The abort listener lives exactly as long as the
// wait: left in place it would add one per poll to a signal that outlives them.
function pause(ms: number, signal: AbortSignal | undefined): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    if (signal?.aborted) {
      reject(new MaterializeCancelled(STOPPED));
      return;
    }
    const onAbort = () => {
      clearTimeout(timer);
      reject(new MaterializeCancelled(STOPPED));
    };
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

// The words for a composition whose run can no longer finish it, or "" when
// the run is fine, cannot be told, or is not the run carrying this composition.
//
// A composition does not follow its run: one stays `composing` after the node
// making it is lost, and a follow that only reads the composition would wait
// for it forever. Only the run named by the composition can explain its stop.
// This is a safety net, so a run that cannot be read (denied, or a hiccup the
// next poll will meet again) leaves the composition's own status in charge.
async function deadRunMessage(execute: MaterializeExecute, runId: string): Promise<string> {
  let run: Row | null;
  try {
    run = first(await execute("workRunForOwner", buildWorkRunForOwner({ runId })));
  } catch {
    return "";
  }
  if (!run || !sameEntityId(rowString(run, "id"), runId) || !DEAD_RUN.has(rowString(run, "status"))) return "";
  return rowString(run, "errorMessage") || RUN_STOPPED;
}

/**
 * Polls the composition until it is ready and has its Files entry, reporting
 * each status through `onStatus`: the first look 300 ms after the start, the
 * rest one interval apart. Throws MaterializeFailed with the
 * composition's reason (or its run's, when the run stopped first) and
 * MaterializeCancelled when it was stopped.
 *
 * An aborted `signal` before the composition is ready cancels it once and stops
 * polling; if that cancel request itself fails, its error is thrown instead,
 * because the composition may still finish. Once it is ready its file exists and
 * there is nothing to cancel, so a stop then only ends the wait for the Files
 * entry: it takes one last look, returns the file when the entry is there, and
 * throws MaterializeCancelled when it is not (there is no artifact yet to hand
 * back; the composition stays ready and its file is kept).
 *
 * The look that saw the composition unfinished can be a whole interval old when
 * the stop arrives, so the composition may have finished in between. The cluster
 * cancels nothing that has finished and says so (`cancelled: false`); the
 * composition is then read again and a ready one is treated exactly like a stop
 * after the file exists, and one that failed reports why.
 *
 * Never hands a description to the Library: see the header, and materialize.
 */
export async function followComposition(ports: MaterializePorts, compositionId: string): Promise<MaterializeResult> {
  const { execute, signal } = ports;
  const interval = ports.intervalMs ?? DEFAULT_INTERVAL_MS;
  const read = async (): Promise<Row | null> => first(await execute("compositionById", buildCompositionById({ compositionId })));
  // The file, once the composition is ready. A new file's Files entry is written
  // after the file, by an automation, so it may not be there yet.
  let made: { fileId: string; name: string } | null = null;
  const filesEntry = async ({ fileId, name }: { fileId: string; name: string }): Promise<MaterializeResult | null> => {
    const artifact = first(await execute("libraryArtifactForFile", buildLibraryArtifactForFile({ fileId })));
    const artifactId = rowString(artifact, "id");
    return artifactId ? { compositionId, fileId, artifactId, name: rowString(artifact, "title") || name } : null;
  };
  // What the composition's row settles: its file once it is ready, nothing while
  // it is still being made, and an error when it failed, was stopped or cannot be read.
  const settled = (row: Row | null): { fileId: string; name: string } | null => {
    if (!row) throw new MaterializeFailed("This composition is no longer readable. Its history remains in Materializer.");
    const status = rowString(row, "status");
    if (status === "failed") throw new MaterializeFailed(rowString(row, "failureReason") || "Materializer could not finish this file.");
    if (status === "cancelled") throw new MaterializeCancelled(STOPPED);
    return status === "ready" ? { fileId: rowString(row, "outputFileId"), name: rowString(row, "name") } : null;
  };
  const stop = async (): Promise<MaterializeResult> => {
    if (made) {
      // Nothing to cancel, and the stop may have cut a wait short: look once more.
      const kept = await filesEntry(made);
      if (kept) return kept;
      throw new MaterializeCancelled(STOPPED);
    }
    const reply = first(await execute("composeCancel", buildComposeCancel({ compositionId, reason: STOP_REASON })));
    // A composition that had already finished is not cancelled, and the cluster says so. Its file may
    // have been made after the look that sent this stop, so read it again before giving it up.
    if (reply?.["cancelled"] === false) {
      const finished = settled(await read());
      if (finished) {
        made = finished;
        const kept = await filesEntry(made);
        if (kept) return kept;
      }
    }
    throw new MaterializeCancelled(STOPPED);
  };
  // Every look is made after a wait: the first a short one, the rest the
  // interval. A stop during any wait (or already in effect) ends it at once.
  let wait = Math.min(FIRST_LOOK_MS, interval);
  for (;;) {
    try {
      await pause(wait, signal);
    } catch {
      return stop();
    }
    wait = interval;
    const row = await read();
    const file = settled(row);
    if (file) {
      // A ready composition keeps its file whatever its run says, which is why
      // the run is not read here.
      made = file;
      const entry = await filesEntry(made);
      if (entry) return entry;
      // A stop already in effect saw the look just made: that was the last one.
      if (signal?.aborted) throw new MaterializeCancelled(STOPPED);
      ports.onStatus?.("Saved; waiting for its Files entry");
    } else {
      const runId = rowString(row, "runId");
      const dead = runId ? await deadRunMessage(execute, runId) : "";
      if (dead) throw new MaterializeFailed(dead);
      ports.onStatus?.(rowString(row, "status"));
    }
  }
}

export interface HandOffRequest { artifactId: string; statement: string; sources?: MaterializeSource[]; requestId: string }
/**
 * `revisionId` when the revision was opened, `revisionFailure` when it could not
 * be; with `revisionId`, `unattached` lists (by label) the references that went
 * without being attached, and is absent when there are none.
 */
export interface HandOffResult { revisionId?: string; revisionFailure?: string; unattached?: string[] }

// The shape a review comment's attachment takes (editors/productivity
// markdownEditor.ts): the artifact, and the version and revision the Library
// holds for it right now. A type alias rather than an interface so it is
// assignable to the builder's open `Record<string, unknown>[]`.
type ReviewAttachment = { artifactId: string; version: number; revision: string };
// What a reference would be as an attachment, and what it adds to the Library's totals.
type Candidate = { attachment: ReviewAttachment; markdown: boolean; bytes: number };
// A reference as the person listed it, and the attachment it became, if any.
type Reference = { label: string; attachment: ReviewAttachment | null };

// Go's path.Ext, which the Library applies to a file's name: the suffix from the
// last "." of the last path element, or "" when there is none.
function extension(name: string): string {
  const element = name.slice(name.lastIndexOf("/") + 1);
  const dot = element.lastIndexOf(".");
  return dot < 0 ? "" : element.slice(dot);
}

// What the Library will take a file of this name and size for, or "" when it
// will take it for nothing. A size the row does not give is not held against it.
function attachmentKind(name: string, size: number | null): "markdown" | "image" | "" {
  const ext = extension(name).toLowerCase();
  const fits = (limit: number) => size === null || (size > 0 && size <= limit);
  if (ext === ".md") return fits(MAX_MARKDOWN_BYTES) ? "markdown" : "";
  if (IMAGE_EXTENSIONS.has(ext === ".jpg" ? ".jpeg" : ext)) return fits(MAX_IMAGE_BYTES) ? "image" : "";
  return "";
}

// The source as an attachment, or null when it cannot be one: not a file, a file
// that does not resolve or is archived, or a type or size the Library refuses.
async function lookUp(execute: MaterializeExecute, source: MaterializeSource): Promise<Candidate | null> {
  if (source.kind !== "library_file" || !has(source.ref)) return null;
  const fileId = source.ref.trim();
  const [artifactRows, fileRows] = await Promise.all([
    execute("libraryArtifactForFile", buildLibraryArtifactForFile({ fileId })),
    execute("libraryFileById", buildLibraryFileById({ fileId })),
  ]);
  const artifact = first(artifactRows);
  const file = first(fileRows);
  const artifactId = rowString(artifact, "id");
  if (!artifactId || !file || artifact?.["archived"] === true || file["archived"] === true) return null;
  const stored = file["size"];
  const size = typeof stored === "number" && Number.isFinite(stored) ? stored : null;
  const kind = attachmentKind(rowString(file, "name"), size);
  if (!kind) return null;
  // As Documents.read does: a file with no version number is on its first.
  const version = rowNumber(file, "versionNumber") > 0 ? rowNumber(file, "versionNumber") : FIRST_VERSION;
  return { attachment: { artifactId, version, revision: `file:${version}` }, markdown: kind === "markdown", bytes: size ?? 0 };
}

const labelOf = (source: MaterializeSource): string => (source.label ?? "").trim() || (source.ref ?? "").trim() || UNNAMED;

// Every reference in the order given, each with the attachment it becomes or
// none. A reference listed twice counts once, and so does one file reached
// through two ids, because the Library refuses a repeat and nothing is lost by
// sending it once. Attachments are taken in order while they stay inside the
// Library's limits; one that would pass a limit is left off and reported, and a
// later, smaller one may still fit.
async function chooseReferences(execute: MaterializeExecute, sources: MaterializeSource[]): Promise<Reference[]> {
  const seen = new Set<string>();
  const distinct = sources.filter(source => {
    if (!has(source.ref)) return true;
    const key = `${source.kind} ${source.ref.trim()}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
  const found = await Promise.all(distinct.map(source => lookUp(execute, source)));
  const references: Reference[] = [];
  const taken = new Set<string>();
  let count = 0;
  let bytes = 0;
  let markdownBytes = 0;
  for (const [index, source] of distinct.entries()) {
    const candidate = found[index] ?? null;
    let attachment: ReviewAttachment | null = null;
    if (candidate) {
      if (taken.has(candidate.attachment.artifactId)) continue;
      if (count < MAX_ATTACHMENTS && bytes + candidate.bytes <= MAX_ATTACHMENT_BYTES &&
          (!candidate.markdown || markdownBytes + candidate.bytes <= MAX_MARKDOWN_TOTAL_BYTES)) {
        taken.add(candidate.attachment.artifactId);
        count++;
        bytes += candidate.bytes;
        if (candidate.markdown) markdownBytes += candidate.bytes;
        attachment = candidate.attachment;
      }
    }
    references.push({ label: labelOf(source), attachment });
  }
  return references;
}

const attachmentsOf = (references: Reference[]): ReviewAttachment[] =>
  references.flatMap(reference => (reference.attachment ? [reference.attachment] : []));
const errorText = (error: unknown): string => (error instanceof Error ? error.message : String(error));

// The calls whose refusals this module hands a caller to show go through here:
// a refusal comes back as the cluster's sentence alone, the name of the call
// that was made, the engine's frame and any Go package name taken off
// (refusalSentence). The original error is kept as the cause.
function inPlainWords(execute: MaterializeExecute): MaterializeExecute {
  return async (name, call) => {
    try {
      return await execute(name, call);
    } catch (error) {
      throw new Error(refusalSentence(error, name), { cause: error });
    }
  };
}

/**
 * Gives a described file's description to the Library's revision workflow, as
 * the person: a document-level comment carrying the statement (the library_file
 * sources the Library accepts attached), then a request to write the document
 * from it. The same calls VS Code's "Describe document" makes
 * (editors/productivity documents.ts comment() and requestRevision()).
 *
 * Returns `{ revisionId }`, the Library request id that follows the revision, or
 * `{ revisionFailure }` with the reason. NEVER THROWS: the file exists and is
 * good, and is never removed because a later step failed.
 *
 * A reference the Library cannot attach does not stop the hand-off: the
 * revision opens with the ones it can, and the labels of the rest come back in
 * `unattached` (absent when there are none) for the caller to show. That is
 * every reference that is not a file, that does not resolve, or whose name or
 * size the Library refuses (it takes Markdown up to 32 KiB and images, eight at
 * most, within its totals). What only the bytes can show is the Library's to
 * judge: if it refuses the attachments, the comment is sent once more without
 * them, under the same request id, and all of them are reported.
 *
 * The comment's id comes from the comment's own receipt. The review listing
 * cannot stand in for it: it withholds each comment's request id.
 */
export async function handOffDescription(execute: MaterializeExecute, request: HandOffRequest): Promise<HandOffResult> {
  try {
    const run = inPlainWords(execute);
    const { artifactId } = request;
    const requestId = (request.requestId ?? "").trim();
    if (!has(request.statement)) throw new Error("there is no description to send.");
    if (!HAND_OFF_ID.test(requestId)) throw new Error("request ids for a description use letters, digits, - and _ (at most 111 characters).");
    let references = await chooseReferences(run, request.sources ?? []);
    const postComment = async (): Promise<Row | null> => first(await run("libraryAddDocumentComment", buildLibraryAddDocumentComment({
      artifactId, expectedVersion: FIRST_VERSION, expectedRevision: FIRST_REVISION,
      anchor: WHOLE_DOCUMENT, body: request.statement, requestId: `${requestId}-comment`, attachments: attachmentsOf(references),
    })));
    let comment: Row | null;
    try {
      comment = await postComment();
    } catch (error) {
      if (attachmentsOf(references).length === 0 || !ATTACHMENT_REFUSAL.test(errorText(error))) throw error;
      references = references.map(({ label }) => ({ label, attachment: null }));
      comment = await postComment();
    }
    const commentId = rowString(comment, "commentId");
    if (!comment?.["saved"] || !commentId) throw new Error("the cluster did not confirm the description. Retry to recover it.");
    const revisionId = `${requestId}-revision`;
    const revision = first(await run("libraryRequestDocumentRevision", buildLibraryRequestDocumentRevision({
      artifactId, expectedVersion: FIRST_VERSION, expectedRevision: FIRST_REVISION,
      commentIds: [commentId], instruction: "", requestId: revisionId,
    })));
    if (!revision?.["runId"] || !revision["proposal"]) throw new Error("the cluster did not confirm the writing request. Retry to recover it.");
    const unattached = references.filter(reference => reference.attachment === null).map(reference => reference.label);
    return unattached.length > 0 ? { revisionId, unattached } : { revisionId };
  } catch (error) {
    return { revisionFailure: HAND_OFF_FAILED + errorText(error) };
  }
}

/**
 * Creates a file: starts the composition, follows it to its Files entry, and,
 * for a described Markdown request, hands the description to the Library's
 * revision workflow, returning its `revisionId` or `revisionFailure` on the
 * result, with the references it could not attach in `unattached`. Resolves once
 * the FILE exists even when the hand-off failed, or was stopped. A stop never
 * deletes a file that was already made: when the file is already in Files at the
 * moment of the stop, it is returned, with the hand-off skipped and (for a
 * described request) `revisionFailure` "Stopped before the description was sent."
 * A stop before the file exists, or before its Files entry does, throws
 * MaterializeCancelled. See the header for the request id rules.
 *
 * The hand-off itself is not interruptible: once it has begun it finishes.
 */
export async function materialize(ports: MaterializePorts, request: MaterializeRequest): Promise<MaterializeResult> {
  if (ports.signal?.aborted) throw new MaterializeCancelled(STOPPED);
  const result = await followComposition(ports, await startMaterialize(ports.execute, request));
  if (!isDescribedMarkdown(request)) return result;
  // The description is the one step here that spends model calls. A stop that
  // arrived while the file was being found ends it, but not the file.
  if (ports.signal?.aborted) return { ...result, revisionFailure: STOPPED_BEFORE_HAND_OFF };
  const handOff = await handOffDescription(ports.execute, {
    artifactId: result.artifactId, statement: request.statement ?? "", sources: request.sources, requestId: request.requestId,
  });
  return { ...result, ...handOff };
}
