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

export const REUSE_LABELS: readonly ReuseLabel[] = ["reusable", "goalSpecific", "accountSpecific"];

function labelOf(value: unknown): ReuseLabel | "" {
  return typeof value === "string" && (REUSE_LABELS as readonly string[]).includes(value) ? (value as ReuseLabel) : "";
}

function objectOf(v: unknown): Record<string, unknown> | null {
  if (typeof v === "string") {
    // An object field reaches a browser as an object; a writer that quoted it
    // spelled the same document once more.
    try {
      return objectOf(JSON.parse(v));
    } catch {
      return null;
    }
  }
  return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
}

function countOf(from: Record<string, unknown> | null, key: string): number | null {
  const v = from?.[key];
  return typeof v === "number" && Number.isFinite(v) && v >= 0 ? Math.round(v) : null;
}

function textOf(from: Record<string, unknown> | null, key: string): string {
  const v = from?.[key];
  return typeof v === "string" ? v : "";
}

/** What one construct's row says about its reuse: the evidence, and the person's label over it. */
export interface ReuseFacts {
  /** What the sweep's evidence says, or "" before it has looked. */
  evidence: ReuseLabel | "";
  /** The person's own label, or "" when they follow the evidence. */
  override: ReuseLabel | "";
  /** Every override written counts, a hand-back included; 0 when there never was one. */
  overrideVersion: number;
  overrideAt: string;
  /** Distinct goal shapes that used it -- null until the sweep has counted. */
  signatureCount: number | null;
  uses: number | null;
  /** Accounts its uses were tied to. */
  accountCount: number;
}

/** A construct row's reuse facts. Absent reads as absent, never as a zero. */
export function reuseFactsFromRow(row: Row): ReuseFacts {
  const flat = flatten(row);
  const evidence = objectOf(flat["reuseEvidence"]);
  const override = objectOf(flat["reuseOverride"]);
  const accounts = evidence?.["accountIds"];
  const signatures = evidence?.["goalSignatures"];
  return {
    evidence: labelOf(flat["reuse"]),
    override: labelOf(override?.["label"]),
    overrideVersion: countOf(override, "version") ?? 0,
    overrideAt: textOf(override, "at"),
    // The count the sweep wrote, else the signatures it kept -- at most twenty
    // are kept, so the count is the one to believe when both are there.
    signatureCount:
      countOf(evidence, "signatureCount") ??
      (Array.isArray(signatures) ? signatures.filter((s) => typeof s === "string" && s !== "").length : null),
    uses: countOf(evidence, "uses"),
    accountCount: Array.isArray(accounts) ? new Set(accounts.filter((a) => typeof a === "string" && a !== "")).size : 0,
  };
}

/** The label a person sees in its facts: their own when they gave one, else the evidence's. */
export function effectiveLabel(facts: ReuseFacts): ReuseLabel | "" {
  return facts.override !== "" ? facts.override : facts.evidence;
}

/** The label a person sees: their own when they gave one, else the evidence's, else "". */
export function effectiveReuse(row: Row): ReuseLabel | "" {
  return effectiveLabel(reuseFactsFromRow(row));
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
