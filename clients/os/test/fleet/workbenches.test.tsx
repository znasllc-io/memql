import { act, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { WorkbenchesSection } = await import("../../src/apps/fleet/workbenches/WorkbenchesSection");
const { fakeConnection, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

async function click(el: Element) {
  await act(async () => {
    (el as HTMLElement).click();
  });
}

function mount(connection: Conn) {
  h.connection = connection;
  return render(withSession(<WorkbenchesSection />));
}

const LIVE_A = {
  id: "v1:workbench:workspace:a",
  planId: "v1:planner:plan:1",
  nodeId: "workbench-0",
  status: "provisioned",
  storageRoot: "/var/lib/memql/workbenches/plan-1",
  createdAt: "2026-08-29T09:00:00Z",
  lastUsedAt: "2026-08-30T11:59:50Z",
};

const LIVE_B = {
  id: "v1:workbench:workspace:b",
  planId: "v1:planner:plan:2",
  nodeId: "workbench-1",
  status: "provisioned",
  storageRoot: "/var/lib/memql/workbenches/plan-2",
  createdAt: "2026-08-30T09:00:00Z",
};

const LOST = {
  id: "v1:workbench:workspace:lost",
  planId: "v1:planner:plan:3",
  nodeId: "workbench-9",
  status: "released",
  storageRoot: "/var/lib/memql/workbenches/plan-3",
  createdAt: "2026-08-28T09:00:00Z",
  releasedAt: "2026-08-29T09:00:00Z",
  releasedReason: "node_lost",
};

beforeEach(() => {
  h.connection = null;
});

describe("the workbenches section", () => {
  it("groups live workspaces by the replica whose disk holds them", async () => {
    mount(
      fakeConnection({
        myWorkspaces: [LIVE_A, LIVE_B],
        clusterNodes: [
          { id: "workbench-0", nodeType: "workbench", health: "healthy", createdAt: "2026-08-01T00:00:00Z" },
          { id: "workbench-1", nodeType: "workbench", health: "healthy", createdAt: "2026-08-01T00:00:00Z" },
        ],
      }),
    );

    await screen.findByText("v1:planner:plan:1");
    // One group bar per replica, in replica order.
    const bars = screen.getAllByText(/^workbench-/, { selector: ".os-fleet-groupbar" });
    expect(bars.map((b) => b.textContent)).toEqual(["workbench-0", "workbench-1"]);
  });

  it("hides released workspaces until asked, then spells node_lost out", async () => {
    mount(fakeConnection({ myWorkspaces: [LIVE_A, LOST] }));
    await screen.findByText("v1:planner:plan:1");
    expect(screen.queryByText("v1:planner:plan:3")).toBeNull();

    await click(screen.getByLabelText("Show released"));

    await screen.findByText("v1:planner:plan:3");
    // The one thing an operator must not have to go to the source for: the
    // files went with the replica, they were NOT migrated, and the plan got
    // a fresh workspace elsewhere.
    const copy = screen.getByText(/left the mesh/);
    expect(copy.textContent).toContain("not migrated");
    expect(copy.textContent).toContain("fresh workspace elsewhere");
  });

  it("names a release reason it has no copy for rather than rendering nothing", async () => {
    mount(
      fakeConnection({
        myWorkspaces: [{ ...LOST, releasedReason: "some_future_reason" }],
      }),
    );
    await click(await screen.findByLabelText("Show released"));
    expect(await screen.findByText(/does not have copy for: some_future_reason/)).toBeTruthy();
  });

  it("distinguishes a replica with no workspaces from a cluster with no replicas", async () => {
    mount(
      fakeConnection({
        myWorkspaces: [LIVE_A],
        clusterNodes: [
          { id: "workbench-0", nodeType: "workbench", health: "healthy", createdAt: "2026-08-01T00:00:00Z" },
          { id: "workbench-7", nodeType: "workbench", health: "healthy", createdAt: "2026-08-01T00:00:00Z" },
          // A non-workbench node must not appear at all.
          { id: "bff-0", nodeType: "bff", health: "healthy", createdAt: "2026-08-01T00:00:00Z" },
        ],
      }),
    );

    const idle = await screen.findByText("workbench-7", { selector: ".os-mono" });
    expect(within(idle.closest("li") as HTMLElement).getByText("no workspaces")).toBeTruthy();
    expect(screen.queryByText("bff-0")).toBeNull();
  });

  it("says so when no workbench replica is running at all", async () => {
    mount(fakeConnection({ myWorkspaces: [], clusterNodes: [] }));
    expect(
      await screen.findByText(/No workbench replicas are running in this cluster/),
    ).toBeTruthy();
  });

  it("collapses the append-only node history to one row per replica", async () => {
    mount(
      fakeConnection({
        myWorkspaces: [],
        clusterNodes: [
          { id: "workbench-0", nodeType: "workbench", health: "starting", createdAt: "2026-08-01T00:00:00Z" },
          { id: "workbench-0", nodeType: "workbench", health: "healthy", createdAt: "2026-08-02T00:00:00Z" },
        ],
      }),
    );
    await screen.findByText("healthy");
    // Without the collapse a replica renders once per liveness row it has
    // ever written.
    expect(screen.getAllByText("workbench-0", { selector: ".os-mono" })).toHaveLength(1);
    expect(screen.queryByText("starting")).toBeNull();
  });

  it("keeps the replicas it has when a refresh fails, and says the read failed", async () => {
    const connection = fakeConnection({
      myWorkspaces: [],
      clusterNodes: [
        { id: "workbench-0", nodeType: "workbench", health: "healthy", createdAt: "2026-08-01T00:00:00Z" },
      ],
    });
    mount(connection);
    await screen.findByText("workbench-0", { selector: ".os-mono" });

    connection.query.clusterNodes.mockRejectedValue(new Error("read refused"));
    await click(screen.getByRole("button", { name: "Refresh" }));

    await waitFor(() => expect(screen.getByText("read refused")).toBeTruthy());
    // A stale answer beats no answer: blanking the list would be strictly
    // less information than it had a moment ago.
    expect(screen.getByText("workbench-0", { selector: ".os-mono" })).toBeTruthy();
    expect(screen.getByText(/from the last successful read/)).toBeTruthy();
  });

  it("reads workspaces from the live feed and replicas from a plain query", async () => {
    const connection = fakeConnection({ myWorkspaces: [LIVE_A], clusterNodes: [] });
    mount(connection);
    await screen.findByText("v1:planner:plan:1");
    // v1:cluster:node carries no broadcast routing rule, so its list has to
    // say when it was read rather than implying it updates itself.
    expect(screen.getByText(/refreshes on request rather than on its own/)).toBeTruthy();
  });
});
