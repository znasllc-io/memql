import { rowBool, rowNumber, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../kit/rows";

import {
  ANALYSIS_TEMPLATE,
  CORPUS_GROUP_ID,
  CORPUS_GROUP_LABEL,
  FILE_ANALYZING,
  FILE_FAILED,
  FILE_READY,
  REJECTED,
  UNVALIDATED,
  VALIDATED,
  type FileStage,
} from "./concepts";

// The Training app's projections: raw wire rows in, the app's own types out.
//
// PURE, AND TESTED WITHOUT A DOM. A LiveCollection holds RAW rows -- its fold
// upserts an arriving event's payload AS the row type with no projection hook
// -- so every predicate this app applies has to run on a projected result
// rather than on whatever the collection happens to hold. That is what
// `useLiveView` is for, and these are the functions it runs.

// ---------------------------------------------------------------------------
// Files
// ---------------------------------------------------------------------------

export interface TrainingFile {
  id: string;
  name: string;
  mimeType: string;
  size: number;
  status: string;
  summary: string;
  failureReason: string;
  embeddingStatus: string;
  trainedIntoDomainIds: string[];
  archived: boolean;
  createdAt: string;
}

export function fileFromRow(raw: Row): TrainingFile {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    name: rowString(row, "name"),
    mimeType: rowString(row, "mimeType"),
    size: rowNumber(row, "size"),
    status: rowString(row, "status"),
    summary: rowString(row, "summary"),
    failureReason: rowString(row, "failureReason"),
    embeddingStatus: rowString(row, "embeddingStatus"),
    trainedIntoDomainIds: stringList(row, "trainedIntoDomainIds"),
    archived: rowBool(row, "archived"),
    createdAt: rowString(row, "createdAt"),
  };
}

/**
 * Whether a file belongs on this surface.
 *
 * NO `requestedBy` RESIDUAL, and that is the re-key's quiet security gain.
 * The old plan feed filtered other people's rows out CLIENT-SIDE, because
 * `v1:planner:plan` declares no row-authz tier and a concept that declares
 * nothing admits every subscriber. `v1:library:file` declares
 * `@rowAuthz(owner="ownerUserId", clusterOwner)`, so admission runs on the
 * SUBSCRIPTION as well as the read (memql#4309) and other people's files
 * never arrive here at all. What is left is this app's own business: an
 * archived file is in the Bin, and the Bin is where you restore it.
 */
export function fileBelongsHere(file: TrainingFile): boolean {
  if (file.id === "") return false;
  return !file.archived;
}

/**
 * What a person would call a change on a file.
 *
 * A HEARTBEAT IS NOT NEWS: nothing here moves on a timer. `status` is the
 * pipeline, `embeddingStatus` settles once, `trainedIntoDomainIds` changes
 * when somebody teaches a domain, and `failureReason` arrives with a failure.
 * `summary` is deliberately absent even though it is written once -- it lands
 * in the same write as `status: "ready"`, so naming it would announce the
 * same event twice.
 */
export function fileFingerprint(file: TrainingFile): string {
  return [
    file.status,
    file.embeddingStatus,
    file.failureReason,
    file.trainedIntoDomainIds.join(","),
  ].join("|");
}

// ---------------------------------------------------------------------------
// Analysis runs
// ---------------------------------------------------------------------------

export interface AnalysisRun {
  id: string;
  template: string;
  status: string;
  /** The file this run is about, off the run's own input envelope. */
  fileId: string;
  errorMessage: string;
  startedAt: string;
  finishedAt: string;
  /** Whether the pass found text at all. */
  readable: boolean;
  /** How many passages the file was split into, and how many are searchable. */
  passages: number;
  embedded: number;
  summarized: boolean;
}

