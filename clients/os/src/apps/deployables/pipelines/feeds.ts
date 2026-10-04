import { useMemo } from "react";
import { getRowByConceptAndId, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useSessionIfPresent } from "../../../chrome/access";
import { flatten } from "../../../kit/rows";
import { useLiveCollection, type LiveCollectionHandle } from "../../../live/useLiveCollection";
import { LIBRARY_ARTIFACT_CONCEPT, PIPELINE_CONCEPT, RUN_CONCEPT, WORK_STEP_CONCEPT, runFileFromRow, sameId, stepFromRow, type RunFileRow, type StepRow } from "./rows";

// The pipelines feeds (epic memql#5479).
//
// TWO ARE RETAINED AT THE APP ROOT -- the caller's pipelines and their runs --
// for the reason every feed in this app is: the Runs tab, a source page's
// Checks rows, its Pipeline fact and the Overview map's Checks node are
// readings of ONE collection per concept, and two subscriptions over one
// concept would be free to disagree about what the cluster holds. Both
// broadcast created AND updated (component/node/routing.go), so a run that
// finishes on an agent replica nobody in this browser is talking to lands on
// the row somebody is looking at.
//
// TWO ARE THE RUN PAGE'S, retained while it is open: the run's work steps and
// the Library files it produced. They are scoped to ONE work run, so they are
// keyed by it -- a key must encode everything that changes what is read.
//
// EVERY FEED RE-APPLIES ITS READ'S SCOPE TO EVENTS (`inScope`). A subscription
// is scoped by concept: a cluster owner's carries every owner's runs, and a
// person running two pipelines at once receives both runs' steps on each
// run's step feed. The person-facing reads are owner-scoped and run-scoped;
// folding an event the read would not have returned makes a row appear that a
// refresh then removes.

/** How many pages of runs the live window walks: 200 runs, newest first. Older pages load on request. */
export const RUNS_LIVE_PAGES = 4;

export interface RunsFeed extends LiveCollectionHandle<Row> {
  /** The cursor after the live window's last page, "" when the window holds every run. */
  olderCursor: () => string;
}

function useViewer(): { viewer: string; ready: boolean } {
  const session = useSessionIfPresent();
  const viewer = session?.access?.userId ?? "";
  return { viewer, ready: session === null || viewer !== "" };
}

/** The caller's pipeline runs, newest first, live. */
export function usePipelineRuns(enabled = true): RunsFeed {
  const { viewer, ready } = useViewer();
  const held = useMemo(() => ({ cursor: "" }), [viewer]);
  const handle = useLiveCollection<Row>(enabled && ready ? `deployables:pipelineRuns:${viewer}` : null, (connection) => ({
    concept: RUN_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.pipelineRunsForOwner({}, { signal, ...(cursor !== "" ? { cursor } : {}) });
      const next = result.meta()?.cursor ?? "";
      held.cursor = next;
      return { rows: result.rows(), nextCursor: next };
    },
    inScope: (row) => viewer === "" || sameId(rowString(flatten(row), "ownerUserId"), viewer),
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, RUN_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    maxPages: RUNS_LIVE_PAGES,
  }));
  return { ...handle, olderCursor: () => held.cursor };
}

/** The caller's pipelines -- one per source they connected -- live. */
export function usePipelines(enabled = true): LiveCollectionHandle<Row> {
  const { viewer, ready } = useViewer();
  return useLiveCollection<Row>(enabled && ready ? `deployables:pipelines:${viewer}` : null, (connection) => ({
    concept: PIPELINE_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.pipelinesForOwner({}, { signal, ...(cursor !== "" ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    inScope: (row) => viewer === "" || sameId(rowString(flatten(row), "ownerUserId"), viewer),
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, PIPELINE_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
  }));
}

export interface RunStepsFeed {
  steps: StepRow[];
  state: LiveCollectionHandle<Row>["snapshot"]["state"];
  error: string;
  reseed: () => void;
}

/**
 * One work run's steps, live. "" reads nothing: a run refused before any work
 * has no work run, and an empty answer is the honest one.
 */
export function useRunSteps(workRunId: string): RunStepsFeed {
  const { snapshot, reseed } = useLiveCollection<Row>(workRunId === "" ? null : `deployables:runSteps:${workRunId}`, (connection) => ({
    concept: WORK_STEP_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.workStepsForOwnerRun({ runId: workRunId }, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    inScope: (row) => sameId(rowString(flatten(row), "runId"), workRunId),
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, WORK_STEP_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
  const steps = useMemo(() => snapshot.rows.map(stepFromRow).filter((s) => s.key !== ""), [snapshot.rows]);
  return { steps, state: snapshot.state, error: snapshot.error, reseed };
}

export interface RunFilesFeed {
  /** By the backing file's short id: what a step's logFileId and artifactFileIds name. */
  byFileId: ReadonlyMap<string, RunFileRow>;
  state: LiveCollectionHandle<Row>["snapshot"]["state"];
}

/**
 * The Library files one work run produced -- each step's full log and its
 * artifacts, which the runner stores under the pipeline owner and the Library
 * indexes on creation. Read through the index (`artifactsForRun`), because the
 * index row is what Files opens and what the content route serves.
 */
export function useRunFiles(workRunId: string): RunFilesFeed {
  const { snapshot } = useLiveCollection<Row>(workRunId === "" ? null : `deployables:runFiles:${workRunId}`, (connection) => ({
    concept: LIBRARY_ARTIFACT_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.artifactsForRun({ runId: workRunId }, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    inScope: (row) => sameId(rowString(flatten(row), "producedByRunId"), workRunId),
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, LIBRARY_ARTIFACT_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    paged: false,
  }));
  const byFileId = useMemo(() => {
    const out = new Map<string, RunFileRow>();
    for (const raw of snapshot.rows) {
      const file = runFileFromRow(raw);
      if (file.fileId !== "" && !file.archived) out.set(file.fileId, file);
    }
    return out;
  }, [snapshot.rows]);
  return { byFileId, state: snapshot.state };
}
