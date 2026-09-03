import { describe, expect, it } from "vitest";
import { buildLogsSearch, buildLogsTail } from "@znasllc-io/memql-sdk-core/client";

import {
  DEFAULT_FILTERS,
  constraintsOf,
  isNarrowed,
  levelsFor,
  subjectIntentOf,
  toSearchArgs,
  toTailArgs,
  windowBounds,
  windowPhrase,
  withinWindow,
  type LogFilters,
} from "../../src/logs/filters";
import { logRowFromRow } from "../../src/logs/rows";

// The facet model, pure (epic memql#4895). Each facet is asserted on the
// RENDERED call as well as on the argument object, because the string is what
// the engine parses.

const NOW = new Date("2026-09-03T12:00:00Z");

function filters(over: Partial<LogFilters> = {}): LogFilters {
  return { ...DEFAULT_FILTERS, ...over };
}

describe("the tail's arguments", () => {
  it("carries the app's scope -- its id and the concepts it owns -- and nothing else by default", () => {
    const args = toTailArgs({ apps: ["files"], subjectConcepts: ["v1:library:artifact"] }, filters());
    expect(args).toEqual({ apps: ["files"], subjectConcepts: ["v1:library:artifact"] });
    expect(buildLogsTail(args)).toBe(
      'builtin logsTail(apps: ["files"], subjectConcepts: ["v1:library:artifact"])',
    );
  });

  it("renders an empty scope as a bare call: the whole store", () => {
    expect(buildLogsTail(toTailArgs({}, filters()))).toBe("builtin logsTail()");
  });

  it("turns the level floor into the levels at and above it, and 'all' into no constraint", () => {
    expect(levelsFor("all")).toBeUndefined();
    expect(levelsFor("info")).toEqual(["info", "warn", "error"]);
    expect(levelsFor("warn")).toEqual(["warn", "error"]);
    expect(levelsFor("error")).toEqual(["error"]);
    expect(buildLogsTail(toTailArgs({}, filters({ levelFloor: "warn" })))).toBe(
      'builtin logsTail(levels: ["warn", "error"])',
    );
  });

  it("narrows on components, nodes, subject, subject concept and text, trimmed", () => {
    const args = toTailArgs(
      {},
      filters({
        components: ["packages.pipeline"],
        nodes: ["bff-0"],
        subject: "  site-1 ",
        subjectConcept: "v1:platform:site",
        text: " timeout ",
      }),
    );
    expect(args).toEqual({
      components: ["packages.pipeline"],
      nodes: ["bff-0"],
      subject: "site-1",
      subjectConcept: "v1:platform:site",
      text: "timeout",
    });
  });

  it("drops a blank subject and the concept with it: the concept only disambiguates a subject", () => {
    const args = toTailArgs({}, filters({ subject: "   ", subjectConcept: "v1:platform:site" }));
    expect(args.subject).toBeUndefined();
    expect(args.subjectConcept).toBeUndefined();
  });

  it("an app facet REPLACES the scope's apps rather than adding to them", () => {
    // In the Logs app the scope is empty and the facet is the whole answer;
    // in a per-app section there is no app facet. Either way one list.
    const args = toTailArgs({ apps: ["files"] }, filters({ apps: ["fleet"] }));
    expect(args.apps).toEqual(["fleet"]);
  });
});

describe("the search's window", () => {
  it("anchors a preset to now, half-open", () => {
    const args = toSearchArgs({}, filters({ window: "15m" }), NOW);
    expect(args?.windowStart).toBe("2026-09-03T11:45:00.000Z");
    expect(args?.windowEnd).toBe("2026-09-03T12:00:00.000Z");
    expect(buildLogsSearch(args!)).toBe(
      'builtin logsSearch(windowStart: "2026-09-03T11:45:00.000Z", windowEnd: "2026-09-03T12:00:00.000Z")',
    );
  });

  it("reads a custom window off the two datetime-local values", () => {
    const bounds = windowBounds({ window: "custom", from: "2026-09-01T10:00", to: "2026-09-02T10:00" }, NOW);
    expect(bounds).not.toBeNull();
    expect(bounds!.end.getTime() - bounds!.start.getTime()).toBe(24 * 60 * 60_000);
  });

  it("is not yet a window when custom is blank, unparseable, or ends before it starts", () => {
    expect(windowBounds({ window: "custom", from: "", to: "" }, NOW)).toBeNull();
    expect(windowBounds({ window: "custom", from: "nope", to: "2026-09-02T10:00" }, NOW)).toBeNull();
    expect(windowBounds({ window: "custom", from: "2026-09-02T10:00", to: "2026-09-01T10:00" }, NOW)).toBeNull();
    expect(toSearchArgs({}, filters({ window: "custom" }), NOW)).toBeNull();
  });

  it("names the window as the tail of a sentence", () => {
    expect(windowPhrase("1h")).toBe("in the last hour");
    expect(windowPhrase("15m")).toBe("in the last 15 minutes");
    expect(windowPhrase("custom")).toBe("in this window");
  });
});

describe("the window fold over a tail", () => {
  it("keeps only the rows inside the window, against the clock", () => {
    const inside = logRowFromRow({ id: "a", occurredAt: "2026-09-03T11:30:00Z", level: "info" });
    const outside = logRowFromRow({ id: "b", occurredAt: "2026-09-03T10:30:00Z", level: "info" });
    expect(withinWindow([inside, outside], "1h", NOW).map((r) => r.id)).toEqual(["a"]);
    expect(withinWindow([inside, outside], "6h", NOW).map((r) => r.id)).toEqual(["a", "b"]);
  });
});

describe("what counts as narrowed", () => {
  it("is every facet but the window: the window is always set, so it cannot be the reason a list is empty", () => {
    expect(isNarrowed(filters())).toBe(false);
    expect(isNarrowed(filters({ window: "15m" }))).toBe(false);
    expect(isNarrowed(filters({ levelFloor: "error" }))).toBe(true);
    expect(isNarrowed(filters({ text: "x" }))).toBe(true);
    expect(isNarrowed(filters({ subject: "s" }))).toBe(true);
    expect(isNarrowed(filters({ components: ["c"] }))).toBe(true);
  });

  it("describes each active constraint with the patch that clears it", () => {
    const active = constraintsOf(
      filters({ levelFloor: "warn", components: ["a", "b"], nodes: ["n"], apps: ["files"], subject: "s-1" }),
    );
    expect(active.map((c) => c.label)).toEqual(["Warnings and above", "a", "b", "n", "app files", "subject s-1"]);
    expect(active[0]?.clear).toEqual({ levelFloor: "all" });
    expect(active[1]?.clear).toEqual({ components: ["b"] });
    expect(active[5]?.clear).toEqual({ subject: "", subjectConcept: "" });
    // The search text is carried by the Refine affordance itself.
    expect(constraintsOf(filters({ text: "x" }))).toEqual([]);
  });
});

describe("the subject intent", () => {
  it("reads the one shape a logs surface acts on and nothing else", () => {
    expect(subjectIntentOf({ subject: " site-1 ", subjectConcept: "v1:platform:site" })).toEqual({
      subject: "site-1",
      subjectConcept: "v1:platform:site",
    });
    expect(subjectIntentOf({ subject: "site-1" })).toEqual({ subject: "site-1", subjectConcept: "" });
    // Another surface's instruction -- the Files place intent -- is left standing.
    expect(subjectIntentOf({ place: "desktop", folderId: "f-1" })).toBeNull();
    expect(subjectIntentOf({ subject: "  " })).toBeNull();
  });
});
