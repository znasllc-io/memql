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
// REFERENCES THE LIBRARY CANNOT ATTACH ARE REPORTED, NOT FATAL. The Library
// takes only Markdown (up to 32 KiB) and images as review attachments, and
// refuses a comment that carries anything else. So the hand-off attaches the
// library_file sources the Library accepts, opens the revision with them, and
// returns every other reference (a PDF, a query, a file it cannot find) by label
// in `unattached` for the caller to show: "2 references were not attached: only
// Markdown and images can be".
//
// handOffDescription derives its two Library request ids from the request id
// (`<id>-comment`, `<id>-revision`), so a retry returns the first receipts. The
// Library accepts only letters, digits, "-" and "_" (8 to 120 characters) as a
// request id, so a request id used with a description is at most 111 of them.
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
import { rowNumber, rowString, type Row } from "./types.js";

/** Runs one named construct as the signed-in person and returns its rows. `call` is the rendered call string. */
export type MaterializeExecute = (name: string, call: string) => Promise<Record<string, unknown>[]>;
export type MaterializeFormat = "markdown" | "html" | "txt" | "csv" | "json" | "docx" | "pdf";
export interface MaterializeSource { kind: "concept_row" | "library_file" | "query"; ref: string; label: string; content?: boolean; includeImages?: boolean }
export interface MaterializeRequest { name: string; format: MaterializeFormat; requestId: string; statement?: string; sources?: MaterializeSource[]; templateId?: string; folderId?: string; outputKind?: "email_template"; accountIds?: string[]; recipeId?: string; deployableKind?: string }
/**
 * What the helpers need from their caller. `signal` stops the follow and
 * cancels the composition; `onStatus` hears every poll's status; `intervalMs`
 * is the poll interval (default two seconds).
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
 * a statement, and nothing of its own to compose from. (A draft or section keys
 * also rule it out; startMaterialize never sends either.)
 */
export function isDescribedMarkdown(request: Pick<MaterializeRequest, "format" | "statement" | "templateId" | "recipeId" | "outputKind" | "deployableKind">): boolean {
  return request.format === "markdown" && has(request.statement) &&
    !has(request.templateId) && !has(request.recipeId) && !has(request.outputKind) && !has(request.deployableKind);
}

/**
 * Opens the composition and returns its id. Sends only the fields
 * MaterializeRequest declares, and omits the ones left empty, so what the
 * server sees is what isDescribedMarkdown judged.
 */
export async function startMaterialize(execute: MaterializeExecute, request: MaterializeRequest): Promise<string> {
  if (!has(request.requestId)) throw new Error("A request id is required, so that a repeated request makes one file.");
  const args: ComposeMaterializeArgs = { name: request.name, format: request.format, requestId: request.requestId };
  if (has(request.statement)) args.statement = request.statement;
  if (request.sources?.length) args.sources = request.sources.map(source => ({ ...source }));
  if (has(request.templateId)) args.templateId = request.templateId;
  if (has(request.folderId)) args.folderId = request.folderId;
  if (has(request.outputKind)) args.outputKind = request.outputKind;
  if (request.accountIds?.length) args.accountIds = [...request.accountIds];
  if (has(request.recipeId)) args.recipeId = request.recipeId;
  if (has(request.deployableKind)) args.deployableKind = request.deployableKind;
  const reply = first(await execute("composeMaterialize", buildComposeMaterialize(args)));
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
 * each status through `onStatus`. Throws MaterializeFailed with the
 * composition's reason (or its run's, when the run stopped first) and
 * MaterializeCancelled when it was stopped. An aborted `signal` cancels the
 * composition once and stops polling; if that cancel request itself fails, its
 * error is thrown instead, because the composition may still finish.
 *
 * Never hands a description to the Library: see the header, and materialize.
 */
export async function followComposition(ports: MaterializePorts, compositionId: string): Promise<MaterializeResult> {
  const { execute, signal } = ports;
  const interval = ports.intervalMs ?? DEFAULT_INTERVAL_MS;
  const stop = async (): Promise<never> => {
    await execute("composeCancel", buildComposeCancel({ compositionId, reason: STOP_REASON }));
    throw new MaterializeCancelled(STOPPED);
  };
  for (;;) {
    if (signal?.aborted) return stop();
    const row = first(await execute("compositionById", buildCompositionById({ compositionId })));
    if (!row) throw new MaterializeFailed("This composition is no longer readable. Its history remains in Materializer.");
    const status = rowString(row, "status");
    if (status === "failed") throw new MaterializeFailed(rowString(row, "failureReason") || "Materializer could not finish this file.");
    if (status === "cancelled") throw new MaterializeCancelled(STOPPED);
    if (status === "ready") {
      // A new file's Files entry is written after the file, by an automation, so
      // it may not be there yet. A ready composition keeps its file whatever its
      // run says, which is why the run is not read here.
      const fileId = rowString(row, "outputFileId");
      const artifact = first(await execute("libraryArtifactForFile", buildLibraryArtifactForFile({ fileId })));
      const artifactId = rowString(artifact, "id");
      if (artifactId) return { compositionId, fileId, artifactId, name: rowString(artifact, "title") || rowString(row, "name") };
      ports.onStatus?.("Saved; waiting for its Files entry");
    } else {
      const runId = rowString(row, "runId");
      const dead = runId ? await deadRunMessage(execute, runId) : "";
      if (dead) throw new MaterializeFailed(dead);
      ports.onStatus?.(status);
    }
    try {
      await pause(interval, signal);
    } catch {
      return stop();
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
    const { artifactId, requestId } = request;
    if (!has(request.statement)) throw new Error("there is no description to send.");
    if (!HAND_OFF_ID.test(requestId)) throw new Error("request ids for a description use letters, digits, - and _ (at most 111 characters).");
    let references = await chooseReferences(execute, request.sources ?? []);
    const postComment = async (): Promise<Row | null> => first(await execute("libraryAddDocumentComment", buildLibraryAddDocumentComment({
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
    const revision = first(await execute("libraryRequestDocumentRevision", buildLibraryRequestDocumentRevision({
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
 * the FILE exists even when the hand-off failed. See the header for the request
 * id rules.
 */
export async function materialize(ports: MaterializePorts, request: MaterializeRequest): Promise<MaterializeResult> {
  if (ports.signal?.aborted) throw new MaterializeCancelled(STOPPED);
  const result = await followComposition(ports, await startMaterialize(ports.execute, request));
  if (!isDescribedMarkdown(request)) return result;
  // The description is the one step here that spends model calls. A stop that
  // arrived while the file was being found ends it, as a stop anywhere else does.
  if (ports.signal?.aborted) throw new MaterializeCancelled(STOPPED);
  const handOff = await handOffDescription(ports.execute, {
    artifactId: result.artifactId, statement: request.statement ?? "", sources: request.sources, requestId: request.requestId,
  });
  return { ...result, ...handOff };
}
