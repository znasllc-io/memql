// /fleet/workbenches (memql#4356), end to end against a fake cluster.
//
// WHAT THIS FILE OWNS, and it is the same split fleetMachines.test.tsx states:
// the engine's tests prove the row gate refuses and the DSL suite proves the
// constructs are shaped right; only this can see the wire form the browser
// sends and what it puts on screen.
//
// The three properties worth the file:
//
//   - THE NODE COLLAPSE. clusterNodes returns the whole append-only history,
//     one row per liveness transition, and there is no per-nodeType read at
//     all. Both narrowings happen client-side, so both are asserted -- with a
//     fixture that gives one replica three rows and includes a node of another
//     type, because a page doing neither would pass a "the node rendered"
//     assertion.
//   - THE RELEASED TOGGLE. A released workspace is a directory that no longer
//     exists, so it is hidden by default; node_lost is the reason an operator
//     has to be able to read off the page, since the files went with the
//     replica and were not migrated.
//   - RELEASE IS DESTRUCTIVE AND CONFIRMS. Asserted as no call before the
//     dialog and exactly one after, so a button wired straight to the mutation
//     fails.

import { describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import {
  Result,
  type AccessSummary,
  type Connection,
  type Event,
  type QueryClient,
  type Role,
  type Row,
} from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { asQueryClient } from "./support/queryFake";

const WORKSPACE = "v1:workbench:workspace";
const CLUSTER_NODE = "v1:cluster:node";

const CLUSTER_CONFIG = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
  domain: "example.com",
};

function node(id: string, concept: string, payload: Record<string, unknown>, createdAt: string): Row {
  return { id, concept, createdAt, payload };
}

// Three rows for workbench-0 -- the append-only liveness stream -- plus one
// row for a node of a different type. A page that skipped either narrowing
// renders differently, which is the point of the fixture.
const NODE_ROWS: Row[] = [
  node(
    "workbench-0",
    CLUSTER_NODE,
    { nodeType: "workbench", address: "10.0.0.4:50052", health: "connecting", lastSeen: "2026-08-20T00:00:00.000Z", labels: {}, provider: "azure", region: "westus" },
    "2026-08-20T00:00:00.000Z",
  ),
  node(
    "workbench-0",
    CLUSTER_NODE,
    { nodeType: "workbench", address: "10.0.0.4:50052", health: "degraded", lastSeen: "2026-08-21T00:00:00.000Z", labels: {}, provider: "azure", region: "westus" },
    "2026-08-21T00:00:00.000Z",
  ),
  node(
    "workbench-0",
    CLUSTER_NODE,
    { nodeType: "workbench", address: "10.0.0.4:50052", health: "healthy", lastSeen: "2026-08-22T00:00:00.000Z", labels: { pool: "general" }, provider: "azure", region: "westus" },
    "2026-08-22T00:00:00.000Z",
  ),
  node(
    "bff-0",
    CLUSTER_NODE,
    { nodeType: "bff", address: "10.0.0.9:50051", health: "healthy", lastSeen: "2026-08-22T00:00:00.000Z", labels: {} },
    "2026-08-22T00:00:00.000Z",
  ),
];

const LIVE_WORKSPACE = node(
  "ws-live",
  WORKSPACE,
  {
    planId: "plan-alpha",
    ownerUserId: "user-1",
    nodeId: "workbench-0",
    status: "provisioned",
    storageRoot: "/var/lib/memql/workbenches/plan-alpha/",
    lastUsedAt: "2026-08-22T10:00:00.000Z",
    releasedAt: "",
    releasedReason: "",
  },
  "2026-08-22T09:00:00.000Z",
);

const LOST_WORKSPACE = node(
  "ws-lost",
  WORKSPACE,
  {
    planId: "plan-beta",
    ownerUserId: "user-1",
    nodeId: "workbench-1",
    status: "released",
    storageRoot: "/var/lib/memql/workbenches/plan-beta/",
    lastUsedAt: "2026-08-19T10:00:00.000Z",
    releasedAt: "2026-08-19T12:00:00.000Z",
    releasedReason: "node_lost",
  },
  "2026-08-19T09:00:00.000Z",
);

const OTHER_WORKSPACE = node(
  "ws-theirs",
  WORKSPACE,
  {
    planId: "plan-gamma",
    ownerUserId: "user-2",
    nodeId: "workbench-0",
    status: "provisioned",
    storageRoot: "/var/lib/memql/workbenches/plan-gamma/",
    lastUsedAt: "2026-08-22T11:00:00.000Z",
    releasedAt: "",
    releasedReason: "",
  },
  "2026-08-22T11:00:00.000Z",
);

interface Harness {
  query: QueryClient;
  subscriptions: unknown;
  calls: string[];
  callsNamed: (construct: string) => string[];
  emit: (concept: string, event: Event) => void;
}

