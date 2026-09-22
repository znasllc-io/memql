import { useCallback, useEffect, useState } from "react";

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { flatten } from "../../kit/rows";
import { absent, figureFrom, type Figure } from "../../kit/measure";

// Decision records, as facts (epic memql#5153, D2).
//
// ===========================================================================
// WHAT A DECISION LIST IS FOR
// ===========================================================================
// A rule set nobody can read the consequences of is a set of assertions. This
// is the list that makes a rule falsifiable: what was asked for, what served,
// which rule decided, and what it cost. It is also how somebody learns the
// three nouns without reading anything -- forty rows of "level, rule, door"
// teach the vocabulary better than a paragraph.
//
// ===========================================================================
// `considered` IS THE HALF THAT MAKES IT EVIDENCE
// ===========================================================================
// The engine keeps the door report on SUCCESS as well as on a refusal: every
// entry the walk passed over, its door, and why. Without it a row says where
// a call went and cannot say whether it had anywhere else to go -- and those
// are the two different situations an operator is trying to tell apart when
// they ask "why did this go to a vendor". A policy with no alternative and a
// policy whose alternatives were all shut look identical from the outcome.

export interface ConsideredEntry {
  /** The chain entry as the policy names it. */
  entry: string;
  door: string;
  /** Why the walk passed over it. Empty on the one that served. */
  why: string;
  served: boolean;
}

export interface DecisionRow {
  id: string;
  createdAt: string;
  requestId: string;
  /**
   * What kind of caller made the call: user | system | connector | anonymous |
   * unattributed (memql#5581). It is what makes a row with no person an ANSWER
   * -- the cluster's own sweep -- rather than a gap in attribution.
   */
  callerKind: string;
  /**
   * Which cache answered, when one did: "exact" | "semantic". EMPTY MEANS A
   * PROVIDER ANSWERED, which is what every row written before memql#5581 was.
   */
  cacheKind: string;
  promptName: string;
  level: string;
  requestedLevel: string;
  servedLevel: string;
  degraded: boolean;
  rule: string;
  policy: string;
  door: string;
  providerName: string;
  vendor: string;
  model: string;
  considered: ConsideredEntry[];
  touches: string[];
  totalCost: Figure;
  totalDurationMs: Figure;
  outcome: string;
  billing: string;
  executionSurface: string;
}

function str(row: Record<string, unknown>, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}

function consideredFrom(raw: unknown): ConsideredEntry[] {
  if (!Array.isArray(raw)) return [];
  return raw.flatMap((one): ConsideredEntry[] => {
    if (one === null || typeof one !== "object") return [];
    const o = one as Record<string, unknown>;
    return [
      {
        entry: str(o, "entry"),
        door: str(o, "door"),
        why: str(o, "why") || str(o, "reason"),
        served: o["served"] === true,
      },
    ];
  });
}

export function decisionFromRow(raw: Row): DecisionRow {
  const r = flatten(raw as Record<string, unknown>);
  const touches = r["touches"];
  return {
    id: str(r, "id"),
    createdAt: str(r, "createdAt"),
    requestId: str(r, "requestId"),
    callerKind: str(r, "callerKind"),
    cacheKind: str(r, "cacheKind"),
    promptName: str(r, "promptName"),
    level: str(r, "level"),
    requestedLevel: str(r, "requestedLevel"),
    servedLevel: str(r, "servedLevel"),
    degraded: r["degraded"] === true,
    rule: str(r, "rule"),
    policy: str(r, "policy"),
    door: str(r, "door"),
    providerName: str(r, "providerName"),
    vendor: str(r, "vendor"),
    model: str(r, "model"),
    considered: consideredFrom(r["considered"]),
    touches: Array.isArray(touches) ? touches.filter((t): t is string => typeof t === "string") : [],
    totalCost: figureFrom(r, "totalCost"),
    totalDurationMs: figureFrom(r, "totalDurationMs"),
    outcome: str(r, "outcome"),
    billing: str(r, "billing"),
    executionSurface: str(r, "executionSurface"),
  };
}

/**
 * What a call actually cost the cluster, in words.
 *
 * THE POINT OF THE WHOLE PROGRAM IS LEGIBLE HERE OR NOWHERE. A call served by
 * a machine the person owns, or by a subscription they already pay for, cost
 * this cluster nothing -- and rendering `$0.00` for it would say "we measured
 * a cost and it was zero", which is a different claim and the wrong one. It
 * would also make a free call and a mis-priced metered call identical.
 *
 * `unknown` is its own answer and is never rounded to either side: it means
 * nobody reported, which happens when an app's usage report or a machine's
 * subscription signal is silent.
 */
export function billingWords(row: DecisionRow): string {
  // A CACHE HIT IS CHECKED FIRST, because the row's `billing` is the concept's
  // default of "metered" and that is not wrong -- it says which bucket the
  // call WOULD have fallen into. Nothing was sent, so nothing was billed, and
  // printing "$0.0000" for it would claim a metered call that cost nothing
  // rather than a call that never happened (memql#5581).
  if (row.cacheKind !== "") return "answered from the cache, nothing was sent";
  if (row.billing === "local") return "your own machine, no charge";
  if (row.billing === "subscription") return "your subscription, no charge here";
  if (row.billing === "unknown") return "not reported";
  return "";
}

