import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../kit/rows";
import { idTail } from "./rows";
import type { ReuseLabel } from "./words";

// How much of a person's automation library is reusable (epic memql#5414,
// design D24). Pure.
//
// ===========================================================================
// THE PERSON'S LABEL WINS, AND THE EVIDENCE IS STILL THERE
// ===========================================================================
// `reuse` is what the sweep's evidence says; `reuseOverride.label` is what the
// person said, and a non-empty one wins. An EMPTY override label is "follow
// the evidence" -- a person who handed the label back -- and is read as no
// override at all. A construct the sweep has not looked at yet carries
// neither, and that is its own answer: NOT YET LABELLED, never folded into
// "for one goal", which is a claim the evidence has to make.

const LABELS: readonly ReuseLabel[] = ["reusable", "goalSpecific", "accountSpecific"];

function labelOf(value: unknown): ReuseLabel | "" {
  return typeof value === "string" && (LABELS as readonly string[]).includes(value) ? (value as ReuseLabel) : "";
}

/** The label a person sees: their own when they gave one, else the evidence's, else "". */
export function effectiveReuse(row: Row): ReuseLabel | "" {
  const flat = flatten(row);
  const override = flat["reuseOverride"];
  if (override !== null && typeof override === "object" && !Array.isArray(override)) {
    const label = labelOf((override as Record<string, unknown>)["label"]);
    if (label !== "") return label;
  }
  return labelOf(flat["reuse"]);
}

export interface ReuseTally {
  /** Every automation counted, labelled or not. */
  total: number;
  reusable: number;
  goalSpecific: number;
  accountSpecific: number;
  unlabelled: number;
}

/**
 * The library, counted by effective label.
 *
 * AUTOMATIONS ONLY -- the authored catalog and the learned procedures are both
 * rows of kind `automation`, and a catalogued query or shape is not something
 * a goal is served by. RETIRED ones are left out: a retired construct is
 * never selected again, so it is not part of what can be reused. Each is
 * counted once, however many of the reads answered it.
 */
export function reuseTally(rows: readonly Row[]): ReuseTally {
  const seen = new Set<string>();
  const tally: ReuseTally = { total: 0, reusable: 0, goalSpecific: 0, accountSpecific: 0, unlabelled: 0 };
  for (const row of rows) {
    const flat = flatten(row);
    const id = idTail(rowString(flat, "id"));
    if (id === "" || seen.has(id)) continue;
    const kind = rowString(flat, "kind");
    if (kind !== "" && kind !== "automation") continue;
    if (rowString(flat, "status") === "retired") continue;
    seen.add(id);
    tally.total += 1;
    const label = effectiveReuse(flat);
    if (label === "") tally.unlabelled += 1;
    else tally[label] += 1;
  }
  return tally;
}

/** Whether anything in the library carries a label yet -- the ratio is absent until one does. */
export function anyLabelled(tally: ReuseTally): boolean {
  return tally.reusable + tally.goalSpecific + tally.accountSpecific > 0;
}
