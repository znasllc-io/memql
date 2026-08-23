// The Nexus section: where it lives, how you reach a goal, and what a page
// says when there is nothing to draw.
//
// These render the WHOLE app (AppRoutes inside the shell), not the pages in
// isolation, because half of what is under test is the wiring: the rail
// group, the splat mount, the tab strip, and the deep links -- each of which
// works fine in a component test and can still be absent from the product.

import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { emptyGoal } from "../src/nexus/scene/fixtures";
import { OWNER_ID, callsNamed, nexusHarness, renderNexus } from "./support/nexusHarness";

function goalRow(id: string, goal: string, status: string, createdAt: string): Row {
  return { id, goal, status, requestedBy: OWNER_ID, createdAt, kind: "userGoal" };
}

describe("the Nexus section", () => {
  it("has a rail group of its own", async () => {
    renderNexus(nexusHarness(), "/nexus/plan-spring");
    const rail = within(screen.getByRole("navigation", { name: /portal sections/i }));
    await waitFor(() => expect(rail.getByRole("heading", { name: /nexus/i })).toBeTruthy());
    expect(rail.getByRole("link", { name: "Goals" }).getAttribute("href")).toContain("/nexus");
  });

  it("opens a goal by id, from a cold load", async () => {
    const h = nexusHarness();
    renderNexus(h, "/nexus/plan-spring");
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: /build a spring catalog/i })).toBeTruthy(),
    );
    // The seed asked for exactly the reads the design names.
    for (const construct of [
      "planById",
      "tasksForPlan",
      "agentsForPlan",
      "artifactsForPlan",
      "authoringBundleForPlan",
    ]) {
      expect(`${construct}: ${callsNamed(h.calls, construct).length > 0}`).toBe(`${construct}: true`);
    }
  });

  it("carries the three surfaces as a tab strip, each deep-linkable", async () => {
    renderNexus(nexusHarness(), "/nexus/plan-spring");
    const tabs = within(await screen.findByRole("navigation", { name: "Nexus" }));
    for (const label of ["Map", "Constructs", "Replay"]) {
      expect(tabs.getByRole("link", { name: label })).toBeTruthy();
    }
    expect(tabs.getByRole("link", { name: "Constructs" }).getAttribute("href")).toBe(
      "/nexus/plan-spring/constructs",
    );
    expect(tabs.getByRole("link", { name: "Replay" }).getAttribute("href")).toBe(
      "/nexus/plan-spring/replay",
    );
  });

  it("lands directly on Constructs and on Replay", async () => {
    renderNexus(nexusHarness(), "/nexus/plan-spring/constructs");
    await waitFor(() => expect(screen.getByText(/spring catalog capture/i)).toBeTruthy());

    renderNexus(nexusHarness(), "/nexus/plan-spring/replay");
    await waitFor(() => expect(screen.getAllByRole("listbox", { name: /goal events/i }).length).toBeGreaterThan(0));
  });

  it("opens a node's detail from its own URL", async () => {
    renderNexus(nexusHarness(), "/nexus/plan-spring/node/task~step-normalise");
    // The dialog performs a FRESH read of the row the node names -- the
    // latest attempt, not the first (world.ts's node-vs-row split).
    await waitFor(() => expect(screen.getByText("shape-normalise-a2")).toBeTruthy());
    expect(screen.getByRole("heading", { name: /row detail/i })).toBeTruthy();
  });

  it("redirects /nexus to the caller's most recent goal, running first", async () => {
    const h = nexusHarness({
      goals: [
        goalRow("plan-old", "An older goal", "succeeded", "2026-08-01T09:00:00Z"),
        goalRow("plan-spring", "Build a spring catalog", "running", "2026-07-01T09:00:00Z"),
      ],
    });
    renderNexus(h, "/nexus");
    // The RUNNING goal wins even though it is older -- that is the ordering
    // the design asks for, and the trap is that a plain newest-first sort
    // would pick the other one.
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: /build a spring catalog/i })).toBeTruthy(),
    );
  });

  it("says where goals come from when there are none", async () => {
    const h = nexusHarness({ goals: [] });
    renderNexus(h, "/nexus");
    await waitFor(() => expect(screen.getByText(/you have no goals yet/i)).toBeTruthy());
    // ...and does NOT offer a button that would do nothing: this console
    // reads goals, it does not start them.
    expect(screen.queryByRole("button", { name: /create/i })).toBeNull();
  });

  it("moves between goals through the picker, by URL", async () => {
    const h = nexusHarness({
      goals: [
        goalRow("plan-spring", "Build a spring catalog", "running", "2026-08-20T09:00:00Z"),
        goalRow("plan-other", "Another goal", "succeeded", "2026-08-19T09:00:00Z"),
      ],
    });
    renderNexus(h, "/nexus/plan-spring");
    const picker = await screen.findByRole("combobox");
    fireEvent.change(picker, { target: { value: "plan-other" } });
    // The picker navigates; a goal is a URL, not component state.
    await waitFor(() => expect(callsNamed(h.calls, "planById").some((c) => c.includes("plan-other"))).toBe(true));
  });

  it("draws a goal with no tasks yet rather than an error", async () => {
    const h = nexusHarness({ world: emptyGoal() });
    renderNexus(h, "/nexus/plan-empty");
    await waitFor(() => expect(screen.getByRole("heading", { name: /summarise last quarter/i })).toBeTruthy());
    expect(screen.getByText(/no tasks yet/i)).toBeTruthy();
  });

  it("says a goal that is not yours is not yours, rather than 404ing it", async () => {
    const h = nexusHarness();
    const original = (h.query as { executeNamed: ReturnType<typeof vi.fn> }).executeNamed;
    (h.query as { executeNamed: ReturnType<typeof vi.fn> }).executeNamed = vi.fn(
      async (name: string, call: string) => {
        if (name === "planById") {
          return new Result({
            bundle: {
              nodes: [
                {
                  id: "plan-spring",
                  payload: { id: "plan-spring", goal: "Someone else's goal", requestedBy: "another-user", status: "running" },
                },
              ],
            },
          });
        }
        return original(name, call);
      },
    );
    renderNexus(h, "/nexus/plan-spring");
    await waitFor(() => expect(screen.getByText(/belongs to someone else/i)).toBeTruthy());
    // The map is not drawn, and the goal's text is not leaked into the page.
    expect(screen.queryByText(/someone else's goal/i)).toBeNull();
  });

  it("says a goal that does not exist does not exist", async () => {
    const h = nexusHarness();
    const original = (h.query as { executeNamed: ReturnType<typeof vi.fn> }).executeNamed;
    (h.query as { executeNamed: ReturnType<typeof vi.fn> }).executeNamed = vi.fn(
      async (name: string, call: string) =>
        name === "planById" ? new Result({ bundle: { nodes: [] } }) : original(name, call),
    );
    renderNexus(h, "/nexus/plan-gone");
    await waitFor(() => expect(screen.getByText(/no goal with that id/i)).toBeTruthy());
  });
});
