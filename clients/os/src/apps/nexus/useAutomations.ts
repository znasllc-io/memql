import { useCallback, useMemo, useState } from "react";
import { getRowByConceptAndId, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../kit/rows";
import { useOsConnection } from "../../live/connection";
import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { CONSTRUCT_CONCEPT, LADDER_POLICY_CONCEPT } from "./concepts";
import {
  ladderPolicyFromRow,
  procedureFromRow,
  type LadderPolicy,
  type ProcedureRow,
} from "./ladder";
import { idTail } from "./rows";

// The automations catalog: three LIVE feeds over the authoring domain.
//
// ===========================================================================
// THE ROUTING RULE WAS CHECKED, AND IT SAYS LIVE
// ===========================================================================
// `component/node/routing.go` forwards graph.node.{created,updated,deleted} for
// `v1:authoring:*` to every node (memql#4542). That covers the construct -- the
// authored catalog and the learned procedures are both rows of it -- and the
// ladder's policy singleton. A first cut of this surface read the absence of a
// rule NAMING the construct as "dark" and printed a not-live caption beside a
// "Look again" button; the wildcard was there all along. That is the mistake
// the OS README's rule exists for: read the PATTERNS, not the names.
//
// It matters more here than anywhere else in the app: a learned procedure's
// ladder moves on its own -- shadow comparisons and replays write it on agent
// nodes -- so a page that dated itself would be showing a rung the procedure
// had already left.
//
// Row admission gates the subscription exactly as it gates the read
// (memql#4309): `v1:authoring:construct` is owner-tier, so a person receives
// their own rows and no one else's. The policy row is readable from the reader
// rung up.
//
// ===========================================================================
// THREE FEEDS, THREE ANSWERS -- AND EACH MIRRORS ITS OWN READ
// ===========================================================================
// The authored catalog and the learned procedures are two READS of one
// concept, and a subscription is scoped by concept alone. So each collection
// re-applies its read's own narrowing to what it folds (`inScope`): the
// catalog takes catalogued rows, the procedures take the `procedure` target
// namespace. Without that, every write to any of this person's constructs
// would land in both lists. The two populations do not overlap -- the lift
// never catalogues what it learns -- and the list still says a procedure once
// if that ever changes (see AutomationsSection).
//
// Each settles on its own. A refusal on one is that feed's own sentence, shown
// beside what did arrive: a cluster whose learned-procedure read fails still
// lists every authored automation, and a policy nobody published is an
// ABSENCE the procedure page says out loud, never a reason to hide anything.
//
// ===========================================================================
// ONE PAGE, AND IT SAYS SO WHEN THERE MAY BE MORE
// ===========================================================================
// `cataloguedConstructsForOwner` carries `paginate 50` and
// `learnedProceduresForOwner` `paginate 100`, and neither generated method
// takes a cursor, so each seed is one page. A list holding at least that many
// is reported as possibly partial, never as the whole: a count presented as a
// total when it is a page is the same class of lie as a spend figure presented
// as measured when it was absent.
export const CATALOG_PAGE_BOUND = 50;
export const PROCEDURE_PAGE_BOUND = 100;

export interface AutomationFeeds {
  /** Every catalogued construct, of every kind; the list narrows to automations. */
  catalog: LiveCollectionHandle<Row>;
  /** The learned procedures, every rung, retired included. */
  procedures: LiveCollectionHandle<Row>;
  /** The ladder's policy singleton: at most one row. */
  policy: LiveCollectionHandle<Row>;
  /** The learned procedures, projected, for the surfaces that read them as rows. */
  procedureRows: ProcedureRow[];
  /** The ladder's values, or null when the cluster published no row. */
  ladderPolicy: LadderPolicy | null;
  /**
   * Whether `ladderPolicy` is an ANSWER. Null before the policy feed settles,
   * or after its read was refused, is not "the cluster published none" --
   * and the procedure page must not say that it is.
   */
  policyKnown: boolean;
}

/** The catalog read's own narrowing: `row.catalogued == true`. */
export function isCatalogued(row: Row): boolean {
  return flatten(row)["catalogued"] === true;
}

/** The learned-procedure read's own narrowing: `row.targetNamespace == "procedure"`. */
export function isLearnedProcedure(row: Row): boolean {
  return rowString(flatten(row), "targetNamespace") === "procedure";
}

/** The policy read's own narrowing: the one row at the literal id. */
function isPrimaryPolicy(row: Row): boolean {
  return idTail(rowString(flatten(row), "id")) === "primary";
}

/**
 * The catalog's three feeds, retained while `active`.
 *
 * HELD BY THE APP ROOT, NOT BY A SECTION, because two sections read them: the
 * Automations list, and an approval card that names the procedure it would
 * promote -- whose parameters are only called by their goal input keys once
 * the procedure itself is in hand. `active` is the app's answer to "does any
 * visible surface need this": the collections exist while it is true and are
 * released when it turns false, so a window on Goals subscribes to none of it.
 */
export function useAutomationFeeds(active = true): AutomationFeeds {
  const catalog = useLiveCollection<Row>(active ? "authoring:catalog" : null, (connection) => ({
    concept: CONSTRUCT_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.cataloguedConstructsForOwner({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, CONSTRUCT_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    inScope: isCatalogued,
    paged: false,
  }));
  const procedures = useLiveCollection<Row>(active ? "authoring:procedures" : null, (connection) => ({
    concept: CONSTRUCT_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.learnedProceduresForOwner({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, CONSTRUCT_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    inScope: isLearnedProcedure,
    paged: false,
  }));
  const policy = useLadderPolicyFeed(active);

  const procedureRows = useMemo(
    () => procedures.snapshot.rows.map(procedureFromRow).filter((p) => p.id !== ""),
    [procedures.snapshot],
  );
  const ladderPolicy = useMemo(
    () => ladderPolicyFromRow(policy.snapshot.rows[0] ?? null),
    [policy.snapshot],
  );

  return {
    catalog,
    procedures,
    policy,
    procedureRows,
    ladderPolicy,
    policyKnown: feedAnswered(policy.snapshot),
  };
}

/**
 * The ladder's policy singleton, live.
 *
 * Settings reads it too, and follows it for the same reason: the seed writes
 * the row again on every boot, so a deploy that changes a value changes it on
 * every open window without anybody asking.
 */
export function useLadderPolicyFeed(active = true): LiveCollectionHandle<Row> {
  return useLiveCollection<Row>(active ? "authoring:ladderPolicy" : null, (connection) => ({
    concept: LADDER_POLICY_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.ladderPolicyCurrent({}, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    reread: async (rowId, signal) => {
      const row = await getRowByConceptAndId(connection.query, LADDER_POLICY_CONCEPT, rowId, { signal });
      return (row as Row) ?? null;
    },
    inScope: isPrimaryPolicy,
    paged: false,
  }));
}

/**
 * Whether a feed's rows are an answer: it has settled, and what it holds is
 * either what its read returned or the last rows it had before degrading. A
 * seed that was refused with nothing to show is not an answer, and an empty
 * list from it is not "none".
 */
export function feedAnswered(snapshot: { state: string; error: string; rows: readonly unknown[] }): boolean {
  if (snapshot.state !== "live" && snapshot.state !== "degraded") return false;
  return snapshot.error === "" || snapshot.rows.length > 0;
}

export interface SetAutomationStatusState {
  busy: string;
  error: string;
  set: (constructId: string, status: "active" | "retired") => Promise<boolean>;
  reset: () => void;
}

/**
 * Arm or retire one catalogued automation.
 *
 * NOTHING NEW IS INVENTED FOR THIS. `setConstructStatus` is the authoring
 * catalog's own verb and the gate the engine already runs is the gate: the
 * mutation writes `ownerUserId: actor.userId`, so a person can only move their
 * own catalog, and `v1:authoring:construct` decides the rest.
 *
 * NEVER OFFERED ON A LEARNED PROCEDURE. A procedure is served through the
 * ladder and nothing else -- `procedureStep` refuses outside a replay -- so
 * arming one would register a construct no goal can run, and retiring one is
 * the ladder's own sweep.
 *
 * THE BUSY FLAG IS PER CONSTRUCT, not per hook, for the reason the approvals
 * queue's is: this is issued from a list, and a shared boolean would grey out
 * every row because somebody retired one of them.
 */
export function useSetAutomationStatus(): SetAutomationStatusState {
  const connection = useOsConnection();
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");

  const set = useCallback(
    async (constructId: string, status: "active" | "retired"): Promise<boolean> => {
      const query = connection?.query ?? null;
      if (query === null) {
        setError("Not connected to the cluster, so nothing was written.");
        return false;
      }
      setBusy(constructId);
      setError("");
      try {
        await query.setConstructStatus({ constructId, status });
        return true;
      } catch (err: unknown) {
        setError(err instanceof Error ? err.message : String(err));
        return false;
      } finally {
        setBusy("");
      }
    },
    [connection],
  );

  return { busy, error, set, reset: () => setError("") };
}
