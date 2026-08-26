// The Constructs page: what a goal built, read rather than watched.
//
// The page's whole job is to be HONEST about a capability that is off by
// default -- runtime authoring is gated by MEMQL_AUTHORING_CAPTURE_MODE and
// most clusters never turn it on -- so the banner gets as much attention here
// as the two verbs do. An empty page reads as "this goal built nothing",
// which is a claim about the goal rather than about the cluster.

import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";

import { springCatalogGoal } from "../src/nexus/scene/fixtures";
import { callsNamed, nexusHarness, renderNexus } from "./support/nexusHarness";

const PATH = "/nexus/plan-spring/constructs";

describe("the Constructs page", () => {
  it("renders the bundle with its status and both reports", async () => {
    renderNexus(nexusHarness(), PATH);
    await waitFor(() => expect(screen.getByText("Spring catalog capture")).toBeTruthy());
    // "active" is the bundle's status AND two of its constructs' -- scoped to
    // the bundle band so the assertion is about the bundle.
    const bundleBand = screen.getByText("Spring catalog capture").closest("div");
    expect(within(bundleBand as HTMLElement).getByText("active")).toBeTruthy();
    expect(screen.getByText("Validation")).toBeTruthy();
    expect(screen.getByText("Dry run")).toBeTruthy();
  });

  it("renders each construct with its kind, name, status and source", async () => {
    renderNexus(nexusHarness(), PATH);
    // Each name appears twice by design: once in the construct list and once
    // as a node in the dependency graph, which is drawn from the same rows.
    await waitFor(() => expect(screen.getAllByText("supplierSheet").length).toBeGreaterThan(0));
    expect(screen.getAllByText("sheetsForSeason").length).toBeGreaterThan(0);
    expect(screen.getAllByText("onSheetLanded").length).toBeGreaterThan(0);
    // The source, verbatim and read-only -- editing a construct is the
    // authoring loop's job, not a text area that could not validate itself.
    expect(screen.getByText(/concept supplierSheet \{/)).toBeTruthy();
    expect(screen.queryByRole("textbox")).toBeNull();
  });

  it("draws the dependency edges as a flat graph, from the same rows", async () => {
    renderNexus(nexusHarness(), PATH);
    const graph = await screen.findByRole("img", { name: /dependency graph/i });
    // Two edges in the fixture, and both endpoints are in-bundle constructs.
    expect(graph.getAttribute("aria-label")).toContain("2 edges");
    expect(within(graph).getByText("supplierSheet")).toBeTruthy();
  });

  it("stages a draft construct behind a confirmation, and calls the mutation", async () => {
    const world = springCatalogGoal();
    // One draft construct, so the Stage button is offered exactly once.
    const h = nexusHarness({
      world: {
        ...world,
        constructs: world.constructs.map((construct) =>
          construct.name === "onSheetLanded" ? { ...construct, status: "draft" } : construct,
        ),
      },
    });
    renderNexus(h, PATH);

    // A Stage button per construct; only the draft one is enabled, which is
    // itself the assertion that the page offers the verb where it applies.
    await waitFor(() => expect(screen.getAllByRole("button", { name: "Stage" }).length).toBe(3));
    const enabled = screen
      .getAllByRole("button", { name: "Stage" })
      .filter((button) => !button.hasAttribute("disabled"));
    expect(enabled).toHaveLength(1);
    fireEvent.click(enabled[0]!);

    // Nothing has been written yet -- the confirmation is the gate.
    expect(callsNamed(h.calls, "setConstructStatus")).toHaveLength(0);
    expect(screen.getByText(/out of draft/i)).toBeTruthy();

    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Stage" }));
    await waitFor(() => expect(callsNamed(h.calls, "setConstructStatus")).toHaveLength(1));
    expect(callsNamed(h.calls, "setConstructStatus")[0]).toContain('status: "staged"');
    expect(callsNamed(h.calls, "setConstructStatus")[0]).toContain("construct-onSheetLanded");
  });

  it("promotes the bundle behind a confirmation that says what promotion does", async () => {
    const world = springCatalogGoal();
    const h = nexusHarness({
      world: { ...world, bundle: world.bundle === null ? null : { ...world.bundle, status: "dryRunPassed" } },
    });
    renderNexus(h, PATH);

    fireEvent.click(await screen.findByRole("button", { name: "Promote" }));
    expect(screen.getByText(/claimed for everyone/i)).toBeTruthy();
    expect(callsNamed(h.calls, "activateAuthoringBundle")).toHaveLength(0);

    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Promote" }));
    await waitFor(() => expect(callsNamed(h.calls, "activateAuthoringBundle")).toHaveLength(1));
    expect(callsNamed(h.calls, "activateAuthoringBundle")[0]).toContain("bundle-spring");
  });

  it("does not offer Promote on a bundle that is already active", async () => {
    renderNexus(nexusHarness(), PATH);
    const promote = await screen.findByRole("button", { name: "Promote" });
    expect(promote.hasAttribute("disabled")).toBe(true);
  });

  it("says the write was refused rather than pretending it landed", async () => {
    const world = springCatalogGoal();
    const h = nexusHarness({
      world: { ...world, bundle: world.bundle === null ? null : { ...world.bundle, status: "draft" } },
      failWrite: "activateAuthoringBundle",
    });
    renderNexus(h, PATH);
    fireEvent.click(await screen.findByRole("button", { name: "Promote" }));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Promote" }));
    // The plain sentence for everybody; the cluster's own words behind the
    // owner-only disclosure (memql#4653). This fixture is an owner, so both
    // are in the tree.
    await waitFor(() => expect(screen.getByText(/That write was refused/)).toBeTruthy());
    expect(screen.getByText(/Nothing was saved/)).toBeTruthy();
    await waitFor(() => expect(screen.getByText("Technical details")).toBeTruthy());
  });

  it("says authoring capture is off when a SUCCEEDED goal left no bundle", async () => {
    const world = springCatalogGoal();
    const h = nexusHarness({ world: { ...world, bundle: null, constructs: [], edges: [] } });
    renderNexus(h, PATH);
    await waitFor(() => expect(screen.getByText(/authoring capture is off/i)).toBeTruthy());
    // And says plainly that the goal is not at fault.
    expect(screen.getByText(/nothing here is a failure/i)).toBeTruthy();
  });

  it("does NOT claim capture is off for a goal that is still running", async () => {
    // The inference is a signature, not a certainty, and it only holds for a
    // goal that finished. A running goal has simply not got there yet.
    const world = springCatalogGoal();
    const h = nexusHarness({
      world: {
        ...world,
        plan: world.plan === null ? null : { ...world.plan, status: "running", completedAt: "" },
        bundle: null,
        constructs: [],
        edges: [],
      },
    });
    renderNexus(h, PATH);
    await waitFor(() => expect(screen.getByText(/no bundle yet/i)).toBeTruthy());
    expect(screen.queryByText(/authoring capture is off/i)).toBeNull();
  });
});