/**
 * The same fact, sized for a column.
 *
 * SAY IT ONCE (DESIGN.md rule 7). The door column already says "your
 * machines" or "a signed-in app", so a cost column repeating "your own
 * machine" spends its width restating the column beside it -- and then
 * truncates, so it says "your own machi..." and restates it badly. What the
 * cost column is for is the money, and for a free call the whole answer is
 * that there is none. The long form stays for the hover and the accessible
 * name, where there is room for it.
 */
export function billingShort(row: DecisionRow): string {
  if (row.cacheKind !== "") return "from cache";
  if (row.billing === "local" || row.billing === "subscription") return "no charge";
  if (row.billing === "unknown") return "not reported";
  return "";
}

/** Whether this row's cost is a number worth printing. */
export function costIsMoney(row: DecisionRow): boolean {
  return row.billing === "metered" && row.cacheKind === "";
}

/**
 * The level line: what was asked for and what served.
 *
 * A row that was NOT degraded says one level, because saying "strong -> strong"
 * twice on every row is a column of noise that hides the handful of rows where
 * the two differ -- and those are the only rows this field exists for.
 */
export function levelWords(row: DecisionRow): string {
  const asked = row.requestedLevel || row.level;
  const served = row.servedLevel || row.level;
  if (asked === "" && served === "") return "";
  if (!row.degraded || asked === served) return served || asked;
  return `${asked} served as ${served}`;
}

/**
 * How the walk read.
 *
 * A chain with ONE entry had no alternative; a chain where every earlier entry
 * was shut is a different story with the same outcome, and the sentence has to
 * separate them or the list cannot answer the question it exists for.
 */
export function consideredSentence(row: DecisionRow): string {
  if (row.considered.length === 0) return "The cluster did not record what else it looked at.";
  if (row.considered.length === 1) {
    return "There was nothing else in the chain, so this was the only place to look.";
  }
  const passed = row.considered.filter((c) => !c.served).length;
  return `${passed} earlier ${passed === 1 ? "door was" : "doors were"} looked at first.`;
}

export interface DecisionFilters {
  since: string;
  level: string;
  door: string;
  rule: string;
  outcome: string;
}

export const NO_FILTERS: DecisionFilters = {
  since: "",
  level: "",
  door: "",
  rule: "",
  outcome: "",
};

export interface DecisionsState {
  rows: DecisionRow[];
  loading: boolean;
  /** The cluster has the read at all. */
  supported: boolean;
  error: string;
  fetchedAt: number | null;
  reload: () => void;
}

/**
 * The recent decisions.
 *
 * FILTERED SERVER-SIDE, because the query takes exactly these arguments and is
 * keyset-paged at 200. Filtering a page of 200 in the browser would narrow the
 * page rather than the search, so a facet would appear to find nothing the
 * moment the cluster got busy -- the classic "it works on my quiet cluster"
 * defect.
 */
export function useDecisions(enabled: boolean, filters: DecisionFilters): DecisionsState {
  const connection = useOsConnection();
  const [rows, setRows] = useState<DecisionRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [supported, setSupported] = useState(true);
  const [error, setError] = useState("");
  const [fetchedAt, setFetchedAt] = useState<number | null>(null);
  const [epoch, setEpoch] = useState(0);
  const reload = useCallback(() => setEpoch((n) => n + 1), []);

  const { since, level, door, rule, outcome } = filters;

  useEffect(() => {
    if (!enabled || connection === null) return;
    const q = connection.query as unknown as Record<string, unknown>;
    const read = q["routerDecisionsRecent"];
    if (typeof read !== "function") {
      setSupported(false);
      setRows([]);
      return;
    }
    setSupported(true);
    const controller = new AbortController();
    let stale = false;
    setLoading(true);
    setError("");
    // ONLY THE ARGUMENTS THAT ARE SET. The query's `when(args.x)` guards drop
    // an absent argument and its connective as if never written; an empty
    // string is a value and would filter on "". Same distinction the rule
    // `when` object keeps, one layer down.
    const args: Record<string, unknown> = {};
    if (since !== "") args["since"] = since;
    if (level !== "") args["level"] = level;
    if (door !== "") args["door"] = door;
    if (rule !== "") args["rule"] = rule;
    if (outcome !== "") args["outcome"] = outcome;
    void (read as (a: Record<string, unknown>, o?: unknown) => Promise<{ rows: () => Iterable<Row> }>)(
      args,
      { signal: controller.signal },
    )
      .then((result) => {
        if (stale) return;
        setRows([...result.rows()].map(decisionFromRow));
        setFetchedAt(Date.now());
      })
      .catch((err: unknown) => {
        if (stale) return;
        setRows([]);
        setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => {
        if (!stale) setLoading(false);
      });
    return () => {
      stale = true;
      controller.abort();
    };
  }, [connection, enabled, epoch, since, level, door, rule, outcome]);

  return { rows, loading, supported, error, fetchedAt, reload };
}

/** A cost figure that is deliberately not a number for a free call. */
export function costFigure(row: DecisionRow): Figure {
  return costIsMoney(row) ? row.totalCost : absent("unmeasured", billingWords(row));
}
