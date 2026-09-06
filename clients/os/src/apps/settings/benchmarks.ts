import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { rowString } from "@znasllc-io/memql-sdk-core/client";

import { stringsOf } from "../../kit";

/**
 * Reading the proving suite's published record (epic memql#4993).
 *
 * PURE. Everything here is a function over rows, asserted directly, for the
 * reason the Deployables traffic reading gives: what a reading MEANS -- an
 * absent figure against a measured zero, a metric that stopped being
 * published, a run that measured a different corpus -- is a claim about
 * functions, and asserting it through render() puts three layers that can each
 * fail for unrelated reasons between the claim and the check.
 *
 * THE ONE RULE THIS FILE EXISTS TO KEEP: a figure with no number and a figure
 * whose number is zero are DIFFERENT ANSWERS. The engine keeps them apart with
 * `absentReason`; this keeps them apart all the way to the pixel, and
 * `benchmarks.test.ts` asserts it in both directions.
 */

/** One published figure, or one published absence. */
export interface Figure {
  readonly metric: string;
  readonly family: string;
  readonly scenario: string;
  readonly arm: "platform" | "baseline";
  readonly unit: string;
  /** null when nothing was measured. Never 0 standing in for null. */
  readonly median: number | null;
  readonly n: number | null;
  readonly low: number | null;
  readonly high: number | null;
  /** "" when the figure was measured. */
  readonly absent: string;
  /** Names the missing code for `seamNotBuilt`. */
  readonly detail: string;
  readonly commit: string;
  readonly measuredOn: string;
}

/** One benchmark run. */
export interface Run {
  readonly id: string;
  readonly tier: string;
  readonly commit: string;
  readonly corpus: string;
  readonly scenarioCount: number;
  readonly verdict: string;
  readonly runner: string;
  readonly startedAt: string;
  /** What the run measured against. `synthetic` means the CI tape holds
   *  placeholder responses -- honest about structure, silent about anything
   *  model-dependent -- and a reader of this page has to be told. */
  readonly models: readonly string[];
}

/** A metric's history: one entry per run, oldest first. */
export interface Trend {
  readonly metric: string;
  readonly family: string;
  readonly unit: string;
  readonly platform: readonly (Figure | null)[];
  readonly baseline: readonly (Figure | null)[];
}

function num(row: Row, key: string): number | null {
  const raw = (row as Record<string, unknown>)[key];
  return typeof raw === "number" ? raw : null;
}

export function runFromRow(row: Row): Run {
  return {
    id: rowString(row, "id"),
    tier: rowString(row, "tier"),
    commit: rowString(row, "commit"),
    corpus: rowString(row, "corpusFingerprint"),
    scenarioCount: num(row, "scenarioCount") ?? 0,
    verdict: rowString(row, "verdict"),
    runner: rowString(row, "runner"),
    startedAt: rowString(row, "startedAt"),
    models: stringsOf(row, "modelIds"),
  };
}

export function figureFromRow(row: Row): Figure {
  const absent = rowString(row, "absentReason");
  const measured = absent === "";
  return {
    metric: rowString(row, "metric"),
    family: rowString(row, "family"),
    scenario: rowString(row, "scenarioId"),
    arm: rowString(row, "arm") === "baseline" ? "baseline" : "platform",
    unit: rowString(row, "unit"),
    // The load-bearing line. An unmeasured sample carries no median at all,
    // and reading one as 0 would collapse "nothing measured this" into "this
    // measured zero" -- which, for durability's headline, is the difference
    // between the claim and its opposite.
    median: measured ? num(row, "median") : null,
    n: measured ? num(row, "n") : null,
    low: measured ? num(row, "p10") : null,
    high: measured ? num(row, "p90") : null,
    absent,
    detail: rowString(row, "detail"),
    commit: rowString(row, "commit"),
    measuredOn: rowString(row, "measuredOn"),
  };
}

/**
 * Why a figure has no number, in words. The engine's set is closed, so this
 * mapping is total; an unrecognised value falls through to itself rather than
 * to "unknown", because a value the OS has not learned yet is still more
 * informative than a shrug.
 */
export function absenceSentence(reason: string, detail: string): string {
  const base = ((): string => {
    switch (reason) {
      case "notMeasurableOnReplay":
        return "Not measurable on a replay. A replay is deterministic, so only the live tier can answer this.";
      case "seamNotBuilt":
        return "The code that would produce this is not built yet.";
      case "tierNotRun":
        return "This tier has not been run.";
      case "noProvider":
        return "The run self-skipped: no provider credential was configured.";
      case "belowFloor":
        return "Too few samples to state a median.";
      case "ceilingReached":
        return "The spend ceiling stopped the run before this was complete.";
      default:
        return reason;
    }
  })();
  return detail === "" ? base : `${base} ${detail}`;
}

/**
 * One number, in its unit, for a person.
 *
 * Ratios become percentages because a reader takes 0.71 for a probability and
 * 71% for a fraction of something, and every ratio here is the latter.
 */
export function formatFigure(value: number, unit: string): string {
  switch (unit) {
    case "usd":
      return `$${value.toFixed(4)}`;
    case "ratio":
      return `${(value * 100).toFixed(1)}%`;
    case "percent":
      return `${value >= 0 ? "+" : ""}${value.toFixed(1)}%`;
    case "ms":
      return value >= 1000 ? `${(value / 1000).toFixed(2)}s` : `${Math.round(value)}ms`;
    default:
      return Number.isInteger(value) ? String(value) : value.toFixed(2);
  }
}

