import { describe, expect, it } from "vitest";

import type { Row } from "@znasllc-io/memql-sdk-core/client";
import {
  absenceSentence,
  buildTrends,
  compareArms,
  figureFromRow,
  formatFigure,
  formatSpread,
  markHeight,
  runFromRow,
  trendSummary,
  type Figure,
} from "../../src/apps/settings/benchmarks";

// The pure half, asserted directly. What a reading MEANS -- an absent figure
// against a measured zero, a metric that stopped being published, a run that
// measured a different corpus -- is a claim about functions, and asserting it
// through render() puts three layers that can each fail for unrelated reasons
// between the claim and the check.

function sampleRow(over: Partial<Record<string, unknown>> = {}): Row {
  return {
    id: "v1:bench:sample:1",
    concept: "v1:bench:sample",
    benchRunId: "v1:bench:run:1",
    family: "durability",
    scenarioId: "durability.a-stopped-run-resumes-with-no-duplicated-effect",
    arm: "platform",
    metric: "durability.duplicatedSideEffects",
    unit: "count",
    n: 3,
    median: 0,
    p10: 0,
    p90: 0,
    minimum: 0,
    maximum: 0,
    mad: 0,
    absentReason: "",
    detail: "",
    tier: "ci",
    commit: "9e91625",
    measuredOn: "2026-09-06",
    ...over,
  } as unknown as Row;
}

describe("a measured zero and an unmeasured figure are different answers", () => {
  it("keeps a measured zero as the number zero", () => {
    const f = figureFromRow(sampleRow());
    expect(f.median).toBe(0);
    expect(f.absent).toBe("");
    expect(formatFigure(f.median!, f.unit)).toBe("0");
  });

  it("reads an unmeasured figure as null and NEVER as zero", () => {
    // The single most load-bearing assertion in this file. An unmeasured
    // sample carries no median at all, and reading one as 0 would collapse
    // "nothing measured this" into "this measured zero" -- which, for
    // durability's headline, is the difference between the claim and its
    // opposite.
    const f = figureFromRow(sampleRow({ absentReason: "notMeasurableOnReplay", median: 0, n: 0 }));
    expect(f.median).toBeNull();
    expect(f.n).toBeNull();
    expect(f.absent).toBe("notMeasurableOnReplay");
  });

  it("ignores a stale median left on an unmeasured row", () => {
    // Belt to the engine's braces: even if a writer ever put a number beside
    // an absent reason, the surface refuses to show it.
    const f = figureFromRow(sampleRow({ absentReason: "seamNotBuilt", median: 42 }));
    expect(f.median).toBeNull();
  });
});

describe("absenceSentence", () => {
  it("says why, in words, for every reason the engine can write", () => {
    for (const reason of [
      "notMeasurableOnReplay",
      "seamNotBuilt",
      "tierNotRun",
      "noProvider",
      "belowFloor",
      "ceilingReached",
    ]) {
      const sentence = absenceSentence(reason, "");
      expect(sentence).not.toBe(reason);
      expect(sentence.length).toBeGreaterThan(20);
    }
  });

  it("appends the detail, which for seamNotBuilt names the missing code", () => {
    const s = absenceSentence("seamNotBuilt", "nothing writes v1:work:modelCall");
    expect(s).toContain("not built");
    expect(s).toContain("v1:work:modelCall");
  });

  it("falls through to the value itself rather than to a shrug", () => {
    // A reason the OS has not learned yet is still more informative than
    // "unknown".
    expect(absenceSentence("somethingNewer", "")).toBe("somethingNewer");
  });
});

describe("formatting", () => {
  it("renders a ratio as a percentage, because a reader takes 0.71 for a probability", () => {
    expect(formatFigure(0.714, "ratio")).toBe("71.4%");
  });

  it("renders dollars, milliseconds and seconds in their own terms", () => {
    expect(formatFigure(0.0412, "usd")).toBe("$0.0412");
    expect(formatFigure(250, "ms")).toBe("250ms");
    expect(formatFigure(2500, "ms")).toBe("2.50s");
  });

  it("prints no spread when there is none worth printing", () => {
    const f = figureFromRow(sampleRow()) as Figure;
    expect(formatSpread(f)).toBe("");
  });

  it("prints the spread when the sample has one", () => {
    const f = figureFromRow(sampleRow({ median: 5, p10: 2, p90: 9, unit: "calls" }));
    expect(formatSpread(f)).toBe("2–9");
  });
});

describe("markHeight", () => {
  it("scales against the metric's own range, so calls and milliseconds share a column", () => {
    const series = [
      figureFromRow(sampleRow({ median: 2 })),
      figureFromRow(sampleRow({ median: 10 })),
    ];
    expect(markHeight(10, series)).toBe(1);
    expect(markHeight(5, series)).toBe(0.5);
  });

  it("gives a measured zero a height of zero and leaves the floor to the stylesheet", () => {
    // The floor lives in CSS so this stays a pure proportion; the rule that a
    // non-zero value never draws nothing is asserted there.
    const series = [figureFromRow(sampleRow({ median: 0 }))];
    expect(markHeight(0, series)).toBe(0);
  });
});

