import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";
import { useSession } from "../../chrome/access";
import { flatten } from "../../kit/rows";
import { useLiveView } from "../../live/liveView";
import { usePagedCollection } from "../../live/useLiveCollection";
import { CONSTRUCT_CONCEPT } from "./concepts";
import {
  automationFromRow,
  automationMatches,
  statusWord,
  type Origin,
} from "./automations";
import {
  ladderPolicyFromRow,
  ladderWord,
  procedureFromRow,
  procedureTitle,
} from "./ladder";
import { effectiveLabel, reuseFactsFromRow } from "./reuse";
import {
  isLearnedProcedure,
  useConstructRecord,
  useLadderPolicyFeed,
  feedAnswered,
  type AutomationFeeds,
} from "./useAutomations";
import type { ReuseFacet } from "./AutomationsSection";

export function useAutomationDirectory(
  search: string,
  origin: "" | Origin,
  reuse: ReuseFacet,
  selectedId: string,
  enabled = true,
) {
  const viewer = useSession().access?.userId ?? "";
  const text = search.trim().toLowerCase();
  const feed = usePagedCollection<Row>(
    enabled && viewer
      ? `authoring:browse:${JSON.stringify([viewer, text, origin, reuse])}`
      : null,
    (connection) => ({
      concept: CONSTRUCT_CONCEPT,
      seed: async (cursor, signal) => {
        const result = await connection.query.nexusAutomationsBrowse(
          {
            search: text || undefined,
            origin: origin || undefined,
            reuse: reuse || undefined,
            statusWords: text
              ? ["active", "staged", "retired", "draft"].filter((status) =>
                  statusWord(status).toLowerCase().includes(text),
                )
              : undefined,
            includeLearned: !!text && "learned".includes(text),
            offLadder: !!text && "off the ladder".includes(text),
          },
          { signal, ...(cursor ? { cursor } : {}) },
        );
        return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
      },
      reread: async (constructId, signal) =>
        (
          await connection.query.nexusAutomationSummaryById(
            { constructId },
            { signal },
          )
        ).rows()[0] ?? null,
      rereadEveryEvent: true,
      comparePageRows: (a, b) =>
        rowString(flatten(b), "createdAt").localeCompare(
          rowString(flatten(a), "createdAt"),
        ) || rowString(b, "id").localeCompare(rowString(a, "id")),
      inScope: (row) => {
        const flat = flatten(row);
        const learned = isLearnedProcedure(row);
        if (
          rowString(flat, "ownerUserId") !== viewer ||
          (!learned && (flat.catalogued !== true || flat.kind !== "automation"))
        )
          return false;
        if (
          (origin === "learned" && !learned) ||
          (origin === "authored" && learned)
        )
          return false;
        const label = effectiveLabel(reuseFactsFromRow(flat));
        if (reuse && label !== (reuse === "unlabelled" ? "" : reuse))
          return false;
        const procedure = procedureFromRow(row);
        return (
          !text ||
          (learned
            ? [
                procedureTitle(procedure),
                procedure.name,
                ladderWord(procedure.ladder),
                "learned",
              ].some((value) => value.toLowerCase().includes(text))
            : automationMatches(automationFromRow(row), text))
        );
      },
    }),
  );
  const catalog = useLiveView(feed.source, "authored", (rows) =>
    rows.filter((row) => !isLearnedProcedure(row)),
  );
  const procedures = useLiveView(feed.source, "learned", (rows) =>
    rows.filter(isLearnedProcedure),
  );
  const record = useConstructRecord(enabled ? selectedId : "");
  const policy = useLadderPolicyFeed(enabled);
  const full = record.snapshot.rows[0];
  const feeds: AutomationFeeds = {
    catalog: {
      source: catalog,
      snapshot: catalog?.snapshot ?? feed.snapshot,
      reseed: feed.reseed,
    },
    procedures: {
      source: procedures,
      snapshot: procedures?.snapshot ?? feed.snapshot,
      reseed: feed.reseed,
    },
    procedureRows: [
      ...(procedures?.snapshot.rows ?? []),
      ...(full && isLearnedProcedure(full) ? [full] : []),
    ].map(procedureFromRow),
    policy,
    ladderPolicy: ladderPolicyFromRow(policy.snapshot.rows[0] ?? null),
    policyKnown: feedAnswered(policy.snapshot),
  };
  return { feed, feeds, record };
}
