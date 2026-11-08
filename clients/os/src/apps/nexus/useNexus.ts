import { useCallback, useEffect, useState } from "react";
import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { readArtifact } from "../../nexus/scene/world";
import { useOsConnection } from "../../live/connection";
import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import {
  APPROVAL_CONCEPT,
  ARTIFACT_CONCEPT,
  GOAL_CONCEPT,
  RUN_CONCEPT,
  STEP_CONCEPT,
} from "./concepts";
import { approvalFromRow, approvalIsPending, runFromRow, modelCallFromRow, observationFromRow, sameRow, stepFromRow, type ModelCallRow, type ObservationRow } from "./rows";

// Complete summary feeds are reserved for the Overview and the unresolved
// approval-recovery set. Resource browsing uses useNexusDirectory's manual
// ten-row pages; selected records and their joins are scoped by identity.
// Steps and artifacts belong only to the opened run. The journal's concepts
// do not broadcast, so its bounded reads poll while that run remains active.

/** Every goal this caller owns, newest first. */
export function useGoals(enabled = true): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>(enabled ? "work:goals" : null, (connection) => ({
    concept: GOAL_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.workGoalsForOwner({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, GOAL_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

/**
 * Every run this caller owns, newest first.
 *
 * Page through summaries, retaining the same unfiltered history for every
 * view. Full inputs, compiled workflows and outputs can be megabytes each.
 * Re-read events through the same projection so updates cannot put them back
 * into this app-wide collection. Run steps are still loaded only on opening.
 */
export function useRuns(enabled = true, recoveryOnly = false): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>(enabled ? (recoveryOnly ? "work:approval-recovery-runs" : "work:runs") : null, (connection) => ({
    concept: RUN_CONCEPT,
    seed: async (cursor, signal) => {
      const method = recoveryOnly ? connection.query.workApprovalRecoveryRuns.bind(connection.query) : connection.query.workRunSummariesForOwner.bind(connection.query);
      const result = await method({}, { signal, ...(cursor !== "" ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    reread: async (rowId, signal) => {
      const result = await connection.query.workRunSummaryForOwner({ runId: rowId }, { signal });
      return result.rows()[0] ?? null;
    },
    rereadEveryEvent: true,
    inScope: row => !recoveryOnly || (() => { const run = runFromRow(row); return !run.cancelRequested && ((run.status === "waiting" && run.waitingOnKind === "approval") || (run.status === "abandoned" && run.errorCode === "run_abandoned" && !!run.humanResumeId)); })(),
  }));
}

/**
 * The caller's approvals, seeded from the pending queue.
 *
 * The seed carries `decision==""` in the DSL. Live updates retain a decided
 * row so an open detail can show its receipt; the list and waiting count
 * must project only pending rows. A decision is an update, not a deletion.
 */
export function useApprovals(enabled = true): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>(enabled ? "work:approvals" : null, (connection) => ({
    concept: APPROVAL_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.workApprovalsForOwner({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, APPROVAL_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

/**
 * The nightly routing reviews the viewer may decide (`workRoutingReviews`).
 *
 * NOBODY OWNS ONE. The evidence fold raises a routing review as the cluster,
 * so the person's own approval reads never list it; the builtin answers an
 * owner or a developer and nobody else (design record 2026-11-08). Row
 * admission sends a row nobody owns to cluster owners only, so a DEVELOPER
 * hears nothing when one is decided: the approvals section re-reads this
 * collection after deciding one, and every collection re-reads on reconnect
 * and resume. An owner's events still fold in through `inScope`, and an
 * id-only one is re-read through the builtin.
 */
export function useRoutingReviews(enabled = true): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>(enabled ? "work:routing-reviews" : null, (connection) => ({
    concept: APPROVAL_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.workRoutingReviews({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const result = await connection.query.workRoutingReviews({ approvalId: rowId }, { signal });
      return result.rows()[0] ?? null;
    },
    inScope: (row) => isPendingRoutingReview(row),
    paged: false,
  }));
}

/** The approval kind the nightly routing review raises. */
export const ROUTING_REVIEW = "routingReview";

/** Whether a row is a routing review still waiting for a decision. */
export function isPendingRoutingReview(row: Row): boolean {
  const approval = approvalFromRow(row);
  return approval.kind === ROUTING_REVIEW && approvalIsPending(approval);
}

/**
 * The steps of ONE run, retained by the page that opened it.
 *
 * `runId` is the key, so opening a different run is a different collection --
 * a new seed and a new baseline, which is right: the previous run's steps are
 * not rows this one is missing.
 *
 * NOT KEYED ON ANYTHING ASYNC. The key is the id the person clicked, which is
 * present at the moment they click it; keying on something that arrives later
 * (a resolved owner, a settled config) re-subscribes the moment it lands and
 * throws away the seed that was already running.
 */
export function useRunSteps(runId: string): LiveCollectionHandle<Row> {
  const key = runId.trim() === "" ? null : `work:steps:${runId}`;
  return useLiveCollection<Row>(key, (connection) => ({
    concept: STEP_CONCEPT,
    inScope: (row) => sameRow(stepFromRow(row).runId, runId),
    seed: async (_cursor, signal) => {
      const result = await connection.query.workStepsForOwnerRun({ runId }, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, STEP_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

/**
 * What ONE run produced -- the Library index rows, not the bytes.
 *
 * Retained by the page, keyed on the run, exactly like `useRunSteps` and for
 * the same reason: the previous run's artifacts are not rows this one is
 * missing.
 *
 * IT IS GENUINELY LIVE, which is why it is a collection rather than the
 * on-demand read the journal gets. `v1:library:artifact` carries broadcast
 * routing rules for all three verbs (component/node/routing.go), so a file
 * that appears while somebody is watching appears on the map. Its sibling
 * `v1:authoring:bundle` broadcasts too (the `v1:authoring:*` rules); the map
 * does not draw it yet -- see src/nexus/concepts.ts.
 *
 * `artifactsForRun` is `@unbounded` on purpose: the map draws the whole set
 * at once, and a truncated page would silently lose a file from the scene.
 * So there is no cursor here and `paged` is false.
 */
export function useRunArtifacts(runId: string): LiveCollectionHandle<Row> {
  const key = runId.trim() === "" ? null : `work:artifacts:${runId}`;
  return useLiveCollection<Row>(key, (connection) => ({
    concept: ARTIFACT_CONCEPT,
    inScope: (row) => sameRow(readArtifact(row).runId, runId),
    seed: async (_cursor, signal) => {
      const result = await connection.query.artifactsForRun({ runId }, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, ARTIFACT_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

// ---------------------------------------------------------------------------
// The journal
// ---------------------------------------------------------------------------

export interface Journal {
  modelCalls: ModelCallRow[];
  observations: ObservationRow[];
  state: "idle" | "loading" | "ready" | "error";
  /** The server's own sentence, verbatim. "" when the last read worked. */
  error: string;
  /** When this window last looked. The whole point of an on-demand read. */
  readAt: string;
  read: () => void;
  /** Only a complete first page may be used for per-step totals. */
  modelCallsComplete: boolean;
  hasOlder: boolean;
  hasNewer: boolean;
  older: () => void;
  newer: () => void;
}

type JournalCursor = { calls: string | null; observations: string | null };
const FIRST_PAGE: JournalCursor = { calls: "", observations: "" };
type JournalSnapshot = Omit<Journal, "read" | "modelCallsComplete" | "hasOlder" | "hasNewer" | "older" | "newer"> & {
  next: JournalCursor;
  pageKey: string;
};
const IDLE: JournalSnapshot = {
  modelCalls: [],
  observations: [],
  state: "idle",
  error: "",
  readAt: "",
  next: { calls: null, observations: null },
  pageKey: "",
};

/** Owner-scoped snapshots while this run page is visible. Journal rows do not
 * broadcast; bounded polling keeps the story current without a cluster-wide feed. */
export function useJournal(runId: string, active = true): Journal {
  const connection = useOsConnection();
  const [state, setState] = useState<JournalSnapshot>(IDLE);
  const [pages, setPages] = useState<{ runId: string; trail: JournalCursor[] }>({ runId, trail: [] });
  const trail = pages.runId === runId ? pages.trail : [];
  const cursor = trail.at(-1) ?? FIRST_PAGE;
  const pageKey = JSON.stringify([runId, cursor.calls, cursor.observations]);
  const hasNewer = trail.length > 0;
  const hasOlder = state.pageKey === pageKey && (state.next.calls !== null || state.next.observations !== null);
  const older = () => {
    if (state.state !== "ready" || !hasOlder) return;
    setPages({ runId, trail: [...trail, state.next] });
  };
  const newer = () => setPages({ runId, trail: trail.slice(0, -1) });
  const [nonce, setNonce] = useState(0);

  const read = useCallback(() => setNonce((n) => n + 1), []);

  // A different run is a different journal. Reset rather than carrying the
  // previous run's rows under a new heading -- which would be this window
  // attributing one run's model calls to another.
  useEffect(() => {
    setState(IDLE);
    setPages({ runId, trail: [] });
  }, [runId]);

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null || runId.trim() === "") {
      setState({ ...IDLE, state: "error", error: "Not connected to the cluster." });
      return;
    }
    const controller = new AbortController();
    const signal = controller.signal;
    let timer: ReturnType<typeof setTimeout> | undefined;
    setState((prev) => prev.pageKey === pageKey
      ? { ...prev, state: prev.readAt ? prev.state : "loading", error: "" }
      : { ...IDLE, state: "loading", pageKey });

    void (async () => {
      try {
        const [calls, observations] = await Promise.all([
          cursor.calls === null ? null : query.workModelCallsPageForOwnerRun({ runId }, { signal, ...(cursor.calls ? { cursor: cursor.calls } : {}) }),
          cursor.observations === null ? null : query.workObservationSummariesForOwnerRun({ runId }, { signal, ...(cursor.observations ? { cursor: cursor.observations } : {}) }),
        ]);
        if (signal.aborted) return;
        setState({
          modelCalls: calls?.rows().map(modelCallFromRow) ?? [],
          observations: observations?.rows().map(observationFromRow) ?? [],
          next: { calls: calls?.meta()?.cursor || null, observations: observations?.meta()?.cursor || null },
          pageKey,
          state: "ready",
          error: "",
          readAt: new Date().toISOString(),
        });
      } catch (err: unknown) {
        if (signal.aborted) return;
        setState(prev => ({
          ...prev,
          state: "error",
          // VERBATIM. A refusal here is the server's own sentence and is the
          // most useful thing this panel can carry.
          error: err instanceof Error ? err.message : String(err),
          readAt: prev.readAt,
        }));
      } finally {
        if (!signal.aborted && active && !hasNewer) timer = setTimeout(read, 4000);
      }
    })();

    return () => { controller.abort(); if (timer) clearTimeout(timer); };
  }, [connection, runId, nonce, active, read, pageKey, cursor.calls, cursor.observations, hasNewer]);

  const modelCallsComplete = state.pageKey === pageKey && state.state === "ready" && !hasNewer && state.next.calls === null;
  return { ...state, read, modelCallsComplete, hasOlder, hasNewer, older, newer };
}