export function runFromRow(raw: Row): AnalysisRun {
  const row = flatten(raw);
  const input = objectAt(row, "input");
  const outcome = objectAt(row, "outcome");
  return {
    id: rowString(row, "id"),
    template: rowString(row, "automationName"),
    status: rowString(row, "status"),
    fileId: stringAt(input, "fileId"),
    errorMessage: rowString(row, "errorMessage"),
    startedAt: rowString(row, "startedAt"),
    finishedAt: rowString(row, "finishedAt"),
    // ABSENT IS NOT FALSE for `readable`: a run still in flight has written
    // no outcome, and reading that as "there is nothing in this file" would
    // label every file unreadable for as long as the pass takes. The caller
    // asks the run's STATUS first, which is what `stageOf` does.
    readable: boolAt(outcome, "readable"),
    passages: numberAt(outcome, "chunks"),
    embedded: numberAt(outcome, "embedded"),
    summarized: boolAt(outcome, "summarized"),
  };
}

/**
 * Whether a run belongs on this surface.
 *
 * The template check is a filter this app owns: `workRunsForOwner` returns
 * every run the caller owns because Nexus needs that, and this app shows
 * analyses. Owner scoping is the ENGINE's -- `v1:work:run` declares the
 * composite owner tier, so nobody else's runs reach this browser.
 */
export function runBelongsHere(run: AnalysisRun): boolean {
  if (run.id === "" || run.fileId === "") return false;
  return run.template === ANALYSIS_TEMPLATE;
}

export function runFingerprint(run: AnalysisRun): string {
  return `${run.status}|${run.finishedAt}|${run.passages}|${run.embedded}`;
}

/** The newest run per file. Runs arrive newest-first, so the first win. */
export function runsByFile(runs: readonly AnalysisRun[]): Map<string, AnalysisRun> {
  const byFile = new Map<string, AnalysisRun>();
  for (const run of runs) {
    if (!byFile.has(run.fileId)) byFile.set(run.fileId, run);
  }
  return byFile;
}

// ---------------------------------------------------------------------------
// The stage: what a person is looking at, and therefore what they may do
// ---------------------------------------------------------------------------

/**
 * Fold a file and its newest run into ONE state.
 *
 * THE FILE ROW LEADS AND THE RUN DECORATES. Every branch below can be decided
 * from the file alone, and the run is consulted only for `unreadable` -- which
 * the file row genuinely cannot express, because a stored image and a read
 * spreadsheet both end at `ready`. That ordering matters: the file row is
 * written synchronously by the upload route and the run row is written by a
 * detached goroutine, so a surface that waited for the run would show nothing
 * for the first moments of every upload.
 *
 * A file with NO run at all reads from its own status. That is not a
 * degradation to paper over: it is exactly what a cluster with no journal
 * wired looks like, and what every file uploaded before this epic looks like.
 */
export function stageOf(file: TrainingFile, run: AnalysisRun | undefined): FileStage {
  if (file.status === FILE_FAILED) return "failed";
  if (file.status === FILE_ANALYZING) return "reading";
  if (file.status !== FILE_READY) return "reading";
  // Ready. The run says whether there was anything in it to read.
  if (run !== undefined && run.status === "succeeded" && !run.readable) return "unreadable";
  // No run, and nothing was embedded: the same shape as an unreadable file,
  // and the honest reading when the journal is not wired.
  if (run === undefined && file.embeddingStatus === "complete" && file.summary === "") {
    return "unreadable";
  }
  return file.trainedIntoDomainIds.length > 0 ? "trained" : "untrained";
}

/**
 * The dot's reading of a file.
 *
 * The kit's `ProvenanceDot` is green = reachable now, amber = not reachable,
 * unknown = NO DOT, and the same component renders the dock's "running", the
 * connection state and the fleet's "online" -- so aliveness reads identically
 * everywhere. This maps onto it rather than inventing a fourth dot:
 *
 *   reading            -> reachable.   Work is happening right now.
 *   failed             -> unreachable. It stopped and did not finish.
 *   everything else    -> unknown, so NO dot. QUIET IS THE SETTLED STATE:
 *                         painting every trained file green would make a page
 *                         of finished work look like a page of running work.
 */
