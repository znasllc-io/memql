// The receipt (design D8): what a goal cost and what it left behind.
//
// MOVED FROM THE PORTAL with the pure scene library (work spine A1, decision
// D7). Only the ARITHMETIC came: it is a function over rows and tests on
// fixtures with no React and no GPU. The card that rendered it was a portal
// page and went with the pages -- sub-project B draws its own on MemQL OS,
// over these same numbers.
//
// The distinction this file is most careful about is ABSENT versus ZERO. A
// run that spent nothing on its subscription and a cluster that does not
// record what the subscription covered are different facts, and a card that
// printed "0" for the second would be inventing a measurement. That rule is
// what B has to carry into whatever it draws.

import { describe, expect, it } from "vitest";

import { formatElapsed, receipt } from "../../src/nexus/scene/receipt";
import {
  cancelledGoal,
  emptyGoal,
  failedGoal,
  springCatalogGoal,
} from "../../src/nexus/scene/fixtures";

describe("the receipt's arithmetic", () => {
  it("is absent for a goal that has not ended -- the absence IS the statement", () => {
    expect(receipt(emptyGoal())).toBeNull();
    const running = springCatalogGoal();
    expect(
      receipt({
        ...running,
        plan: running.plan === null ? null : { ...running.plan, status: "running", completedAt: "" },
      }),
    ).toBeNull();
  });

  it("counts steps once and tasks per row, which are different questions", () => {
    const r = receipt(springCatalogGoal());
    expect(r).not.toBeNull();
    // Seven semantic ROWS, six distinct STEPS -- one of them was retried.
    // "How much work was this" is the second number; the first is how many
    // times the engine ran something.
    expect(r!.tasks).toBe(7);
    expect(r!.attempts).toBe(6);
    expect(r!.agents).toBe(4);
    expect(r!.artifacts).toBe(2);
    expect(r!.constructs).toBe(3);
    expect(r!.tokensSpent).toBe(184_320);
  });

  it("reports an absent subscription figure as absent, never as zero", () => {
    const world = springCatalogGoal();
    // The engine serving this plan does not carry the field (epic #4358 has
    // not landed there).
    expect(receipt(world)!.subscriptionCovered).toBeNull();

    const covered = receipt({
      ...world,
      plan:
        world.plan === null
          ? null
          : { ...world.plan, hasTokenSpentSubscription: true, tokenSpentSubscription: 0 },
    });
    // Present and zero: the subscription covered nothing, which is a
    // measurement rather than a silence.
    expect(covered!.subscriptionCovered).toBe(0);
  });

  it("names the failure and the task that was still running", () => {
    const r = receipt(failedGoal());
    expect(r!.outcome).toBe("failed");
    expect(r!.failure).toBe("the supplier feed stopped responding");
    expect(r!.lastRunningTask).toBe("shape-price");
    // The cost is still reported. A card that appeared only on success would
    // be a scoreboard rather than a record.
    expect(r!.tokensSpent).toBeGreaterThan(0);
    expect(r!.elapsedMs).toBeGreaterThan(0);
  });

  it("says who cancelled a cancelled goal", () => {
    const r = receipt(cancelledGoal());
    expect(r!.outcome).toBe("cancelled");
    expect(r!.cancelledBy).toBe("user-1");
    expect(r!.failure).toBe("");
  });

  it("declines to compute a duration it cannot, rather than printing a zero", () => {
    const world = springCatalogGoal();
    const undated = receipt({
      ...world,
      plan: world.plan === null ? null : { ...world.plan, createdAt: "" },
    });
    expect(undated!.elapsedMs).toBe(-1);
    expect(formatElapsed(-1)).toBe("");
    // ...and a goal that genuinely took no measurable time still reads as 0s,
    // which is a different answer from "unknown".
    expect(formatElapsed(0)).toBe("0s");
  });

  it("renders a duration as its two largest units and no more", () => {
    expect(formatElapsed(8_000)).toBe("8s");
    expect(formatElapsed(750_000)).toBe("12m 30s");
    expect(formatElapsed(15_120_000)).toBe("4h 12m");
    expect(formatElapsed(200_000_000)).toBe("2d 7h");
  });
});