describe("buildTrends", () => {
  const runs = [
    runFromRow({ id: "r2", concept: "v1:bench:run", commit: "bbb", startedAt: "2026-09-06" } as unknown as Row),
    runFromRow({ id: "r1", concept: "v1:bench:run", commit: "aaa", startedAt: "2026-09-05" } as unknown as Row),
  ];

  it("orders oldest first, because a trend reads left to right", () => {
    const samples = new Map<string, Figure[]>([
      ["r1", [figureFromRow(sampleRow({ median: 1, commit: "aaa" }))]],
      ["r2", [figureFromRow(sampleRow({ median: 2, commit: "bbb" }))]],
    ]);
    const [t] = buildTrends(runs, samples);
    expect(t!.platform.map((f) => f?.commit)).toEqual(["aaa", "bbb"]);
  });

  it("leaves a null where a run published nothing, rather than closing the gap", () => {
    // A series that closed over its gaps would draw a continuous line through
    // a hole, and the hole is information: it says the metric was not
    // published by that run at all, which is different from being published
    // as unmeasured.
    const samples = new Map<string, Figure[]>([
      ["r1", []],
      ["r2", [figureFromRow(sampleRow({ median: 2 }))]],
    ]);
    const [t] = buildTrends(runs, samples);
    expect(t!.platform[0]).toBeNull();
    expect(t!.platform[1]?.median).toBe(2);
  });
});

describe("compareArms", () => {
  const runs = [runFromRow({ id: "r1", concept: "v1:bench:run" } as unknown as Row)];

  it("states the comparison the epic asks for, in words", () => {
    const samples = new Map<string, Figure[]>([
      [
        "r1",
        [
          figureFromRow(sampleRow({ arm: "platform", median: 0, unit: "count" })),
          figureFromRow(sampleRow({ arm: "baseline", median: 1, unit: "count" })),
        ],
      ],
    ]);
    const [t] = buildTrends(runs, samples);
    const said = compareArms(t!);
    expect(said).toContain("0 on the platform");
    expect(said).toContain("1 on the bare loop");
  });

  it("states BOTH figures and no verdict", () => {
    // Whether lower or higher is better is a property of the metric, declared
    // once in the Go registry. Restating it here would be a second copy that
    // can disagree -- and it read backwards at real size on recovery.rate,
    // where higher is better and the sentence said "baseline lower than
    // platform" as if lower were the point.
    const samples = new Map<string, Figure[]>([
      [
        "r1",
        [
          figureFromRow(sampleRow({ arm: "platform", median: 1, unit: "ratio" })),
          figureFromRow(sampleRow({ arm: "baseline", median: 0.75, unit: "ratio" })),
        ],
      ],
    ]);
    const [t] = buildTrends(runs, samples);
    const said = compareArms(t!);
    expect(said).toBe("100.0% on the platform, 75.0% on the bare loop");
    expect(said).not.toMatch(/lower|higher|better|worse|improved|regressed/);
  });

  it("says NOTHING when one side is unmeasured", () => {
    // Inventing a comparison from an absence is exactly what the engine's
    // `undecidable` verdict refuses to do, and the surface must not do it
    // either.
    const samples = new Map<string, Figure[]>([
      [
        "r1",
        [
          figureFromRow(sampleRow({ arm: "platform", median: 0 })),
          figureFromRow(sampleRow({ arm: "baseline", absentReason: "notMeasurableOnReplay" })),
        ],
      ],
    ]);
    const [t] = buildTrends(runs, samples);
    expect(compareArms(t!)).toBe("");
  });
});

describe("trendSummary", () => {
  it("puts every value in words, because a chart may not be readable by sight alone", () => {
    const runs = [runFromRow({ id: "r1", concept: "v1:bench:run" } as unknown as Row)];
    const samples = new Map<string, Figure[]>([
      [
        "r1",
        [
          figureFromRow(sampleRow({ arm: "platform", median: 0, unit: "count" })),
          figureFromRow(sampleRow({ arm: "baseline", median: 2, unit: "count" })),
        ],
      ],
    ]);
    const [t] = buildTrends(runs, samples);
    const said = trendSummary(t!);
    expect(said).toContain("durability.duplicatedSideEffects");
    expect(said).toContain("Platform: 0");
    expect(said).toContain("Baseline: 2");
  });

  it("says an all-unmeasured series was never measured, and why", () => {
    const runs = [runFromRow({ id: "r1", concept: "v1:bench:run" } as unknown as Row)];
    const samples = new Map<string, Figure[]>([
      ["r1", [figureFromRow(sampleRow({ arm: "platform", absentReason: "tierNotRun" }))]],
    ]);
    const [t] = buildTrends(runs, samples);
    expect(trendSummary(t!)).toContain("published once, never measured");
    expect(trendSummary(t!)).toContain("has not been run");
  });
});
