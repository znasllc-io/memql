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
import { ACCESS, OWNER_ID, callsNamed, nexusHarness, renderNexus } from "./support/nexusHarness";

function goalRow(id: string, goal: string, status: string, createdAt: string): Row {
  return { id, goal, status, requestedBy: OWNER_ID, createdAt, kind: "userGoal" };
}

describe("the Nexus section", () => {
  it("is a rail destination of its own", async () => {
    renderNexus(nexusHarness(), "/nexus/plan-spring");
    const rail = within(screen.getByRole("navigation", { name: /portal sections/i }));
    // A flat row since memql#4655, not a caption with rows under it. Nexus
    // keeps its per-goal tabs (Map / Constructs / Replay) INSIDE a goal, which
    // is a different altitude from an area's facets -- so Goals is simply
    // where the row lands rather than a row of its own.
    await waitFor(() =>
      expect(rail.getByRole("link", { name: "Nexus" }).getAttribute("href")).toBe("/nexus"),
    );
    expect(rail.queryByRole("heading", { name: /nexus/i })).toBeNull();
    expect(rail.queryByRole("link", { name: "Goals" })).toBeNull();
    // Agents left the rail with the caption: it is a card in the Views
    // gallery now, at the same URL it always had.
    expect(rail.queryByRole("link", { name: "Agents" })).toBeNull();
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
    // BOTH assertions WAIT, and the second one is the reason this comment
    // exists. It used to be a synchronous getByRole, which reads as safe --
    // surely the dialog is up by the time its content is -- and is not.
    //
    // NodeDetail returns null until the async scene contains the node, so the
    // heading and the map's own label for the same row arrive in DIFFERENT
    // commits. Waiting on the row text and then asserting the heading is
    // waiting on one thing to assert another; whether it holds depends on
    // which commit lands first, which depends on how fast the box is.
    // Measured: queryAllByText for that row returns 1 or 3 depending on the
    // run, so the text genuinely has more than one source.
    //
    // It passed locally and in PR CI and failed in the merge queue, whose
    // runner is slower -- the shape that gets re-run rather than fixed.
    await waitFor(() => expect(screen.getByText("shape-normalise-a2")).toBeTruthy());
    expect(await screen.findByRole("heading", { name: /row detail/i })).toBeTruthy();
  });

  it("lists the caller's goals at /nexus, running first, statuses shown", async () => {
    const h = nexusHarness({
      goals: [
        goalRow("plan-old", "An older goal", "failed", "2026-08-01T09:00:00Z"),
        goalRow("plan-spring", "Build a spring catalog", "running", "2026-07-01T09:00:00Z"),
      ],
    });
    renderNexus(h, "/nexus");
    const list = within(await screen.findByRole("list", { name: /your goals/i }));
    const [first, second] = list.getAllByRole("listitem");
    if (first === undefined || second === undefined) throw new Error("expected two goal rows");
    // The RUNNING goal is pinned on top even though it is older -- the same
    // ordering the redirect used to encode, now visible instead of implied.
    expect(within(first).getByRole("link", { name: /build a spring catalog/i })).toBeTruthy();
    // ...and the older goal SAYS it failed. Three failed goals all reading as
    // "planning" in a status-less <select> is the incident this page is for.
    expect(within(second).getByText("failed")).toBeTruthy();
    expect(within(second).getByRole("link", { name: /an older goal/i }).getAttribute("href")).toBe(
      "/nexus/plan-old",
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

  // memql#4581. THE FIXTURES ABOVE ARE WHY THIS SHIPPED: every one of them
  // uses a BARE id on both sides, so the ownership check has never once been
  // asked the question production actually asks it.
  //
  // In production the two sides arrive in DIFFERENT shapes. A row's
  // requestedBy is bare-ified on egress (WireBareifyData, query data), while
  // MyAccess.userId is a proto scalar the bare-ifier never walks and stays
  // canonical. A raw `!==` between them is always true, so a goal the caller
  // had created seconds earlier reported "belongs to someone else".
  it("draws a goal when the caller's id is canonical and the row's is bare", async () => {
    const h = nexusHarness();
    (h.query as { getMyAccess: ReturnType<typeof vi.fn> }).getMyAccess = vi.fn(async () => ({
      ...ACCESS,
      userId: `v1:identity:user:${OWNER_ID}`,
    }));
    renderNexus(h, "/nexus/plan-spring");

    // The goal is drawn: same owner, spelled two ways.
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: /build a spring catalog/i })).toBeTruthy(),
    );
    expect(screen.queryByText(/belongs to someone else/i)).toBeNull();
  });

  // The other half of the same seam: a genuinely different owner must STILL be
  // refused when the shapes differ, or the fix would have turned an
  // over-refusal into a leak.
  it("still refuses a goal owned by someone else when the shapes differ", async () => {
    const h = nexusHarness();
    (h.query as { getMyAccess: ReturnType<typeof vi.fn> }).getMyAccess = vi.fn(async () => ({
      ...ACCESS,
      userId: "v1:identity:user:someone-else",
    }));
    renderNexus(h, "/nexus/plan-spring");
    await waitFor(() => expect(screen.getByText(/belongs to someone else/i)).toBeTruthy());
    expect(screen.queryByRole("heading", { name: /build a spring catalog/i })).toBeNull();
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
