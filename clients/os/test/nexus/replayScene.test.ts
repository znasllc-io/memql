import { describe, expect, it } from "vitest";

import { NOW, goalProgress, scene, stepStatusAt } from "../../src/nexus/scene/scene";
import { layout } from "../../src/nexus/scene/layout";
import {
  approval,
  chainWorld,
  goal,
  moment,
  run,
  step,
  world,
} from "../../src/nexus/scene/fixtures";

describe("the world at a moment", () => {
  it("returns the world itself at NOW, without copying every collection", () => {
    const w = chainWorld(3);
    expect(scene(w, NOW)).toBe(w);
  });

  it("drops rows that did not exist yet", () => {
    const w = chainWorld(4, "done");
    const early = scene(w, moment(2));
    expect(early.steps.length).toBeLessThan(w.steps.length);
  });

  // The wrong-in-the-VISIBLE-direction choice: a node that genuinely exists
  // vanishing the moment somebody touches the scrubber reads as a rendering
  // bug, and appearing early reads as what it is.
  it("keeps a row the cluster never dated, at every position", () => {
    const w = world({ steps: [step("a", { seq: 0, createdAt: "" })] });
    expect(scene(w, moment(-100)).steps).toHaveLength(1);
  });

  it("reports the status the row HAD, never the status it has now", () => {
    const finished = step("a", {
      seq: 0,
      status: "failed",
      startedAt: moment(2),
      finishedAt: moment(9),
    });
    expect(stepStatusAt(finished, moment(5))).toBe("running");
    expect(stepStatusAt(finished, moment(1))).toBe("pending");
    expect(stepStatusAt(finished, moment(9))).toBe("failed");
    expect(stepStatusAt(finished, NOW)).toBe("failed");
  });

  it("un-decides an approval decided after the moment", () => {
    const w = world({
      approvals: [approval({ decision: "approved", decidedAt: moment(12) })],
    });
    expect(scene(w, moment(8)).approvals[0]?.decision).toBe("");
    expect(scene(w, moment(13)).approvals[0]?.decision).toBe("approved");
  });

  // Otherwise the first frames of a replay render a goal with no run selected
  // and therefore no road at all.
  it("keeps the open run selected even before it existed", () => {
    const w = chainWorld(2);
    expect(scene(w, moment(-100)).run?.id).toBe(w.run?.id);
  });

  it("still lays out at an early moment", () => {
    const result = layout(scene(chainWorld(5, "done"), moment(2)));
    expect(result.nodes.size).toBeGreaterThan(0);
  });
});

describe("what fills the beacon", () => {
  it("counts per step key, so a retried run can still fill completely", () => {
    const w = world({
      steps: [
        { ...step("a", { seq: 0, status: "failed", attempt: 1 }), id: "s-a-1" },
        { ...step("a", { seq: 0, status: "done", attempt: 2 }), id: "s-a-2" },
        { ...step("b", { seq: 1, status: "done" }), id: "s-b-1" },
      ],
    });
    const progress = goalProgress(w);
    expect(progress.total).toBe(2);
    expect(progress.completed).toBe(2);
    expect(progress.fraction).toBe(1);
  });

  it("says COMPILING rather than zero when there is no denominator yet", () => {
    const w = world({ run: run({ status: "compiling" }), steps: [] });
    const progress = goalProgress(w);
    expect(progress.compiling).toBe(true);
    expect(progress.fraction).toBe(0);
  });

  it("is dark until the goal is closed, however well the run went", () => {
    const running = world({
      goal: goal({ status: "active" }),
      run: run({ status: "succeeded" }),
      steps: [step("a", { seq: 0, status: "done" })],
    });
    expect(goalProgress(running).lit).toBe(false);

    const closed = world({
      goal: goal({ status: "closed" }),
      run: run({ status: "succeeded" }),
      steps: [step("a", { seq: 0, status: "done" })],
    });
    expect(goalProgress(closed).lit).toBe(true);
  });
});
