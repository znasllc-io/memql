import { useCallback, useMemo, useState } from "react";
import { getRowByConceptAndId, rowString, type LiveState, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../../kit/rows";
import { useOsConnection } from "../../../live/connection";
import { useSessionIfPresent } from "../../../chrome/access";
import { useLiveCollection } from "../../../live/useLiveCollection";
import { pullFromRow, pullIsLive, type ModelPull } from "./models";

// The per-machine model pulls: a LIVE feed and the act that starts one
// (epic memql#5103).
//
// ===========================================================================
// LIVE, UNLIKE EVERY OTHER READING ON THIS PANEL
// ===========================================================================
// The Models SECTION's two readings are deliberately not live -- `fleetModels`
// and `inferenceStatus` are virtual projections with no `graph.node.*` event
// behind them, so a collection over either would render "Loading from the
// cluster" and then a list that never moves.
//
// v1:worker:modelPull is the opposite: a real row, written on the agent replica
// driving the download and carrying a broadcast routing rule
// (component/node/routing.go) precisely so it reaches this browser. A pull
// takes minutes, the person who pressed the button is watching, and a
// re-read button would be asking them to poll by hand.
//
// So this app now has both kinds on one screen, which the Cluster app's rule
// warns about. The distinction is legible here rather than confusing because
// the two are about different things: the catalog answers "what can the fleet
// run" and is a snapshot with a timestamp, while a pull is an EVENT WITH A
// DURATION and says so by moving.

export const MODEL_PULL_CONCEPT = "v1:worker:modelPull";

export interface ModelPullsHandle {
  /** Newest first. Bounded by the query's own page size. */
  pulls: ModelPull[];
  /** The pull still running, if any. At most one is ever started per machine
   *  from this surface, and a second is refused below. */
  live: ModelPull | null;
  /** Whether the feed has settled. A group that rendered "no pulls" while the
   *  seed was still in flight would tell somebody their pull had vanished. */
  loading: boolean;
  feedError: string;
  state: LiveState;
  reseed: () => void;
  /** Start a pull. Resolves to "" on success, or the refusal to show. */
  start: (model: string) => Promise<string>;
  starting: boolean;
}

/**
 * A message a person can act on, from whatever the cluster refused with.
 *
 * The engine's refusals already carry a sentence written for a reader
 * (fleet_model_pull.go), so the work here is to SURFACE it rather than to
 * translate it -- a second vocabulary in the client is a second thing to keep
 * in step, and the one that drifts is always the copy nobody tests.
 */
function refusalFrom(err: unknown): string {
  const raw = err instanceof Error ? err.message : String(err);
  const trimmed = raw.trim();
  return trimmed === ""
    ? "The cluster refused the pull and said nothing about why."
    : trimmed;
}

export function useModelPulls(workerId: string): ModelPullsHandle {
  const connection = useOsConnection();
  const userId = useSessionIfPresent()?.access?.userId ?? "";
  const [starting, setStarting] = useState(false);

  // The KEY is the machine, because that is what changes the read. It must
  // not carry anything that merely arrives late -- a key change restarts the
  // collection from empty, which would blank a pull somebody is watching.
  const { snapshot, reseed } = useLiveCollection<Row>(
    !workerId || !userId ? null : `fleet:pulls:${userId}:${workerId}`,
    (conn) => ({
      concept: MODEL_PULL_CONCEPT,
      seed: async (_cursor, signal) => {
        const result = await conn.query.modelPullsForWorker({ workerId }, { signal });
        return { rows: result.rows(), nextCursor: "" };
      },
      reread: async (rowId, signal) => {
        const row = await getRowByConceptAndId(conn.query, MODEL_PULL_CONCEPT, rowId, { signal });
        return (row as Row) ?? null;
      },
      inScope: row => pullFromRow(row).workerId === workerId && rowString(flatten(row), "ownerUserId") === userId,
      paged: false,
    }),
  );

  const pulls = useMemo(() => {
    // FILTERED TO THIS MACHINE ON THE CLIENT AS WELL. The seed is scoped by
    // the query, but the subscription is by CONCEPT: a pull the same person
    // starts on a different machine arrives on this feed too, and would
    // otherwise appear under the machine they are looking at.
    const rows = snapshot.rows.map(pullFromRow).filter((p) => p.pullId !== "");
    return rows
      .filter((p) => p.workerId === workerId)
      .sort((a, b) => (a.requestedAt < b.requestedAt ? 1 : a.requestedAt > b.requestedAt ? -1 : 0));
  }, [snapshot, workerId]);

  const live = useMemo(() => pulls.find(pullIsLive) ?? null, [pulls]);

  const start = useCallback(
    async (model: string): Promise<string> => {
      const wanted = model.trim();
      if (connection === null) return "A pull can only be started over a live connection.";
      if (wanted === "") return "Name the model to pull.";
      setStarting(true);
      try {
        await connection.query.fleetModelPull({ registrationId: workerId, model: wanted });
        // NOTHING IS WRITTEN THROUGH LOCALLY. The row arrives on the feed
        // above, from the cluster, and showing an optimistic one here would
        // paint a pull that the engine may have refused a moment later --
        // on the one surface whose job is saying whether a pull is really
        // happening.
        return "";
      } catch (err: unknown) {
        return refusalFrom(err);
      } finally {
        setStarting(false);
      }
    },
    [connection, workerId],
  );

  return {
    pulls,
    live,
    loading: snapshot.state === "seeding",
    feedError: snapshot.error || (snapshot.state === "disconnected" ? "Download updates are interrupted. Showing the last known state." : ""),
    state: snapshot.state,
    reseed,
    start,
    starting,
  };
}
