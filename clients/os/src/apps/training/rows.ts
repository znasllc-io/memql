import { rowBool, rowNumber, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import {
  ANALYZE_PLAN_KIND,
  CORPUS_GROUP_ID,
  CORPUS_GROUP_LABEL,
  REJECTED,
  TERMINAL_PLAN_STATUSES,
  UNVALIDATED,
  VALIDATED,
} from "./concepts";

// The Training app's projections: raw wire rows in, the app's own types out.
//
// PURE, AND TESTED WITHOUT A DOM. A LiveCollection holds RAW rows -- its fold
// upserts an arriving event's payload AS the row type with no projection hook
// -- so every predicate this app applies has to run on a projected result
// rather than on whatever the collection happens to hold. That is what
// `useLiveView` is for, and these are the functions it runs.

// ---------------------------------------------------------------------------
// Analysis plans
// ---------------------------------------------------------------------------

export interface AnalysisPlan {
  id: string;
  kind: string;
  status: string;
  goal: string;
  requestedBy: string;
  errorMessage: string;
  createdAt: string;
  completedAt: string;
}

export function planFromRow(row: Row): AnalysisPlan {
  return {
    id: rowString(row, "id"),
    kind: rowString(row, "kind"),
    status: rowString(row, "status"),
    goal: rowString(row, "goal"),
    requestedBy: rowString(row, "requestedBy"),
    errorMessage: rowString(row, "errorMessage"),
    createdAt: rowString(row, "createdAt"),
    completedAt: rowString(row, "completedAt"),
  };
}

/**
 * Whether a plan row belongs on this surface.
 *
 * BOTH HALVES ARE CLIENT-SIDE, and they are client-side for two different
 * reasons that must not be collapsed:
 *
 *   - `kind` is a filter this app owns. `plansForUser` returns every plan the
 *     caller requested, whatever its kind, because that is the query the Nexus
 *     goal picker needs; narrowing it server-side would be a change to a
 *     surface this app does not own.
 *   - `requestedBy` is a RESIDUAL. `v1:planner:plan` declares no row-authz
 *     tier (memql#4366), so while the SEED is server-scoped
 *     (`plansForUser` binds `requestedBy==actor.userId`), the SUBSCRIPTION is
 *     not: a concept that declares nothing admits every subscriber, so
 *     `graph.node.*.v1:planner:*` delivers other people's plans to this
 *     browser. Nexus labels the same filter the same way, and the residual is
 *     recorded in `docs/public/operate/auth/per-row-authz-audit.md`.
 *
 * A BLANK viewer id matches NOTHING rather than everything. Access resolves
 * asynchronously, and "show every plan in the cluster until we know who is
 * looking" is the exact shape of the bug this filter exists to prevent.
 */
export function planBelongsHere(plan: AnalysisPlan, viewerUserId: string): boolean {
  if (plan.id === "") return false;
  if (plan.kind !== ANALYZE_PLAN_KIND) return false;
  if (viewerUserId.trim() === "") return false;
  return plan.requestedBy === viewerUserId;
}

/**
 * What a person would call a change on an analysis plan.
 *
 * A HEARTBEAT IS NOT NEWS: nothing here moves on a timer. `status` is the
 * whole point of the surface, `errorMessage` arrives with a failure, and
 * `completedAt` is stamped once. Token counters and metrics are deliberately
 * absent -- they tick while a plan runs, and naming one would pulse the row
 * continuously for the life of the analysis.
 */
export function planFingerprint(plan: AnalysisPlan): string {
  return `${plan.status}|${plan.completedAt}|${plan.errorMessage}`;
}

export function planIsTerminal(plan: AnalysisPlan): boolean {
  return TERMINAL_PLAN_STATUSES.includes(plan.status);
}

/**
 * The dot's reading of a plan.
 *
 * The kit's `ProvenanceDot` is green = reachable now, amber = not reachable,
 * unknown = NO DOT, and the same component renders the dock's "running", the
 * connection state and the fleet's "online" -- so aliveness reads identically
 * everywhere. This maps onto it rather than inventing a fourth dot:
 *
 *   running            -> reachable.   Work is happening right now.
 *   failed / cancelled -> unreachable. The work stopped and did not finish;
 *                                      amber is what the shell says that with.
 *   queued / succeeded -> unknown, so NO dot. A queued plan has not started
 *                                      and a succeeded one is over, and
 *                                      neither is a liveness reading. QUIET IS
 *                                      THE SUCCESS STATE: painting every
 *                                      finished analysis green would make a
 *                                      page of history look like a page of
 *                                      running work.
 */
export function planDotTone(plan: AnalysisPlan): "reachable" | "unreachable" | "unknown" {
  if (plan.status === "failed" || plan.status === "cancelled") return "unreachable";
  if (plan.status === "running") return "reachable";
  return "unknown";
}

/**
 * The file this plan is about.
 *
 * `CreateQueuedAnalyzePlan` writes the goal as `Analyze <name>`, so the name
 * is recoverable -- but the prefix is the SERVER's wording and this parse is a
 * courtesy over it. A goal that does not carry the prefix renders whole rather
 * than being blanked: a plan whose goal we cannot parse still has a goal.
 */
export function planFileName(plan: AnalysisPlan): string {
  const goal = plan.goal.trim();
  const prefix = "Analyze ";
  return goal.startsWith(prefix) ? goal.slice(prefix.length).trim() || goal : goal;
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
