import { approvalMatches, approvalSearchArgs } from "./approvalSearch";
import { useMemo } from "react";
import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";
import { useSession } from "../../chrome/access";
import { flatten } from "../../kit/rows";
import {
  useLiveCollection,
  usePagedCollection,
} from "../../live/useLiveCollection";
import { useMergedPagedCollections } from "../../live/useMergedPagedCollections";
import { APPROVAL_CONCEPT, GOAL_CONCEPT, RUN_CONCEPT } from "./concepts";
import { ROUTING_REVIEW, useRoutingReviews } from "./useNexus";
import {
  approvalFromRow,
  approvalIsPending,
  goalFromRow,
  goalMatches,
  idTail,
  runFromRow,
  runIsTerminal,
  runMatches,
  runTitle,
  sameRow,
} from "./rows";

function recent(a: Row, b: Row) {
  return (
    rowString(flatten(b), "createdAt").localeCompare(
      rowString(flatten(a), "createdAt"),
    ) || rowString(b, "id").localeCompare(rowString(a, "id"))
  );
}
function owned(row: Row, viewer: string) {
  return rowString(flatten(row), "ownerUserId") === viewer;
}

export function useGoalDirectory(
  search: string,
  includePipelines: boolean,
  ascending: boolean,
  enabled = true,
) {
  const viewer = useSession().access?.userId ?? "";
  const text = search.trim();
  return usePagedCollection<Row>(
    enabled && viewer
      ? `work:goals:browse:${JSON.stringify([viewer, text, includePipelines, ascending])}`
      : null,
    (connection) => ({
      concept: GOAL_CONCEPT,
      seed: async (cursor, signal) => {
        const method = ascending
          ? connection.query.workGoalsBrowseOldest.bind(connection.query)
          : connection.query.workGoalsBrowse.bind(connection.query);
        const result = await method(
          { search: text || undefined, includePipelines },
          { signal, ...(cursor ? { cursor } : {}) },
        );
        return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
      },
      reread: async (goalId, signal) =>
        (
          await connection.query.workGoalSummaryForOwner({ goalId }, { signal })
        ).rows()[0] ?? null,
      rereadEveryEvent: true,
      comparePageRows: (a, b) => (ascending ? -1 : 1) * recent(a, b),
      inScope: (row) => {
        const goal = goalFromRow(row);
        return (
          owned(row, viewer) &&
          goalMatches(goal, text) &&
          (includePipelines ||
            !["pipeline", "pipelines"].includes(goal.requestedVia))
        );
      },
    }),
  );
}

/** Complete, compact status marks only for the goals currently displayed. */
export function useGoalRunStatuses(goalIds: readonly string[]) {
  const viewer = useSession().access?.userId ?? "";
  const ids = [...new Set(goalIds)].sort();
  return useLiveCollection<Row>(
    viewer && ids.length
      ? `work:goal-statuses:${JSON.stringify([viewer, ids])}`
      : null,
    (connection) => ({
      concept: RUN_CONCEPT,
      seed: async (cursor, signal) => {
        const result = await connection.query.workGoalRunStatuses(
          { goalIds: ids },
          { signal, ...(cursor ? { cursor } : {}) },
        );
        return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
      },
      reread: async (runId, signal) =>
        (
          await connection.query.workRunSummaryForOwner({ runId }, { signal })
        ).rows()[0] ?? null,
      rereadEveryEvent: true,
      inScope: (row) =>
        owned(row, viewer) &&
        ids.some((id) => sameRow(id, runFromRow(row).goalId)),
    }),
  );
}