export function fileDotTone(stage: FileStage): "reachable" | "unreachable" | "unknown" {
  if (stage === "failed") return "unreachable";
  if (stage === "reading" || stage === "uploading") return "reachable";
  return "unknown";
}

function stringList(row: Row, key: string): string[] {
  const raw = (row as Record<string, unknown>)[key];
  if (!Array.isArray(raw)) return [];
  return raw.filter((v): v is string => typeof v === "string" && v.trim() !== "");
}

function objectAt(row: Row, key: string): Record<string, unknown> {
  const raw = (row as Record<string, unknown>)[key];
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) return {};
  return raw as Record<string, unknown>;
}

function stringAt(obj: Record<string, unknown>, key: string): string {
  const raw = obj[key];
  return typeof raw === "string" ? raw : "";
}

function numberAt(obj: Record<string, unknown>, key: string): number {
  const raw = obj[key];
  return typeof raw === "number" && Number.isFinite(raw) ? raw : 0;
}

function boolAt(obj: Record<string, unknown>, key: string): boolean {
  return obj[key] === true;
}

// ---------------------------------------------------------------------------
// Chunks
// ---------------------------------------------------------------------------

export interface Chunk {
  id: string;
  domainId: string;
  text: string;
  sourceRef: string;
  seq: number;
  tokenCount: number;
  documentId: string;
  source: string;
  sourceTopic: string;
  validationStatus: string;
  superseded: boolean;
  supersededReason: string;
  createdAt: string;
}

/**
 * `validationStatus` falls back to "unvalidated" when the key is ABSENT, which
 * is what the concept's own `@default` says a chunk with no stored value is.
 * A blank string gets the same treatment for the same reason -- both mean
 * "nobody has decided", and the queue is defined by that state.
 */
export function chunkFromRow(row: Row): Chunk {
  const status = rowString(row, "validationStatus").trim();
  return {
    id: rowString(row, "id"),
    domainId: rowString(row, "domainId"),
    text: rowString(row, "text"),
    sourceRef: rowString(row, "sourceRef"),
    seq: rowNumber(row, "seq"),
    tokenCount: rowNumber(row, "tokenCount"),
    documentId: rowString(row, "documentId"),
    source: rowString(row, "source"),
    sourceTopic: rowString(row, "sourceTopic"),
    validationStatus: status === "" ? UNVALIDATED : status,
    superseded: rowBool(row, "superseded"),
    supersededReason: rowString(row, "supersededReason"),
    createdAt: rowString(row, "createdAt"),
  };
}

/**
 * Whether a chunk is work.
 *
 * SUPERSEDED IS CHECKED FIRST and is not a status. A chunk the Trainer Agent
 * marked outdated is history -- retrieval already excludes it -- and one that
 * was superseded before anybody reviewed it is still `unvalidated`, so a queue
 * that only looked at the status would ask somebody to approve content the
 * engine has already stopped using.
 */
export function chunkAwaitsReview(chunk: Chunk): boolean {
  if (chunk.id === "") return false;
  if (chunk.superseded) return false;
  return chunk.validationStatus === UNVALIDATED;
}

export function chunkFingerprint(chunk: Chunk): string {
  return `${chunk.validationStatus}|${chunk.superseded}|${chunk.text.length}`;
}

// ---------------------------------------------------------------------------
// Grouping and rollups
// ---------------------------------------------------------------------------

export interface ChunkGroup {
  /** `documentId`, or "" for the seeded-corpus group. */
  id: string;
  label: string;
  chunks: Chunk[];
}

/**
 * Chunks as review cards: one group per upload batch, plus one for everything
 * with no back-reference.
 *
 * ORDER IS THE INPUT'S. The reads that feed this are already sorted newest
 * first by the engine, and a second sort here would be a second opinion about
 * an ordering the caller can see -- groups therefore appear in the order their
 * first chunk did. The corpus group is LAST regardless, because it is the
 * standing pile rather than something that just happened.
 */
