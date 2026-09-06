import { describe, expect, it } from "vitest";

import { compareEvents, events, timelineBounds } from "../../src/nexus/scene/events";
import { layout } from "../../src/nexus/scene/layout";
import { GOAL_NODE_ID } from "../../src/nexus/scene/world";
import {
  approval,
  chainWorld,
  goal,
  moment,
  retryWorld,
  run,
  step,
  world,
} from "../../src/nexus/scene/fixtures";

// A SCRUBBER IS READ AS EVIDENCE, which is the whole reason this module has a
// rule instead of a heuristic: a moment with no timestamp produces no event.
// Every test in the first block has a reachable positive beside it, because
// "no events" is otherwise indistinguishable from a broken query.

describe("an event is never invented", () => {
  it("produces no completion for a step that says it is done and never says when", () => {
    const undated = world({
      steps: [step("a", { seq: 0, status: "done", finishedAt: "" })],
    });
    const kinds = events(undated).map((e) => e.kind);
    expect(kinds).not.toContain("step.completed");
    // The reachable positive: the same row WITH a stamp produces exactly one.
    const dated = world({ steps: [step("a", { seq: 0, status: "done" })] });
    expect(events(dated).filter((e) => e.kind === "step.completed")).toHaveLength(1);
  });

  it("produces no terminal event for a run that says it succeeded and never says when", () => {
    const undated = world({ run: run({ status: "succeeded", finishedAt: "" }) });
    expect(events(undated).map((e) => e.kind)).not.toContain("run.succeeded");
    const dated = world({ run: run({ status: "succeeded", finishedAt: moment(9) }) });
    expect(events(dated).filter((e) => e.kind === "run.succeeded")).toHaveLength(1);
  });

  it("treats whitespace as absent, because the wire can carry it", () => {
    const w = world({ steps: [step("a", { seq: 0, status: "done", finishedAt: "   " })] });
    expect(events(w).map((e) => e.kind)).not.toContain("step.completed");
  });

  it("takes the terminal KIND from the status and the MOMENT from the row", () => {
    const failed = world({
      run: run({ status: "failed", finishedAt: moment(9), errorMessage: "provider refused" }),
    });
    const terminal = events(failed).find((e) => e.kind === "run.failed");
    expect(terminal?.at).toBe(moment(9));
    expect(terminal?.label).toContain("provider refused");
  });
});

describe("a retry re-lights, it does not duplicate", () => {
  it("emits a pair per attempt, all naming ONE node", () => {
    const list = events(retryWorld());
    const starts = list.filter((e) => e.kind === "step.started");
    expect(starts).toHaveLength(3);
    expect(new Set(starts.map((e) => e.nodeId)).size).toBe(1);
    expect(starts.map((e) => e.attempt).sort()).toEqual([1, 2, 3]);
  });

  it("says WHICH attempt in the label, which the map's re-light cannot", () => {
    const second = events(retryWorld()).find(
      (e) => e.kind === "step.started" && e.attempt === 2,
    );
    expect(second?.label).toContain("attempt 2");
  });
});

describe("every event names a node the layout drew", () => {
  it("holds for a chain", () => {
    const w = chainWorld(4, "running");
    const drawn = layout(w).nodes;
    for (const event of events(w)) {
      expect(drawn.has(event.nodeId)).toBe(true);
    }
  });

  it("holds for a goal and its approvals", () => {
    const w = world({
      steps: [step("a", { seq: 0, status: "waiting" })],
      approvals: [approval({ stepKey: "a", decidedAt: moment(8), decision: "approved" })],
    });
    const drawn = layout(w).nodes;
    for (const event of events(w)) {
      expect(drawn.has(event.nodeId)).toBe(true);
    }
    expect(events(w).some((e) => e.kind === "approval.decided")).toBe(true);
  });

  it("lights the beacon node for the goal's own moments", () => {
    const w = world({ goal: goal({ closedAt: moment(20), closeReason: "shipped" }) });
    const closed = events(w).find((e) => e.kind === "goal.closed");
    expect(closed?.nodeId).toBe(GOAL_NODE_ID);
    expect(closed?.label).toContain("shipped");
  });
});

describe("the order is total", () => {
  it("puts creation before start before finish at the same instant", () => {
    const same = moment(5);
    const w = world({
      steps: [step("a", { seq: 0, createdAt: same, startedAt: same, finishedAt: same })],
    });
    expect(events(w).map((e) => e.kind)).toEqual([
      "goal.created",
      "run.started",
      "step.created",
      "step.started",
      "step.completed",
    ]);
  });

  it("breaks a remaining tie on the id, so two renders never disagree", () => {
    const a = { id: "a", at: moment(1), kind: "step.started" as const, nodeId: "n", rowId: "", label: "", attempt: 1 };
    const b = { ...a, id: "b" };
    expect(compareEvents(a, b)).toBeLessThan(0);
    expect(compareEvents(b, a)).toBeGreaterThan(0);
  });
});

describe("the span the scrubber covers", () => {
  it("is empty for a world with nothing dated", () => {
    const w = world({
      goal: goal({ createdAt: "" }),
      run: run({ startedAt: "", createdAt: "" }),
    });
    expect(timelineBounds(events(w))).toEqual({ from: "", to: "", count: 0 });
  });

  it("runs from the first moment to the last", () => {
    const bounds = timelineBounds(events(chainWorld(3, "done")));
    expect(bounds.from).toBe(moment(0));
    expect(bounds.count).toBeGreaterThan(3);
    expect(bounds.to > bounds.from).toBe(true);
  });
});
