import { useCallback, useEffect, useState } from "react";
import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { APPROVAL_CONCEPT, GOAL_CONCEPT, RUN_CONCEPT, STEP_CONCEPT } from "./concepts";
import { modelCallFromRow, observationFromRow, type ModelCallRow, type ObservationRow } from "./rows";

// The Nexus app's feeds, and the one read that is deliberately not a feed.
//
// ===========================================================================
// THREE FEEDS AT THE APP ROOT, ONE PER CONCEPT
// ===========================================================================
// Goals, runs and approvals are retained ONCE, at the app root, and passed
// down. `useLiveCollection` constructs a collection per COMPONENT -- it
// memoises on `[connection, key]` inside the hook and does not call the SDK's
// shared registry -- so a second `useRuns()` inside the run page would open a
// second subscription and run a second seed over the same concept, and the two
// would then be free to disagree about what the cluster holds. That is the
// Deployables map-and-list failure, and here it would be worse: the Goals
// surface decides whether a run is in flight from the run feed and the run
// page decides which acts to offer from the same fact.
//
// The rule is per CONCEPT, not per app, so three feeds is not three copies of
// one thing: a goal, a run and an approval describe different things and
// cannot disagree.
//
// ===========================================================================
// STEPS ARE THE FOURTH, AND THEY ARE PER-RUN AND NOT AT THE ROOT
// ===========================================================================
// This is the rule the Deployables app wrote down about deployment timelines:
// a per-run detail feed is retained BY THE PAGE and never by the root, because
// subscribing a window to every step of every run in the cluster to render one
// timeline is exactly what that rule forbids. A run can have hundreds of steps
// and a person can have hundreds of runs.
//
// So the steps feed is keyed on the open run and lives as long as the page
// does. `key` is null while no run is open, which is what `useLiveCollection`
// takes to mean "no collection at all" rather than "a collection over
// nothing".
//
// ===========================================================================
// THE JOURNAL IS NOT LIVE AND THE SURFACE SAYS SO
// ===========================================================================
// `v1:work:modelCall` and `v1:work:observation` carry no broadcast routing
// rule, deliberately, on volume grounds -- one row per model request and one
// per tool result (design record section D). A `useLiveCollection` over either
// would render "Loading from the cluster" and then a list that silently never
// moved, which is WORSE than a plain read: the caption would be claiming
// wiring that is not there. So the journal is an on-demand read that prints
// when it was taken and offers to look again -- the same call the Training app
// made for the knowledge side and Accounts made for its ledger.

/** Every goal this caller owns, newest first. */
export function useGoals(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("work:goals", (connection) => ({
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
 * SEEDED UNFILTERED, and the show-finished preference is a fold over rows
 * already here. Seeding filtered would make the toggle re-run the read and
 * re-baseline every arrival cue, so revealing rows the browser already had
 * would announce them as new -- the Accounts and Files apps both record this,
 * and `workRunsForOwner` takes no arguments anyway.
 */
export function useRuns(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("work:runs", (connection) => ({
    concept: RUN_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.workRunsForOwner({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, RUN_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
}

/**
 * The caller's PENDING approvals.
 *
 * The read carries `decision==""` in the DSL, so this feed is the inbox by
 * construction. A decided approval leaves it on its own update -- which is
 * exactly right: deciding one is the act that empties the queue, and watching
 * the row go is the confirmation.
 */
export function useApprovals(): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>("work:approvals", (connection) => ({
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
}

const IDLE: Omit<Journal, "read"> = {
  modelCalls: [],
  observations: [],
  state: "idle",
  error: "",
  readAt: "",
};

/**
 * One run's model calls and observations, read when asked.
 *
 * BOTH HALVES SETTLE TOGETHER, which is the opposite of the Accounts ledger's
 * band-by-band rule -- and the difference is what a refusal would mean. There,
 * one of four bands is genuinely gated and the other three must survive it.
 * Here both reads carry the same owner conjunct against the same run, so they
 * succeed together or refuse together; splitting them would offer a "look
 * again" that re-reads one half of one reading.
 *
 * IT DOES NOT READ ON OPEN. The journal is the expensive half of a run and
 * most visits to a run page are about the timeline, so it is read when the
 * person asks for it. That also means "read at" is never a lie about a read
 * this window did not take.
 */
export function useJournal(runId: string): Journal {
  const connection = useOsConnection();
  const [state, setState] = useState<Omit<Journal, "read">>(IDLE);
  const [nonce, setNonce] = useState(0);

  const read = useCallback(() => setNonce((n) => n + 1), []);

  // A different run is a different journal. Reset rather than carrying the
  // previous run's rows under a new heading -- which would be this window
  // attributing one run's model calls to another.
  useEffect(() => {
    setState(IDLE);
  }, [runId]);

  useEffect(() => {
    if (nonce === 0) return;
    const query = connection?.query ?? null;
    if (query === null || runId.trim() === "") {
      setState({ ...IDLE, state: "error", error: "Not connected to the cluster." });
      return;
    }
    const controller = new AbortController();
    const signal = controller.signal;
    setState((prev) => ({ ...prev, state: "loading", error: "" }));

    void (async () => {
      try {
        const [calls, observations] = await Promise.all([
          query.workModelCallsForOwnerRun({ runId }, { signal }),
          query.workObservationsForOwnerRun({ runId }, { signal }),
        ]);
        if (signal.aborted) return;
        setState({
          modelCalls: calls.rows().map(modelCallFromRow),
          observations: observations.rows().map(observationFromRow),
          state: "ready",
          error: "",
          readAt: new Date().toISOString(),
        });
      } catch (err: unknown) {
        if (signal.aborted) return;
        setState({
          modelCalls: [],
          observations: [],
          state: "error",
          // VERBATIM. A refusal here is the server's own sentence and is the
          // most useful thing this panel can carry.
          error: err instanceof Error ? err.message : String(err),
          readAt: new Date().toISOString(),
        });
      }
    })();

    return () => controller.abort();
  }, [connection, runId, nonce]);

  return { ...state, read };
}