export function useRunDirectory(
  search: string,
  showFinished: boolean,
  ascending: boolean,
  conversationId = "",
  enabled = true,
) {
  const viewer = useSession().access?.userId ?? "";
  const text = search.trim();
  return usePagedCollection<Row>(
    enabled && viewer
      ? `work:runs:browse:${JSON.stringify([viewer, text, showFinished, ascending, conversationId])}`
      : null,
    (connection) => ({
      concept: RUN_CONCEPT,
      seed: async (cursor, signal) => {
        const method = ascending
          ? connection.query.workRunsBrowseOldest.bind(connection.query)
          : connection.query.workRunsBrowse.bind(connection.query);
        const result = await method(
          {
            search: text || undefined,
            showFinished,
            conversationId: conversationId || undefined,
          },
          { signal, ...(cursor ? { cursor } : {}) },
        );
        return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
      },
      reread: async (runId, signal) =>
        (
          await connection.query.workRunSummaryForOwner({ runId }, { signal })
        ).rows()[0] ?? null,
      rereadEveryEvent: true,
      comparePageRows: (a, b) => (ascending ? -1 : 1) * recent(a, b),
      inScope: (row) => {
        const run = runFromRow(row);
        const flat = flatten(row);
        const input = flat.input as
          | { conversation?: { id?: string } }
          | undefined;
        const conversation = flat.conversation as { id?: string } | undefined;
        return (
          owned(row, viewer) &&
          (showFinished || !runIsTerminal(run)) &&
          (!conversationId ||
            (conversation?.id ?? input?.conversation?.id) === conversationId) &&
          (runMatches(run, text) ||
            runTitle(run).toLowerCase().includes(text.toLowerCase()))
        );
      },
    }),
  );
}

/** Oldest first, the queue's order; the id settles a tie. */
function oldestRequest(a: Row, b: Row) {
  return rowString(flatten(a), "requestedAt").localeCompare(rowString(flatten(b), "requestedAt"))
    || rowString(a, "id").localeCompare(rowString(b, "id"));
}

/**
 * The approvals inbox: the person's own pending approvals, paged, merged with
 * the routing reviews they may decide.
 *
 * A ROUTING REVIEW IS NOBODY'S OWN, so the person's browse never holds one;
 * it comes from its own collection (useRoutingReviews) and is merged in the
 * queue's order, the way the users roster merges people and invitations. The
 * routing reviews are returned too, so the section can re-read them after a
 * decision a developer is sent no update for.
 */
export function useApprovalDirectory(search: string, enabled = true) {
  const viewer = useSession().access?.userId ?? "";
  const text = search.trim();
  const own = useOwnApprovalDirectory(viewer, text, enabled);
  const routingReviews = useRoutingReviews(enabled && viewer !== "");
  const merged = useMergedPagedCollections(
    `work:approvals:inbox:${JSON.stringify([viewer, text])}`,
    own,
    routingReviews,
    (ownRows, reviewRows) => [...ownRows, ...reviewRows.filter(row => approvalMatches(approvalFromRow(row), text))].sort(oldestRequest),
  );
  return {
    ...merged,
    snapshot: merged.source?.snapshot ?? own.snapshot,
    routingReviews,
  };
}

function useOwnApprovalDirectory(viewer: string, text: string, enabled: boolean) {
  return usePagedCollection<Row>(
    enabled && viewer
      ? `work:approvals:browse:${JSON.stringify([viewer, text])}`
      : null,
    (connection) => ({
      concept: APPROVAL_CONCEPT,
      seed: async (cursor, signal) => {
        const result = await connection.query.workApprovalsBrowse(
          approvalSearchArgs(text),
          { signal, ...(cursor ? { cursor } : {}) },
        );
        return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
      },
      reread: async (approvalId, signal) =>
        (
          await connection.query.workApprovalForOwner(
            { approvalId },
            { signal },
          )
        ).rows()[0] ?? null,
      rereadEveryEvent: true,
      comparePageRows: (a, b) =>
        rowString(flatten(a), "requestedAt").localeCompare(
          rowString(flatten(b), "requestedAt"),
        ) || rowString(a, "id").localeCompare(rowString(b, "id")),
      inScope: (row) =>
        owned(row, viewer) &&
        approvalIsPending(approvalFromRow(row)) &&
        approvalMatches(approvalFromRow(row), text),
    }),
  );
}

