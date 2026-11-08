import { useMemo } from "react";
import { filterRuns, NO_RUN_FILTER, type RunFilter } from "./runs";
import { getRowByConceptAndId, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useSessionIfPresent } from "../../../chrome/access";
import { flatten } from "../../../kit/rows";
import { useLiveCollection, usePagedCollection, type LiveCollectionHandle, type PagedCollectionHandle } from "../../../live/useLiveCollection";
import { LIBRARY_ARTIFACT_CONCEPT, PIPELINE_CONCEPT, RUN_CONCEPT, WORK_STEP_CONCEPT, runFileFromRow, runFromRow, sameId, stepFromRow, type RunFileRow, type StepRow } from "./rows";

// The pipelines feeds (epic memql#5479).
//
// SUMMARY WINDOWS ARE RETAINED BY VISIBLE CONSUMERS. Runs, a source's checks
// and the Changes directory share a session collection only when owner and
// filters agree. Selected runs load by id independently of the recent window.
// The SDK shares their concept subscription, so completion on another replica
// reaches every interested view without duplicate wire subscriptions.
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

export type RunsFeed = PagedCollectionHandle<Row>;

function useViewer(): { viewer: string; ready: boolean } {
  const session = useSessionIfPresent();
  const viewer = session?.access?.userId ?? "";
  return { viewer, ready: session === null || viewer !== "" };
}

/** Summary pages shared by every reader of the same owner and filter. */
export function usePipelineRuns(enabled = true, filter: RunFilter = NO_RUN_FILTER): RunsFeed {
  const { viewer, ready } = useViewer();
  const criteria = { ...filter, search: filter.search.trim() };
  return usePagedCollection<Row>(enabled && ready ? `deployables:pipelineRuns:${viewer}:${JSON.stringify(criteria)}` : null, connection => ({
    concept: RUN_CONCEPT,
    comparePageRows: (a, b) => {
      const left = runFromRow(a), right = runFromRow(b);
      return right.queuedAt.localeCompare(left.queuedAt) || right.id.localeCompare(left.id);
    },
    seed: async (cursor, signal) => {
      const result = await connection.query.pipelineRunsBrowse(criteria, { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    inScope: row => (viewer === "" || sameId(rowString(flatten(row), "ownerUserId"), viewer))
      && filterRuns([runFromRow(row)], criteria).length > 0,
    reread: async (runId, signal) => (await connection.query.pipelineRunForOwner({ runId }, { signal })).rows()[0] ?? null,
  }));
}

/** Deep links must not depend on how far someone has paged through history. */
export function usePipelineRun(runId: string): LiveCollectionHandle<Row> {
  const { viewer, ready } = useViewer();
  return useLiveCollection<Row>(runId && ready ? `deployables:pipelineRun:${viewer}:${runId}` : null, connection => ({
    concept: RUN_CONCEPT,
    eventInScope: id => sameId(id, runId),
    seed: async (_cursor, signal) => ({ rows: (await connection.query.pipelineRunForOwner({ runId }, { signal })).rows(), nextCursor: "" }),
    inScope: row => sameId(rowString(flatten(row), "id"), runId) && (viewer === "" || sameId(rowString(flatten(row), "ownerUserId"), viewer)),
    reread: async (id, signal) => sameId(id, runId) ? (await connection.query.pipelineRunForOwner({ runId }, { signal })).rows()[0] ?? null : null,
    paged: false,
  }));
}

/** The caller's pipelines -- one per source they connected -- live. */
export function usePipelines(enabled = true): LiveCollectionHandle<Row> {
  const { viewer, ready } = useViewer();
  return useLiveCollection<Row>(enabled && ready ? `deployables:pipelines:${viewer}` : null, (connection) => ({
    concept: PIPELINE_CONCEPT,
    seed: async (cursor, signal) => {
      const result = await connection.query.pipelineDirectory({}, { signal, ...(cursor !== "" ? { cursor } : {}) });
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

/** Owner-scoped channel configuration; retain and reapply owner scope to events. */
export function usePipelineChannels(enabled = true): LiveCollectionHandle<Row> {
  const { viewer, ready } = useViewer();
  return useLiveCollection<Row>(enabled && ready ? `deployables:pipelineChannels:${viewer}` : null, connection => ({
    concept: "v1:pipelines:channel",
    seed: async (cursor, signal) => {
      const result = await connection.query.pipelineChannelsForOwner({}, { signal, ...(cursor ? { cursor } : {}) });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
    inScope: row => viewer === "" || sameId(rowString(flatten(row), "ownerUserId"), viewer),
    reread: async (rowId, signal) => (await getRowByConceptAndId(connection.query, "v1:pipelines:channel", rowId, { signal })) as Row ?? null,
  }));
}