export function groupChunksByDocument(chunks: readonly Chunk[]): ChunkGroup[] {
  const groups: ChunkGroup[] = [];
  const byId = new Map<string, ChunkGroup>();
  let corpus: ChunkGroup | null = null;

  for (const chunk of chunks) {
    const documentId = chunk.documentId.trim();
    if (documentId === CORPUS_GROUP_ID) {
      if (corpus === null) {
        corpus = { id: CORPUS_GROUP_ID, label: CORPUS_GROUP_LABEL, chunks: [] };
      }
      corpus.chunks.push(chunk);
      continue;
    }
    let group = byId.get(documentId);
    if (group === undefined) {
      group = { id: documentId, label: documentId, chunks: [] };
      byId.set(documentId, group);
      groups.push(group);
    }
    group.chunks.push(chunk);
  }

  if (corpus !== null) groups.push(corpus);
  return groups;
}

export interface DomainRollup {
  domainId: string;
  total: number;
  validated: number;
  unvalidated: number;
  rejected: number;
}

/**
 * The per-domain rollup, from ONE pass over `allDocumentChunkDomains`.
 *
 * This is what the `validationStatus` key added to `documentChunkDomainLite`
 * buys (memql#4740). Without it the only way to show a breakdown was to page
 * `documentChunksForDomain` per domain and count the first 50 -- a number that
 * looks like a total and is not one.
 *
 * THE THREE PARTS SUM TO `total` BY CONSTRUCTION: an absent or unrecognised
 * status counts as `unvalidated`, which is what the concept's `@default` says
 * a row with no stored value is. A fourth bucket for "something else" would
 * make the rollup stop adding up, and a reader checking the arithmetic is
 * exactly the reader this surface is for.
 */
export function rollupDomains(rows: readonly Row[]): DomainRollup[] {
  const byDomain = new Map<string, DomainRollup>();
  const order: string[] = [];

  for (const row of rows) {
    const domainId = rowString(row, "domainId").trim();
    if (domainId === "") continue;
    let rollup = byDomain.get(domainId);
    if (rollup === undefined) {
      rollup = { domainId, total: 0, validated: 0, unvalidated: 0, rejected: 0 };
      byDomain.set(domainId, rollup);
      order.push(domainId);
    }
    rollup.total += 1;
    const status = rowString(row, "validationStatus").trim();
    if (status === VALIDATED) rollup.validated += 1;
    else if (status === REJECTED) rollup.rejected += 1;
    else rollup.unvalidated += 1;
  }

  // Sorted by name, not by count: a domain that gains a chunk must not jump
  // the list under somebody reading it, and the alphabet is the one ordering
  // that is stable against the data.
  order.sort((a, b) => a.localeCompare(b));
  return order.map((domainId) => byDomain.get(domainId)!);
}

// ---------------------------------------------------------------------------
// The domain row itself (epic memql#4800)
// ---------------------------------------------------------------------------

/**
 * A knowledge domain's own facts, as opposed to a rollup of its chunks.
 *
 * NEW, because the concept is. `v1:knowledge:knowledgeDomain` had rows and no
 * declaration anywhere in this tree until epic memql#4800 -- the catalog
 * seeder wrote them through a mutation the engine could not resolve -- so this
 * page has always labelled a card by its raw `domainId` and said so on screen.
 * With the concept declared, projected and readable, a card can say the
 * domain's NAME and render the client it is tagged with.
 */
export interface DomainMeta {
  id: string;
  name: string;
  category: string;
  tier: string;
  /**
   * The client this domain was trained for (D5). A TAG AND NOTHING MORE:
   * agent routing, domain attachment, retrieval and scoring are all
   * deliberately unaffected, and this page renders and filters by it. A domain
   * carrying one behaves identically to a domain that does not.
   */
  accountId: string;
}

export function domainMetaFromRow(raw: Row): DomainMeta {
  const row = flatten(raw);
  return {
    id: rowString(row, "id"),
    name: rowString(row, "name"),
    category: rowString(row, "category"),
    tier: rowString(row, "tier"),
    accountId: rowString(row, "accountId"),
  };
}
