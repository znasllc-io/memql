// The Run affordance: a queued goal's startPlan click, in the two places it
// renders -- the goal header and the goals list -- and the two places it must
// NOT render. `queued` is "planning complete, tasks emitted, waiting for a
// human to click Run" (dsl/planner/concepts.memql); the console shipping no
// such click is why every finished planning pass looked like a hang.

import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { springCatalogGoal } from "../src/nexus/scene/fixtures";
import type { GoalWorld } from "../src/nexus/scene/world";
import { callsNamed, nexusHarness, OWNER_ID, renderNexus } from "./support/nexusHarness";

function goalRow(id: string, goal: string, status: string, createdAt: string): Row {
  return { id, goal, status, requestedBy: OWNER_ID, createdAt, kind: "userGoal" };
}

// The spring-catalog world with its plan parked at `queued`, which is the
// state the fixture never carries on its own.
function queuedWorld(): GoalWorld {
  const base = springCatalogGoal();
  if (base.plan === null) throw new Error("fixture carries no plan");
  return { ...base, plan: { ...base.plan, status: "queued" } };
}

describe("running a queued goal", () => {
  it("offers Run in the goal header at queued, and startPlan names the plan", async () => {
    const h = nexusHarness({ world: queuedWorld() });
    renderNexus(h, "/nexus/plan-spring");
    const run = await screen.findByRole("button", { name: "Run" });
    fireEvent.click(run);
    await waitFor(() =>
      expect(callsNamed(h.calls, "startPlan").some((call) => call.includes("plan-spring"))).toBe(
        true,
      ),
    );
  });

  it("offers Run on the goals list row at queued", async () => {
    const h = nexusHarness({
      goals: [
        goalRow("plan-q", "Ship the thing", "queued", "2026-08-20T09:00:00Z"),
        goalRow("plan-r", "Another goal", "succeeded", "2026-08-19T09:00:00Z"),
      ],
    });
    renderNexus(h, "/nexus");
    const list = within(await screen.findByRole("list", { name: /your goals/i }));
    fireEvent.click(list.getByRole("button", { name: "Run" }));
    await waitFor(() =>
      expect(callsNamed(h.calls, "startPlan").some((call) => call.includes("plan-q"))).toBe(true),
    );
    // The succeeded row carries no Run -- one button on the page, on the one
    // row that can use it.
    expect(list.getAllByRole("button", { name: "Run" }).length).toBe(1);
  });

  it("does not offer Run while the goal is running", async () => {
    // The fixture's own plan is running; the header shows the badge alone.
    const h = nexusHarness();
    renderNexus(h, "/nexus/plan-spring");
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: /build a spring catalog/i })).toBeTruthy(),
    );
    expect(screen.queryByRole("button", { name: "Run" })).toBeNull();
  });

  it("says why when the cluster refuses the start", async () => {
    const h = nexusHarness({ world: queuedWorld(), failWrite: "startPlan" });
    renderNexus(h, "/nexus/plan-spring");
    fireEvent.click(await screen.findByRole("button", { name: "Run" }));
    // The refusal lands beside the button rather than vanishing: an operator
    // who clicked Run and saw nothing change would click it again.
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("refused"));
  });
});
