// The receipt (design D8): what a goal cost and what it left behind.
//
// The arithmetic is pure and asserted directly; the card is asserted on the
// page, including the case the design is specific about -- it must be present
// in Replay AT THE MOMENT OF SUCCESS, not only at the end of the timeline.
//
// The distinction this file is most careful about is ABSENT versus ZERO. A
// plan that spent nothing on its subscription and a cluster that does not
// record what the subscription covered are different facts, and a card that
// printed "0" for the second would be inventing a measurement.

import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";

import { formatElapsed, receipt } from "../src/nexus/scene/receipt";
import { scene } from "../src/nexus/scene/scene";
import {
  MOMENT,
  cancelledGoal,
  emptyGoal,
  failedGoal,
  springCatalogGoal,
} from "../src/nexus/scene/fixtures";
import { nexusHarness, renderNexus } from "./support/nexusHarness";

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

describe("the completion card on the page", () => {
  it("materializes under the goal when it succeeds, and dismisses", async () => {
    renderNexus(nexusHarness(), "/nexus/plan-spring");
    // Scoped to the receipt's own region: "Constructs" is also a tab, and an
    // unscoped match would be reading the tab strip.
    const card = within(await screen.findByRole("region", { name: /goal receipt/i }));
    expect(card.getByText("Goal reached")).toBeTruthy();
    expect(card.getByText("Tasks")).toBeTruthy();
    expect(card.getByText("Steps")).toBeTruthy();
    expect(card.getByText("Agents raised")).toBeTruthy();
    expect(card.getByText("Constructs")).toBeTruthy();
    expect(card.getByText("184,320")).toBeTruthy();
    // Absent, not zero -- the fixture's engine does not carry the field.
    expect(card.queryByText(/covered by subscription/i)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    await waitFor(() => expect(screen.queryByText("Goal reached")).toBeNull());
  });

  it("gives a failed goal the same card, with the failure", async () => {
    const h = nexusHarness({ world: failedGoal() });
    renderNexus(h, "/nexus/plan-failed");
    await waitFor(() => expect(screen.getByText("Goal failed")).toBeTruthy());
    expect(screen.getByText(/supplier feed stopped responding/i)).toBeTruthy();
    expect(screen.getByText(/last running/i)).toBeTruthy();
    // The cost is still there.
    expect(screen.getByText("Tokens")).toBeTruthy();
  });

  it("says a cancelled goal was cancelled, and by whom", async () => {
    const h = nexusHarness({ world: cancelledGoal() });
    renderNexus(h, "/nexus/plan-cancelled");
    await waitFor(() => expect(screen.getByText("Goal cancelled")).toBeTruthy());
    expect(screen.getByText(/cancelled by/i)).toBeTruthy();
  });

  it("is present in Replay at the moment of success and absent before it", async () => {
    const world = springCatalogGoal();
    // Before the goal completes there is no receipt, because there is no
    // outcome yet -- scrubbing back past the completion takes the card away.
    expect(receipt(scene(world, MOMENT(20)))).toBeNull();
    expect(receipt(scene(world, MOMENT(21)))).not.toBeNull();

    renderNexus(nexusHarness(), "/nexus/plan-spring/replay");
    // "Goal reached" is also an entry in the event list, so the assertion is
    // on the receipt's region rather than on the words.
    await waitFor(() => expect(screen.getByRole("region", { name: /goal receipt/i })).toBeTruthy());

    // Scrub back before the completion: the card goes, because at that moment
    // the goal had no outcome to report.
    const slider = screen.getByRole("slider", { name: /replay position/i });
    fireEvent.change(slider, { target: { value: "2" } });
    await waitFor(() => expect(screen.queryByRole("region", { name: /goal receipt/i })).toBeNull());
  });
});