function harness(overrides: { role?: Role } = {}): Harness {
  const calls: string[] = [];
  const handlers = new Map<string, (event: Event) => void>();

  const access: AccessSummary = {
    requestId: "r1",
    userId: "user-1",
    primaryEmail: "owner@example.com",
    clusterRole: overrides.role ?? "owner",
    sessionId: "",
    displayName: "Ops Person",
  };

  const executeNamed = vi.fn(async (_name: string, call: string) => {
    calls.push(call);

    if (call === "query clusterNodes()") {
      return new Result({ bundle: { nodes: NODE_ROWS } });
    }
    if (call === "query myWorkspaces()") {
      // The unfiltered caller read -- released rows included. Narrowing them
      // out is the page's job here, and that is what the toggle test checks.
      return new Result({ bundle: { nodes: [LIVE_WORKSPACE, LOST_WORKSPACE] } });
    }
    if (call === 'query allWorkspaces(status: "provisioned")') {
      return new Result({ bundle: { nodes: [LIVE_WORKSPACE, OTHER_WORKSPACE] } });
    }
    if (call === "query allWorkspaces()") {
      return new Result({ bundle: { nodes: [LIVE_WORKSPACE, OTHER_WORKSPACE, LOST_WORKSPACE] } });
    }
    return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
  });

  const query = asQueryClient({
    listConcepts: vi.fn(async () => []),
    getMyAccess: vi.fn(async () => access),
    executeNamed,
  });

  const subscriptions = {
    subscribeGraph: (fn: (event: Event) => void, opts?: { concept?: string }) => {
      const concept = opts?.concept ?? "";
      handlers.set(concept, fn);
      return () => handlers.delete(concept);
    },
  };

  return {
    query,
    subscriptions,
    calls,
    callsNamed: (construct: string) => calls.filter((c) => c.includes(`${construct}(`)),
    emit: (concept: string, event: Event) => {
      act(() => {
        handlers.get(concept)?.(event);
      });
    },
  };
}

