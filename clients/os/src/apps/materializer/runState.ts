import {
  rowString,
  type LiveSnapshot,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";
import type { LiveListSource } from "../../live/LiveList";

const UNFINISHED = new Set(["draft", "composing", "rendering"]);

/** Only the run actually carrying an unfinished composition can explain its interruption. */
export function unfinishedCompositionRunIds(rows: readonly Row[]): string[] {
  return [
    ...new Set(
      rows
        .filter((row) => UNFINISHED.has(rowString(row, "status")))
        .map((row) => rowString(row, "runId"))
        .filter(Boolean),
    ),
  ].sort();
}

export function compositionWithRunState(
  composition: Row,
  run: Row | undefined,
): Row {
  if (!run || !UNFINISHED.has(rowString(composition, "status")))
    return composition;
  for (const [compositionKey, runKey] of [
    ["runId", "id"],
    ["ownerUserId", "ownerUserId"],
    ["goalId", "goalId"],
  ] as const) {
    const expected = rowString(composition, compositionKey);
    if (!expected || expected !== rowString(run, runKey)) return composition;
  }
  const runStatus = rowString(run, "status");
  if (!["failed", "cancelled", "abandoned"].includes(runStatus))
    return composition;
  const status = runStatus === "cancelled" ? "cancelled" : "failed";
  const failureReason =
    rowString(run, "errorMessage") ||
    (runStatus === "abandoned"
      ? "The cluster lost the node making this file. Open its goal in Nexus to resume."
      : runStatus === "cancelled"
        ? "The work run was stopped before the file was ready."
        : "The work run failed before the file was ready. Open its goal in Nexus for details.");
  // This is a view only. A resumed run reveals the unchanged composition's
  // progress again, and a ready composition always keeps its file receipt.
  const payload = composition["payload"];
  return {
    ...composition,
    status,
    failureReason,
    ...(payload && typeof payload === "object"
      ? { payload: { ...payload, status, failureReason } }
      : {}),
  } as Row;
}

/** Join two retained feeds without restarting the list or its arrival baseline on a heartbeat. */
export function compositionRunSource(
  compositions: LiveListSource<Row>,
  runs: LiveListSource<Row>,
): LiveListSource<Row> {
  let fromCompositions: LiveSnapshot<Row> | null = null;
  let fromRuns: LiveSnapshot<Row> | null = null;
  let cached: LiveSnapshot<Row> | null = null;
  return {
    subscribe(listener) {
      const releaseCompositions = compositions.subscribe(listener);
      const releaseRuns = runs.subscribe(listener);
      return () => {
        releaseCompositions();
        releaseRuns();
      };
    },
    get snapshot() {
      const c = compositions.snapshot;
      const r = runs.snapshot;
      if (c === fromCompositions && r === fromRuns && cached) return cached;
      fromCompositions = c;
      fromRuns = r;
      const byId = new Map(r.rows.map((run) => [rowString(run, "id"), run]));
      const states = [c.state, r.state];
      const state = states.includes("disconnected")
        ? "disconnected"
        : states.includes("degraded")
          ? "degraded"
          : states.includes("seeding")
            ? "seeding"
            : "live";
      cached = {
        rows: c.rows.map((row) =>
          compositionWithRunState(row, byId.get(rowString(row, "runId"))),
        ),
        state,
        error: [c.error, r.error].filter(Boolean).join("; "),
        version: c.version + r.version,
      };
      return cached;
    },
  };
}