/** The spread, or "" when there is none worth printing. */
export function formatSpread(f: Figure): string {
  if (f.median === null || f.low === null || f.high === null) return "";
  if (f.low === f.high) return "";
  return `${formatFigure(f.low, f.unit)}–${formatFigure(f.high, f.unit)}`;
}

/**
 * Fold runs and their samples into one trend per metric, oldest run first.
 *
 * A run that produced no figure for a metric leaves a null in the series
 * rather than being skipped. That gap is information: it says the metric was
 * not published by that run at all, which is a different thing from being
 * published as unmeasured, and a series that closed over its gaps would draw a
 * continuous line through a hole.
 */
export function buildTrends(runs: readonly Run[], samplesByRun: ReadonlyMap<string, readonly Figure[]>): Trend[] {
  const ordered = [...runs].reverse(); // the feed is newest-first; a trend reads left to right
  const metrics = new Map<string, { family: string; unit: string }>();
  for (const run of ordered) {
    for (const f of samplesByRun.get(run.id) ?? []) {
      if (!metrics.has(f.metric)) metrics.set(f.metric, { family: f.family, unit: f.unit });
    }
  }

  const trends: Trend[] = [];
  for (const [metric, meta] of metrics) {
    const platform: (Figure | null)[] = [];
    const baseline: (Figure | null)[] = [];
    for (const run of ordered) {
      const samples = samplesByRun.get(run.id) ?? [];
      platform.push(samples.find((s) => s.metric === metric && s.arm === "platform") ?? null);
      baseline.push(samples.find((s) => s.metric === metric && s.arm === "baseline") ?? null);
    }
    trends.push({ metric, family: meta.family, unit: meta.unit, platform, baseline });
  }
  trends.sort((a, b) => a.metric.localeCompare(b.metric));
  return trends;
}

/** The families, in the order the scorecard prints them. */
export const FAMILY_ORDER = [
  "amortizedCost",
  "reliability",
  "recovery",
  "durability",
  "learningCurve",
  "speed",
  "governance",
] as const;

export function familyTitle(family: string): string {
  switch (family) {
    case "amortizedCost":
      return "Amortized cost";
    case "reliability":
      return "Reliability";
    case "recovery":
      return "Recovery";
    case "durability":
      return "Durability";
    case "learningCurve":
      return "Learning curve";
    case "speed":
      return "Speed";
    case "governance":
      return "Governance";
    default:
      return family;
  }
}

/**
 * A mark's height as a fraction of the rail, 0..1, scaled against the metric's
 * OWN range across the runs shown.
 *
 * Scaling per metric rather than globally is what lets calls, milliseconds and
 * ratios share a column: the shape says "how this metric moved", which is the
 * only question a rail this small can answer honestly.
 *
 * A NON-ZERO VALUE NEVER DRAWS NOTHING -- the floor is applied by the
 * stylesheet, not here, so this stays a pure proportion.
 */
export function markHeight(value: number, series: readonly (Figure | null)[]): number {
  let max = 0;
  for (const f of series) {
    if (f?.median !== null && f?.median !== undefined && f.median > max) max = f.median;
  }
  if (max <= 0) return value === 0 ? 0 : 1;
  return Math.max(0, Math.min(1, value / max));
}

/**
 * The prose summary of one trend, for the screen reader.
 *
 * A picture that could only be read by looking at it would leave its values
 * reachable through hover alone, which is the one thing a chart may not do.
 */
export function trendSummary(t: Trend): string {
  const say = (series: readonly (Figure | null)[], label: string): string => {
    const seen = series.filter((f): f is Figure => f !== null);
    if (seen.length === 0) return `${label}: not published.`;
    const measured = seen.filter((f) => f.median !== null);
    if (measured.length === 0) {
      const times = seen.length === 1 ? "once" : `${seen.length} times`;
      return `${label}: published ${times}, never measured. ${absenceSentence(seen[seen.length - 1]!.absent, "")}`;
    }
    const last = measured[measured.length - 1]!;
    const first = measured[0]!;
    const move =
      measured.length < 2 || last.median === first.median
        ? "unchanged"
        : `from ${formatFigure(first.median!, t.unit)} to ${formatFigure(last.median!, t.unit)}`;
    return `${label}: ${formatFigure(last.median!, t.unit)} across ${measured.length} runs, ${move}.`;
  };
  return `${t.metric}. ${say(t.platform, "Platform")} ${say(t.baseline, "Baseline")}`;
}

/**
 * How the platform's newest figure compares with the baseline's.
 *
 * Returns "" when the two are not comparable -- either side unmeasured, or
 * only one arm published. A blank is correct there: inventing a comparison
 * from an absence is exactly what the engine's `undecidable` verdict refuses
 * to do, and the surface must not do it either.
 */
export function compareArms(t: Trend): string {
  const newest = (series: readonly (Figure | null)[]): Figure | null => {
    for (let i = series.length - 1; i >= 0; i -= 1) {
      const f = series[i];
      if (f && f.median !== null) return f;
    }
    return null;
  };
  const p = newest(t.platform);
  const b = newest(t.baseline);
  if (p === null || b === null) return "";
  if (p.median === b.median) return `${formatFigure(p.median!, t.unit)} on both`;
  // BOTH FIGURES, NO VERDICT. Whether lower or higher is better is a property
  // of the metric, declared once in the Go registry, and restating it here
  // would be a second copy that can disagree. The generated scorecard page --
  // which reads that registry -- is where "improved" and "regressed" belong.
  return `${formatFigure(p.median!, t.unit)} on the platform, ${formatFigure(b.median!, t.unit)} on the bare loop`;
}
