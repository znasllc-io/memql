import { describe, expect, it } from "vitest";

import { formatElapsed, receipt } from "../../src/nexus/scene/receipt";
import { approval, chainWorld, moment, run, step, world } from "../../src/nexus/scene/fixtures";

describe("the receipt", () => {
  it("is absent while the run is still going, which IS the statement", () => {
    expect(receipt(chainWorld(3, "running"))).toBeNull();
  });

  it("counts step keys as work and rows as attempts", () => {
    const w = world({
      run: run({ status: "succeeded", finishedAt: moment(30) }),
      steps: [
        { ...step("a", { seq: 0, status: "failed", attempt: 1 }), id: "r1" },
        { ...step("a", { seq: 0, status: "done", attempt: 2 }), id: "r2" },
        { ...step("b", { seq: 1, status: "done", kind: "reasoning" }), id: "r3" },
      ],
    });
    const card = receipt(w);
    expect(card?.steps).toBe(2);
    expect(card?.attempts).toBe(3);
    expect(card?.thought).toBe(1);
  });

  // The version of this feature that flatters instead of informing is the one
  // that hides the cost when the outcome was bad.
  it("gives a failed run the same card, plus the failure and the step in flight", () => {
    const w = world({
      run: run({
        status: "failed",
        finishedAt: moment(30),
        errorMessage: "the provider refused",
      }),
      steps: [
        step("a", { seq: 0, status: "done" }),
        step("b", { seq: 1, status: "running", finishedAt: "" }),
      ],
    });
    const card = receipt(w);
    expect(card?.outcome).toBe("failed");
    expect(card?.failure).toBe("the provider refused");
    expect(card?.lastRunningStep).toBe("b");
  });

  it("names who cancelled, and only on a cancellation", () => {
    const w = world({
      run: run({ status: "cancelled", finishedAt: moment(9), cancelledBy: "v1:identity:user:u1" }),
    });
    expect(receipt(w)?.cancelledBy).toBe("v1:identity:user:u1");
  });

  // Zero and "not measured" are different answers.
  it("renders an ABSENT spend as null rather than as zero", () => {
    const unmeasured = world({
      run: {
        ...run({ status: "succeeded", finishedAt: moment(9) }),
        spent: {
          tokens: 0,
          tokensSubscription: 0,
          cost: 0,
          modelCalls: 0,
          retries: 0,
          wallClockMs: 0,
          present: false,
        },
      },
    });
    const card = receipt(unmeasured);
    expect(card?.cost).toBeNull();
    expect(card?.modelCalls).toBeNull();
    expect(card?.subscriptionTokens).toBeNull();
  });

  it("counts the approvals the run had to raise", () => {
    const w = world({
      run: run({ status: "succeeded", finishedAt: moment(9) }),
      approvals: [approval({ id: "a1" }), approval({ id: "a2" })],
    });
    expect(receipt(w)?.approvals).toBe(2);
  });

  it("declines a duration it cannot compute rather than printing NaN", () => {
    const w = world({ run: run({ status: "succeeded", startedAt: "", finishedAt: moment(9) }) });
    expect(receipt(w)?.elapsedMs).toBe(-1);
    expect(formatElapsed(-1)).toBe("");
  });

  it("renders the two largest units and never more", () => {
    expect(formatElapsed(8_000)).toBe("8s");
    expect(formatElapsed(750_000)).toBe("12m 30s");
    expect(formatElapsed(15_120_000)).toBe("4h 12m");
    expect(formatElapsed(200_000_000)).toBe("2d 7h");
  });
});
