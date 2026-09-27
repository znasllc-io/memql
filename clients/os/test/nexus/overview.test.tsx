import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

// Nexus's Overview (epic memql#5414): measured figures from the reads Nexus
// already makes, and the automation library by EFFECTIVE reuse label -- with
// absence kept absent and loading drawn as the shape of what is coming.

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { NexusApp } = await import("../../src/apps/nexus/NexusApp");
const { LocalNexusSettingsStore } = await import("../../src/apps/nexus/settings");
const { approvalRow, constructRow, fakeConnection, goalRow, rowsResult, runRow, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

function mount(connection: Conn) {
  h.connection = connection;
  const navigate = vi.fn();
  render(
    withSession(
      <NexusApp
        sectionId="overview"
        navigate={navigate}
        askContext={() => {}}
        store={new LocalNexusSettingsStore(null)}
      />,
    ),
  );
  return { navigate };
}

/** One overview figure's own cell: its value and its quiet line. (The info
 *  control names every figure too, so the cell is found by its own mark.) */
function metric(label: string): HTMLElement {
  const cell = document.querySelector(`[data-overview-metric="${label}"]`);
  if (!(cell instanceof HTMLElement)) throw new Error(`no metric ${label}`);
  return cell;
}

function value(label: string): string {
  return (metric(label).querySelector("dd")?.textContent ?? "").trim();
}

describe("the Overview's figures", () => {
  it("counts what is open, moving and waiting from the feeds the app already holds", async () => {
    mount(
      fakeConnection({
        goals: [
          goalRow({ id: "g1", status: "active" }),
          goalRow({ id: "g2", status: "open" }),
          goalRow({ id: "g3", status: "closed" }),
        ],
        runs: [runRow({ id: "r1", status: "running" }), runRow({ id: "r2", status: "waiting" }), runRow({ id: "r3" })],
        approvals: [approvalRow({ id: "a1" })],
      }),
    );
    await waitFor(() => expect(value("Goals open")).toBe("2"));
    expect(value("Runs in flight")).toBe("2");
    expect(value("Approvals waiting")).toBe("1");
  });

  it("puts reusable against goal-specific, and a person's own label wins", async () => {
    mount(
      fakeConnection({
        constructs: [
          constructRow({ id: "c1", name: "a", reuse: "reusable" }),
          constructRow({ id: "c2", name: "b", reuse: "goalSpecific", reuseOverride: { label: "reusable", version: 1 } }),
          constructRow({ id: "c3", name: "c", reuse: "goalSpecific" }),
          constructRow({ id: "c4", name: "d", reuse: "accountSpecific" }),
          constructRow({ id: "c5", name: "e" }),
        ],
        learnedProcedures: [constructRow({ id: "p1", name: "procedureA", targetNamespace: "procedure", catalogued: false, reuse: "goalSpecific" })],
      }),
    );
    await waitFor(() => expect(value("Reusable to goal-specific")).toBe("2 to 2"));
    expect(within(metric("Reusable to goal-specific")).getByText("1 for one account, 1 not yet labelled")).toBeTruthy();
    const breakdown = screen.getByRole("region", { name: "Your automations, by reuse" });
    expect(within(breakdown).getByText("Reusable")).toBeTruthy();
    expect(within(breakdown).getByText("Not yet labelled")).toBeTruthy();
  });

  it("keeps the ratio ABSENT until something is labelled -- never 0 to 0", async () => {
    mount(fakeConnection({ constructs: [constructRow({ id: "c1", name: "a" }), constructRow({ id: "c2", name: "b" })] }));
    await waitFor(() => expect(within(metric("Reusable to goal-specific")).getByText("2 not yet labelled")).toBeTruthy());
    expect(value("Reusable to goal-specific")).toBe("—");
    expect(screen.queryByText(/0 to 0/)).toBeNull();
    // Unlabelled is its own segment, never folded into "for one goal".
    const breakdown = screen.getByRole("region", { name: "Your automations, by reuse" });
    expect(within(breakdown).queryByText("For one goal")).toBeNull();
  });

  it("draws the shape of a figure still being read, never a caption and never a zero", async () => {
    const conn = fakeConnection();
    conn.query.cataloguedConstructsForOwner.mockImplementation(() => new Promise(() => {}) as never);
    mount(conn);
    await waitFor(() => expect(value("Goals open")).toBe("0"));
    const cell = metric("Reusable to goal-specific");
    expect(cell.querySelector('[aria-busy="true"]')).toBeTruthy();
    expect(value("Reusable to goal-specific")).not.toMatch(/\d/);
    // Loading is ANNOUNCED, never painted: every such word is screen-reader-only.
    const loading = screen.queryAllByText(/^Loading/);
    expect(loading.length).toBeGreaterThan(0);
    expect(loading.every((el) => el.classList.contains("os-sr-only"))).toBe(true);
  });

  it("says a refused library read in its own words, and leaves the figure absent", async () => {
    mount(fakeConnection({ constructs: new Error("permission denied: v1:authoring:construct") }));
    expect(await screen.findByText("Your automations could not be read.")).toBeTruthy();
    expect(screen.getByText("permission denied: v1:authoring:construct")).toBeTruthy();
    expect(value("Reusable to goal-specific")).toBe("—");
  });

  it("says there is nothing yet when there is nothing at all, and points at Goals", async () => {
    const { navigate } = mount(fakeConnection());
    fireEvent.click(await screen.findByRole("button", { name: "Open goals" }));
    expect(screen.getByText("Nothing to summarise yet")).toBeTruthy();
    expect(navigate).toHaveBeenCalledWith("goals");
  });

  it("states the reuse threshold as the cluster's own policy row says it", async () => {
    const conn = fakeConnection();
    conn.query.feedbackPolicyCurrent.mockImplementation(async () =>
      rowsResult([{ id: "v1:work:feedbackPolicy:primary", validateAnswers: true, reusableAfterSignatures: 3 }]),
    );
    mount(conn);
    expect(await screen.findByText(/An automation is reusable once 3 different kinds of goal have used it/)).toBeTruthy();
    expect(conn.query.feedbackPolicyCurrent).toHaveBeenCalled();
  });

  it("says the rule without a number when the policy cannot be read", async () => {
    const conn = fakeConnection();
    conn.query.feedbackPolicyCurrent.mockImplementation(async () => {
      throw new Error("rank floor: reader");
    });
    mount(conn);
    expect(await screen.findByText(/An automation is reusable once enough different kinds of goal have used it/)).toBeTruthy();
  });
});
