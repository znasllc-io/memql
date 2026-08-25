// Starting a goal from the console (memql#4528).
//
// Rendered through the WHOLE app, like the rest of the Nexus tests, because
// half of what is under test is wiring: the action is in the goal chrome AND
// in the empty state, and either can work in isolation while being absent
// from the product.
//
// The harness fakes at `executeNamed`, so the assertions below read the REAL
// composed MemQL call string rather than a hand-typed copy of it. That is the
// difference between proving the page calls createPlan and proving it calls
// createPlan with arguments the engine would accept.

import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { CONSOLE_PARTITION_ID } from "../src/nexus/NewGoalDialog";
import { OWNER_ID, callsNamed, nexusHarness, renderNexus } from "./support/nexusHarness";

function openDialog(): void {
  fireEvent.click(screen.getByRole("button", { name: "New goal" }));
}

function goalBox(): HTMLElement {
  return screen.getByRole("textbox", { name: /goal/i });
}

function startButton(): HTMLElement {
  return screen.getByRole("button", { name: "Start the goal" });
}

function location(): string {
  return screen.getByTestId("location").textContent ?? "";
}

describe("starting a goal from the console", () => {
  it("offers the verb in the goal chrome, beside the picker", async () => {
    renderNexus(nexusHarness(), "/nexus/plan-spring");
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: /build a spring catalog/i })).toBeTruthy(),
    );
    // The picker is the LIST; this is the VERB. Both, in the same header.
    expect(screen.getByRole("combobox", { name: /goal/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: "New goal" })).toBeTruthy();
  });

  it("offers the same verb from the empty state, and no longer says the console cannot", async () => {
    renderNexus(nexusHarness({ goals: [] }), "/nexus");
    await waitFor(() => expect(screen.getByText(/you have no goals yet/i)).toBeTruthy());

    // The reversed decision is gone from the copy, not merely outvoted by a
    // button sitting next to it.
    expect(screen.queryByText(/it does not start them/i)).toBeNull();
    // And the new copy says what happens next, honestly: the planner sizes
    // the goal before anything is spent.
    expect(screen.getByText(/sizes a goal before it spends anything/i)).toBeTruthy();
    expect(screen.getByRole("button", { name: "New goal" })).toBeTruthy();
  });

  it("writes createPlan with the arguments the engine requires, then opens the goal", async () => {
    const h = nexusHarness();
    renderNexus(h, "/nexus/plan-spring");
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: /build a spring catalog/i })).toBeTruthy(),
    );

    openDialog();
    fireEvent.change(goalBox(), { target: { value: "  Draft the Q3 board update  " } });
    fireEvent.click(startButton());

    await waitFor(() => expect(callsNamed(h.calls, "createPlan")).toHaveLength(1));
    const call = callsNamed(h.calls, "createPlan")[0] ?? "";

    // Every @required argument, in the composed call the engine would parse.
    expect(call).toContain('kind: "userGoal"');
    // Trimmed: the leading and trailing spaces the operator typed are not
    // part of the goal.
    expect(call).toContain('goal: "Draft the Q3 board update"');
    // requestedBy is the CALLER's own user id -- it is what makes the goal
    // theirs, and Nexus filters the map on it.
    expect(call).toContain(`requestedBy: "${OWNER_ID}"`);
    expect(call).toContain(`partitionId: "${CONSOLE_PARTITION_ID}"`);
    expect(call).toContain("input: {}");
    expect(call).toMatch(/planId: "[0-9a-f]{16}"/);

    // NO startPlan. This button starts the PLANNING lifecycle; whether
    // anything is spent is the estimate / approval / budget gates' decision
    // (docs/public/ai/llm-cost-control.md).
    expect(callsNamed(h.calls, "startPlan")).toHaveLength(0);

    // It lands on the new goal's own map, at the id it minted -- so the
    // operator watches it materialize rather than going to find it.
    const planId = /planId: "([0-9a-f]{16})"/.exec(call)?.[1] ?? "";
    expect(planId).not.toBe("");
    await waitFor(() => expect(location()).toBe(`/nexus/${planId}`));
  });

  it("keeps the dialog open and says why when the cluster refuses", async () => {
    const h = nexusHarness({ failWrite: "createPlan" });
    renderNexus(h, "/nexus/plan-spring");
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: /build a spring catalog/i })).toBeTruthy(),
    );

    openDialog();
    fireEvent.change(goalBox(), { target: { value: "Something the cluster will refuse" } });
    fireEvent.click(startButton());

    // Inline, on the field, and the dialog stays up with the text intact so
    // the operator is not made to retype it.
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("refused"));
    expect(startButton()).toBeTruthy();
    expect((goalBox() as HTMLTextAreaElement).value).toBe("Something the cluster will refuse");
    // Still on the goal it started from: navigation is the SUCCESS path only.
    expect(location()).toBe("/nexus/plan-spring");
  });

  it("holds the verb back until there is a goal to start", async () => {
    renderNexus(nexusHarness(), "/nexus/plan-spring");
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: /build a spring catalog/i })).toBeTruthy(),
    );
    openDialog();
    // Empty, and whitespace, are both nothing to start.
    expect((startButton() as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(goalBox(), { target: { value: "   " } });
    expect((startButton() as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(goalBox(), { target: { value: "Real goal" } });
    expect((startButton() as HTMLButtonElement).disabled).toBe(false);
  });

  it("keeps the picker live, so a goal started anywhere appears without a reload", async () => {
    const first: Row = {
      id: "plan-spring",
      goal: "Build a spring catalog",
      status: "running",
      requestedBy: OWNER_ID,
      createdAt: "2026-08-25T10:00:00Z",
      kind: "userGoal",
    };
    const h = nexusHarness({ goals: [first] });
    renderNexus(h, "/nexus/plan-spring");
    await waitFor(() => expect(screen.getByRole("combobox", { name: /goal/i })).toBeTruthy());
    await waitFor(() => expect(callsNamed(h.calls, "plansForUser").length).toBeGreaterThan(0));
    const before = callsNamed(h.calls, "plansForUser").length;

    // A CDC arrival on the plan concept re-runs the OWNER-SCOPED read; the
    // event's own payload is never spliced into the list.
    h.emit("v1:planner:plan", {
      topic: "graph.node.created.v1:planner:plan",
      kind: "created",
      payload: { id: "plan-new" },
    } as never);

    await waitFor(() =>
      expect(callsNamed(h.calls, "plansForUser").length).toBeGreaterThan(before),
    );
  });
});