function renderWorkbenches(h: Harness) {
  const dial = vi.fn(
    async () =>
      ({
        nodeId: "bff-test",
        serverVersion: "0.0.0-test",
        query: h.query,
        subscriptions: h.subscriptions,
        dispatcher: { sendAndWait: vi.fn(async () => ({})) },
        close: vi.fn(),
        done: vi.fn(() => new Promise<void>(() => {})),
      }) as unknown as Connection,
  ) as unknown as typeof Connection.dial;

  return render(
    <MemoryRouter initialEntries={["/fleet/workbenches"]}>
      <AuthProvider
        config={CLUSTER_CONFIG}
        fetchImpl={async () => {
          throw new Error("the fleet tests must make no identity calls");
        }}
        storage={null}
        navigate={() => {}}
        redirectUri="https://api.example.com/portal/auth/callback"
      >
        <ClusterProvider dial={dial}>
          <AppRoutes />
        </ClusterProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

// One Band's section, so an assertion about the replica list cannot match a
// workspace row that happens to name the same node.
function sectionFor(caption: string): HTMLElement {
  const heading = screen.getByRole("heading", { name: caption });
  const section = heading.closest("section");
  if (section === null) throw new Error(`no section wrapping the caption ${caption}`);
  return section;
}

describe("the workbench replicas", () => {
  it("collapses the node stream to the latest row per id and lists only workbenches", async () => {
    const h = harness();
    renderWorkbenches(h);
    await waitFor(() => expect(screen.getAllByText("workbench-0").length).toBeGreaterThan(0));

    // Scoped to the Replicas band: a workspace row also names the node it
    // lives on, so an unscoped count would pass for the wrong reason.
    const replicas = within(sectionFor("Replicas"));

    // ONE entry for the replica, not three -- the collapse ran.
    expect(replicas.getAllByText("workbench-0").length).toBe(1);
    // Its health is the NEWEST row's, not the first one the query returned.
    expect(replicas.getByText("healthy")).toBeTruthy();
    expect(replicas.queryByText("connecting")).toBeNull();
    expect(replicas.queryByText("degraded")).toBeNull();
    // A node of another type is not a workbench replica.
    expect(screen.queryByText("bff-0")).toBeNull();
    expect(h.calls).toContain("query clusterNodes()");
  });

  it("counts the listed workspaces per replica and says what the count is of", async () => {
    const h = harness();
    renderWorkbenches(h);
    await waitFor(() => expect(screen.getAllByText("workbench-0").length).toBeGreaterThan(0));

    // v1:cluster:node declares no capacity field, so this is a count of the
    // rows this page loaded -- and the caption has to say so rather than
    // implying a fill level.
    expect(screen.getByText(/of the ones listed below, not a cluster total/)).toBeTruthy();
  });
});

describe("the workspaces list", () => {
  it("hides released rows by default and shows them on the toggle, naming node_lost", async () => {
    const h = harness();
    renderWorkbenches(h);
    await waitFor(() => expect(screen.getByText("plan-alpha")).toBeTruthy());

    // The released row came back in the SAME read -- so its absence is the
    // page filtering, not the fixture being empty.
    expect(screen.queryByText("plan-beta")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Show released" }));

    await waitFor(() => expect(screen.getByText("plan-beta")).toBeTruthy());
    expect(screen.getByText(/node_lost/)).toBeTruthy();
    // The reason is EXPLAINED, not just printed: the files went with the
    // replica and were not migrated, which is a design decision an operator
    // must not have to read the source to learn.
    expect(screen.getByText(/they are not migrated/)).toBeTruthy();
  });

  it("offers the all-workspaces view to a cluster owner and narrows it server-side", async () => {
    const h = harness();
    renderWorkbenches(h);
    await waitFor(() => expect(screen.getByText("plan-alpha")).toBeTruthy());

    fireEvent.change(screen.getByRole("combobox", { name: "Whose workspaces" }), {
      target: { value: "all" },
    });

    // The status narrowing is an ARGUMENT for the owner read, because
    // allWorkspaces declares one and myWorkspaces does not.
    await waitFor(() => expect(h.calls).toContain('query allWorkspaces(status: "provisioned")'));
    await waitFor(() => expect(screen.getByText("plan-gamma")).toBeTruthy());
  });

  it("does not offer the all view, or Release, to a non-cluster-owner", async () => {
    const h = harness({ role: "writer" });
    renderWorkbenches(h);

    // The page RENDERED, so this is not passing because nothing loaded.
    await waitFor(() => expect(screen.getByText("plan-alpha")).toBeTruthy());
    expect(screen.queryByRole("combobox", { name: "Whose workspaces" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Release" })).toBeNull();
    expect(h.callsNamed("allWorkspaces").length).toBe(0);
    expect(h.calls).toContain("query myWorkspaces()");
  });

  it("releases only after the confirmation, through releaseWorkspace", async () => {
    const h = harness();
    renderWorkbenches(h);
    await waitFor(() => expect(screen.getByText("plan-alpha")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Release" }));
    await waitFor(() => expect(screen.getByRole("dialog")).toBeTruthy());
    // Nothing sent yet. This half fails against a button wired straight to the
    // mutation.
    expect(h.callsNamed("releaseWorkspace").length).toBe(0);
    // The dialog states what is destroyed, in facts.
    expect(screen.getByText(/is not recoverable and is not moved anywhere first/)).toBeTruthy();

    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Release it" }));

    await waitFor(() => expect(h.callsNamed("releaseWorkspace").length).toBe(1));
    expect(h.callsNamed("releaseWorkspace")[0]).toBe(
      'mutation releaseWorkspace(workspaceId: "ws-live", reason: "explicit")',
    );
  });

  it("carries a new workspace in over the subscription, without a refetch", async () => {
    const h = harness();
    renderWorkbenches(h);
    await waitFor(() => expect(screen.getByText("plan-alpha")).toBeTruthy());
    const readsBefore = h.calls.filter((c) => c === "query myWorkspaces()").length;

    h.emit(WORKSPACE, {
      subscriptionId: "s",
      kind: "NODE_CREATED",
      timestamp: new Date(),
      payloadOmitted: false,
      seq: 0,
      gapBefore: false,
      payload: {
        id: "ws-new",
        concept: WORKSPACE,
        planId: "plan-delta",
        ownerUserId: "user-1",
        nodeId: "workbench-0",
        status: "provisioned",
        storageRoot: "/var/lib/memql/workbenches/plan-delta/",
      },
    });

    await waitFor(() => expect(screen.getByText("plan-delta")).toBeTruthy());
    expect(h.calls.filter((c) => c === "query myWorkspaces()").length).toBe(readsBefore);
  });

  it("drops a workspace out of the list when it is released while released rows are hidden", async () => {
    // A release is an UPDATE, not a delete. Leaving the row on screen would
    // show a directory that no longer exists.
    const h = harness();
    renderWorkbenches(h);
    await waitFor(() => expect(screen.getByText("plan-alpha")).toBeTruthy());

    h.emit(WORKSPACE, {
      subscriptionId: "s",
      kind: "NODE_UPDATED",
      timestamp: new Date(),
      payloadOmitted: false,
      seq: 0,
      gapBefore: false,
      payload: {
        id: "ws-live",
        concept: WORKSPACE,
        planId: "plan-alpha",
        ownerUserId: "user-1",
        nodeId: "workbench-0",
        status: "released",
        releasedAt: "2026-08-23T00:00:00.000Z",
        releasedReason: "explicit",
      },
    });

    await waitFor(() => expect(screen.queryByText("plan-alpha")).toBeNull());
  });
});
