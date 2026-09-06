import { describe, expect, it } from "vitest";

import {
  approvalsOfRun,
  bindingNodeId,
  compareSteps,
  conceptIdForKind,
  depths,
  latestAttempts,
  readApproval,
  readGoal,
  readRun,
  readStep,
  stepNodeId,
  stepsOfRun,
} from "../../src/nexus/scene/world";
import {
  approval,
  chainWorld,
  moment,
  retryWorld,
  step,
  wireRows,
  world,
} from "../../src/nexus/scene/fixtures";

// The narrowing layer. Every test here is about a row the wire could actually
// send, which means a row with keys missing -- that is what these readers are
// for, and a fixture where every key is present tests nothing.

describe("the readers survive a projection gap", () => {
  it("reads a full goal row", () => {
    const row = readGoal(wireRows.goal);
    expect(row?.statement).toBe("Ship the Q4 pricing page");
    expect(row?.requestedVia).toBe("nexus");
  });

  it("reads a goal row carrying nothing but an id", () => {
    const row = readGoal(wireRows.sparseGoal);
    expect(row?.id).toBe("v1:work:goal:g2");
    expect(row?.statement).toBe("");
    expect(row?.status).toBe("");
  });

  it("refuses a row with no id rather than inventing one", () => {
    expect(readGoal({})).toBeNull();
    expect(readGoal(null)).toBeNull();
    expect(readRun({})).toBeNull();
  });

  it("narrows the run's nested objects", () => {
    const row = readRun(wireRows.run);
    expect(row?.spent.tokens).toBe(1200);
    expect(row?.spent.cost).toBeCloseTo(0.41);
    expect(row?.spent.present).toBe(true);
    expect(row?.waitingKind).toBe("approval");
    expect(row?.stepOrder).toEqual(["s0", "s1"]);
  });

  it("says a run with no spend object is UNMEASURED, not zero", () => {
    const row = readRun(wireRows.sparseRun);
    expect(row?.spent.present).toBe(false);
    expect(row?.spent.tokens).toBe(0);
  });

  it("reads a step's binding and skills", () => {
    const row = readStep(wireRows.step);
    expect(row.binding.present).toBe(true);
    expect(row.binding.model).toBe("claude-opus-5");
    expect(row.binding.skillIds).toEqual(["s:a", "s:b"]);
    expect(row.callName).toBe("decide");
    expect(row.attempt).toBe(2);
  });

  // THE ONE THAT `?? 1` WOULD HAVE GOT WRONG. `rowNumber` answers 0 for an
  // absent key rather than null, so a null-coalesce compiles, reads correctly
  // and never fires -- and every step in the cluster would be attempt 0.
  it("reads an absent attempt as the FIRST attempt", () => {
    expect(readStep(wireRows.sparseStep).attempt).toBe(1);
  });

  it("says a step with no binding has not been bound", () => {
    const row = readStep(wireRows.sparseStep);
    expect(row.binding.present).toBe(false);
    expect(row.binding.skillIds).toEqual([]);
  });

  it("reads an approval's evidence", () => {
    const row = readApproval(wireRows.approval);
    expect(row.tier).toBe("B");
    expect(row.ruleId).toBe("spend-ceiling");
    expect(row.decision).toBe("");
  });
});

describe("node identity is not row identity", () => {
  it("keys a step's node on runId:key, so a retry re-lights one node", () => {
    const attempts = retryWorld().steps;
    const ids = new Set(attempts.map(stepNodeId));
    expect(attempts).toHaveLength(3);
    expect(ids.size).toBe(1);
  });

  it("names the LATEST attempt as the row a node re-reads", () => {
    const world = retryWorld();
    const latest = latestAttempts(world.steps).get("flaky");
    expect(latest?.attempt).toBe(3);
    expect(latest?.id).toBe("v1:work:step:flaky-3");
  });

  it("gives a binding its own node id", () => {
    const s = step("a");
    expect(bindingNodeId(s)).not.toBe(stepNodeId(s));
  });

  it("gives no concept to the nodes that have no row", () => {
    expect(conceptIdForKind("you")).toBe("");
    expect(conceptIdForKind("cluster")).toBe("");
    expect(conceptIdForKind("fold")).toBe("");
    expect(conceptIdForKind("step")).not.toBe("");
    expect(conceptIdForKind("goal")).not.toBe("");
  });
});

describe("depth is a fact about dependsOn", () => {
  it("puts a chain on one step per column", () => {
    const w = chainWorld(4);
    const d = depths(stepsOfRun(w, "v1:work:run:r1"));
    expect(d.get("s0")).toBe(0);
    expect(d.get("s3")).toBe(3);
  });

  it("puts steps that can run together in the same column", () => {
    const steps = [
      step("root", { seq: 0 }),
      step("a", { seq: 1, dependsOn: ["root"] }),
      step("b", { seq: 2, dependsOn: ["root"] }),
    ];
    const d = depths(steps);
    expect(d.get("a")).toBe(1);
    expect(d.get("b")).toBe(1);
  });

  // A malformed template must render as a flat map, never as a tab that stops
  // responding.
  it("does not hang on a cycle", () => {
    const steps = [
      step("a", { seq: 0, dependsOn: ["b"] }),
      step("b", { seq: 1, dependsOn: ["a"] }),
    ];
    const d = depths(steps);
    expect(d.size).toBe(2);
  });

  it("ignores a dependency on a step this run does not have", () => {
    const d = depths([step("a", { seq: 0, dependsOn: ["from-another-run"] })]);
    expect(d.get("a")).toBe(0);
  });
});

describe("ordering is total and stable", () => {
  it("orders by seq then key, never by insertion", () => {
    const a = step("z", { seq: 0 });
    const b = step("a", { seq: 0 });
    expect(compareSteps(a, b)).toBeGreaterThan(0);
    expect(compareSteps(b, a)).toBeLessThan(0);
    expect(compareSteps(a, a)).toBe(0);
  });

  it("orders a run's approvals by request time", () => {
    const w = world({
      approvals: [
        approval({ id: "v1:work:approval:b", requestedAt: moment(9) }),
        approval({ id: "v1:work:approval:a", requestedAt: moment(6) }),
      ],
    });
    expect(approvalsOfRun(w, "v1:work:run:r1").map((a) => a.id)).toEqual([
      "v1:work:approval:a",
      "v1:work:approval:b",
    ]);
  });
});