/** A selected identity is independent of every list's search and cursor. */
export function useWorkRecord(kind: "goal" | "run" | "approval", id: string) {
  const viewer = useSession().access?.userId ?? "";
  const concept =
    kind === "goal"
      ? GOAL_CONCEPT
      : kind === "run"
        ? RUN_CONCEPT
        : APPROVAL_CONCEPT;
  return useLiveCollection<Row>(
    viewer && id ? `work:record:${JSON.stringify([viewer, kind, id])}` : null,
    (connection) => {
      const read = async (rowId: string, signal?: AbortSignal) => {
        const result =
          kind === "goal"
            ? await connection.query.workGoalForOwner(
                { goalId: rowId },
                { signal },
              )
            : kind === "run"
              ? await connection.query.workRunSummaryForOwner(
                  { runId: rowId },
                  { signal },
                )
              : await connection.query.workApprovalForOwner(
                  { approvalId: rowId },
                  { signal },
                );
        // A routing review is nobody's own, so the person's own read never
        // has it: it is read through the routing reviews instead, which
        // answer an owner or developer whatever its decision.
        if (kind === "approval" && result.rows().length === 0) {
          return (await connection.query.workRoutingReviews({ approvalId: rowId }, { signal })).rows();
        }
        return result.rows();
      };
      return {
        concept,
        eventInScope: (rowId) => sameRow(rowId, id),
        seed: async (_cursor, signal) => ({
          rows: await read(id, signal),
          nextCursor: "",
        }),
        reread: async (rowId, signal) => (await read(rowId, signal))[0] ?? null,
        rereadEveryEvent: true,
        inScope: (row) =>
          (owned(row, viewer) || (kind === "approval" && approvalFromRow(row).kind === ROUTING_REVIEW))
          && sameRow(rowString(row, "id"), id),
        paged: false,
      };
    },
  );
}

export function useGoalRuns(goalId: string) {
  const viewer = useSession().access?.userId ?? "";
  return useLiveCollection<Row>(
    viewer && goalId
      ? `work:goal-runs:${JSON.stringify([viewer, goalId])}`
      : null,
    (connection) => ({
      concept: RUN_CONCEPT,
      seed: async (cursor, signal) => {
        const result = await connection.query.workRunSummariesForGoal(
          { goalId },
          { signal, ...(cursor ? { cursor } : {}) },
        );
        return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
      },
      reread: async (runId, signal) =>
        (
          await connection.query.workRunSummaryForOwner({ runId }, { signal })
        ).rows()[0] ?? null,
      rereadEveryEvent: true,
      inScope: (row) =>
        owned(row, viewer) && sameRow(runFromRow(row).goalId, goalId),
    }),
  );
}

export function useRunApprovals(runIds: readonly string[]) {
  const viewer = useSession().access?.userId ?? "";
  const ids = useMemo(() => [...new Set(runIds)].sort(), [runIds.join("|")]);
  return useLiveCollection<Row>(
    viewer && ids.length
      ? `work:run-approvals:${JSON.stringify([viewer, ids])}`
      : null,
    (connection) => ({
      concept: APPROVAL_CONCEPT,
      seed: async (cursor, signal) => {
        const result = await connection.query.workApprovalsForRuns(
          { runIds: ids },
          { signal, ...(cursor ? { cursor } : {}) },
        );
        return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
      },
      reread: async (approvalId, signal) =>
        (
          await connection.query.workApprovalForOwner(
            { approvalId },
            { signal },
          )
        ).rows()[0] ?? null,
      rereadEveryEvent: true,
      inScope: (row) =>
        owned(row, viewer) &&
        ids.some((id) => idTail(id) === idTail(approvalFromRow(row).runId)),
    }),
  );
}

export function useRunRecords(runIds: readonly string[]) {
  const viewer = useSession().access?.userId ?? "";
  const ids = [...new Set(runIds)].sort();
  return useLiveCollection<Row>(
    viewer && ids.length
      ? `work:run-names:${JSON.stringify([viewer, ids])}`
      : null,
    (connection) => ({
      concept: RUN_CONCEPT,
      eventInScope: (id) => ids.some((wanted) => sameRow(wanted, id)),
      seed: async (cursor, signal) => {
        const result = await connection.query.workRunSummariesByIds(
          { runIds: ids },
          { signal, ...(cursor ? { cursor } : {}) },
        );
        return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
      },
      reread: async (runId, signal) =>
        (
          await connection.query.workRunSummaryForOwner({ runId }, { signal })
        ).rows()[0] ?? null,
      rereadEveryEvent: true,
      inScope: (row) =>
        owned(row, viewer) &&
        ids.some((id) => sameRow(rowString(row, "id"), id)),
    }),
  );
}
