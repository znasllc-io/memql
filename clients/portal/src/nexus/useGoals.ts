import { useMemo } from "react";
import { getRowByConceptAndId, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";
import { useLive } from "../cluster/useLive";
import { useMyAccess } from "../cluster/useMyAccess";

// The caller's goals, for the picker and the recent-goals strip.
//
// ONE READ: `plansForUser` (dsl/planner/queries.memql), owner-scoped by
// `requestedBy==actor.userId` server-side. Its own header says why it had to
// exist -- `activePlansForUser` and `waitingPlansForUser` between them cannot
// name a goal that FINISHED, which is most of what a person looks back at and
// the whole of what Replay is for; and `allPlans` carries no caller gate at
// all, so a picker built on it would put other people's goals in your list.
//
// The running ones are pinned to the top CLIENT-SIDE from the status already
// on each row. That is a presentation decision over rows the caller already
// holds, not a second read: MemQL has no ORDER BY over a computed rank, and
// asking twice to get an ordering is two chances for the two answers to
// disagree about which goals exist.

// The statuses that mean "this goal is going right now". Listed rather than
// inferred from a negation, because `queued` and `waitingForSlot` are also
// not-terminal and belong BELOW a running goal rather than above it.
const PLAN_CONCEPT = "v1:planner:plan";

const RUNNING = new Set(["running", "routing", "planning"]);
const OPEN = new Set([
  "running",
  "routing",
  "planning",
  "queued",
  "waitingForSlot",
  "paused",
  "awaitingFeedback",
  "needsAgent",
]);

export interface Goal {
  id: string;
  goal: string;
  status: string;
  createdAt: string;
  running: boolean;
  open: boolean;
}

export interface GoalsState {
  goals: Goal[];
  loading: boolean;
  error: string;
  // The goal `/nexus` opens: the running one if there is one, otherwise the
  // most recent. "" when the caller has no goals at all.
  mostRecentId: string;
  reload: () => void;
}

function toGoal(row: Row): Goal {
  const status = rowString(row, "status");
  return {
    id: rowString(row, "id"),
    goal: rowString(row, "goal"),
    status,
    createdAt: rowString(row, "createdAt"),
    running: RUNNING.has(status),
    open: OPEN.has(status),
  };
}

export function useGoals(): GoalsState {
  const { query, status } = useCluster();
  const { access } = useMyAccess();
  const userId = access?.userId ?? "";
  const connected = query !== null && status === "connected";

  // LIVE THROUGH THE STORE (memql#4539, carrying memql#4528). The picker and
  // the index list these, and a goal started from the console appearing only
  // after a reload is the kind of staleness that makes an operator doubt the
  // button worked. It used to re-run the owner-scoped read on EVERY plan event
  // in the cluster; the rows fold now.
  //
  // THE SCOPE RE-FILTER IS LOAD-BEARING HERE, not a formality.
  // `v1:planner:plan` declares no row-authz tier yet (memql#4366), so this
  // feed carries events for other people's plans. `plansForUser` is gated on
  // requestedBy==actor.userId server-side, and `inScope` says the same thing
  // about an arriving event -- without it, folding an event's payload would
  // put somebody else's goal straight into this list. That is exactly the
  // difference between a re-read trigger, which was safe by accident, and a
  // fold, which has to be safe on purpose.
  // The key carries the caller's id, so the collection is re-created when
  // access resolves -- but the READ is not gated on it. Waiting for the id
  // would hold the picker in "loading" for a caller whose access never
  // resolves at all (a PAT with no user row), and the seed does not need it:
  // plansForUser is owner-scoped server-side. `inScope` refusing every fold
  // until the id is known is what keeps the unresolved window safe.
  const live = useLive<Row>(
    connected ? `nexus:goals:${userId}` : null,
    () => ({
      concept: PLAN_CONCEPT,
      actions: ["created", "updated"],
      paged: false,
      seed: async (_cursor, signal) => {
        if (query === null) return { rows: [], nextCursor: "" };
        const result = await query.plansForUser({}, { signal });
        return { rows: result.rows(), nextCursor: "" };
      },
      reread: async (rowId, signal) => {
        if (query === null) return null;
        return getRowByConceptAndId(query, PLAN_CONCEPT, rowId, { signal });
      },
      inScope: (row) => userId !== "" && rowString(row, "requestedBy") === userId,
    }),
  );

  const goals = useMemo(() => {
    const list = live.rows.map(toGoal).filter((goal) => goal.id !== "");
    // Running first, then newest first within each group. A total order, so
    // the picker does not reshuffle between renders.
    list.sort((a, b) => {
      if (a.running !== b.running) return a.running ? -1 : 1;
      if (a.createdAt !== b.createdAt) return a.createdAt < b.createdAt ? 1 : -1;
      return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
    });
    return list;
  }, [live.rows]);

  return {
    goals,
    // True from mount, and true while there is no connection, for the reason
    // every other read surface in this portal starts true: `false` claims "you
    // have no goals", and the index page renders that claim as a full empty
    // state. Reporting it before a read has even been attempted put that
    // sentence on screen for one frame on every load.
    loading: !connected || live.state === "seeding",
    error: live.error,
    mostRecentId: goals[0]?.id ?? "",
    reload: live.reload,
  };
}
