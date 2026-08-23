import { useEffect, useMemo, useState } from "react";
import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useCluster } from "../cluster/ClusterProvider";

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
  const { query } = useCluster();
  const [rows, setRows] = useState<Row[]>([]);
  // True from mount for the reason every other read surface in this portal
  // starts true: a read is effectively in flight, and `false` would claim
  // "you have no goals" before anything had been asked.
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    if (query === null) return;
    let live = true;
    setLoading(true);
    setError("");
    void query
      .plansForUser({})
      .then((result) => {
        if (live) setRows(result.rows());
      })
      .catch((err: unknown) => {
        if (live) setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (live) setLoading(false);
      });
    return () => {
      live = false;
    };
  }, [query, epoch]);

  const goals = useMemo(() => {
    const list = rows.map(toGoal).filter((goal) => goal.id !== "");
    // Running first, then newest first within each group. A total order, so
    // the picker does not reshuffle between renders.
    list.sort((a, b) => {
      if (a.running !== b.running) return a.running ? -1 : 1;
      if (a.createdAt !== b.createdAt) return a.createdAt < b.createdAt ? 1 : -1;
      return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
    });
    return list;
  }, [rows]);

  return {
    goals,
    loading,
    error,
    mostRecentId: goals[0]?.id ?? "",
    reload: () => setEpoch((n) => n + 1),
  };
}
