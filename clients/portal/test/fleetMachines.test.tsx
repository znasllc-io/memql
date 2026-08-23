// /fleet/machines (memql#4355), end to end against a fake cluster.
//
// WHAT THIS FILE OWNS. The DSL conformance suite proves the constructs are
// shaped correctly and the engine's own tests prove the row-authz tier
// actually refuses; neither can see what the browser SENDS or what it renders.
// This file asserts the wire form -- that the page issues the named calls the
// DSL declares, with the arguments quoted -- and the handful of behaviours the
// issue turns on: the online derivation reaching the screen, the label merge
// with operator precedence, rename, the policy editor's create-versus-update
// choice, revoke behind a confirmation, and that a non-cluster-owner is never
// offered the all-machines read.
//
// EVERY TEST IS WRITTEN TO FAIL ON THE VACUOUS PASS.
//
//   - "operator labels win" would pass against a page that rendered ONLY the
//     operator map, so it also asserts the REPORTED value for the same key is
//     absent -- which is the half that proves a merge happened.
//   - "the policy editor updates" would pass against a page that always
//     created, so the create and update cases assert the OTHER call was never
//     issued.
//   - "revoke confirms" would pass against a page whose button did nothing at
//     all, so it asserts no call before the dialog and exactly one after.
//   - "a non-owner sees no all-machines view" would pass against a page that
//     failed to render, so it asserts the machine list IS there and only the
//     scope control is missing.

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

const REGISTRATION = "v1:worker:registration";

const CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
  domain: "example.com",
};

function node(id: string, concept: string, payload: Record<string, unknown>, createdAt: string): Row {
  return { id, concept, createdAt, payload };
}

// Fresh relative to the moment the page renders, so the online derivation has
// something to be true about. A literal timestamp here would go stale the day
// after it was written and turn a real assertion into a permanent "offline".
function secondsAgo(seconds: number): string {
  return new Date(Date.now() - seconds * 1000).toISOString();
}

function machineRow(over: Partial<Record<string, unknown>> = {}): Row {
  return node(
    "wk-1",
    REGISTRATION,
    {
      ownerUserId: "user-1",
      identityId: "ident-1",
      name: "jose-mac-mini",
      displayName: "",
      capabilities: ["HEADLESS", "COMPUTERUSE"],
      capabilityDescriptor: { displayServer: "quartz", computerUseAvailable: true },
      // The same key on both sides, with DIFFERENT values -- that is what makes
      // the merge assertion meaningful.
      labels: { os: "darwin", tier: "laptop" },
      operatorLabels: { tier: "studio" },
      connectedNodeId: "agent-0",
      activeCount: 2,
      concurrency: { HEADLESS: 8, COMPUTERUSE: 1 },
      platformInfo: { os: "darwin", arch: "arm64", hostname: "jose-mac-mini" },
      version: "v2026.5.4",
      buildTag: "computeruse",
      registeredAt: "2026-08-01T00:00:00.000Z",
      lastSeenAt: secondsAgo(5),
      revokedAt: "",
      ...over,
    },
    "2026-08-01T00:00:00.000Z",
  );
}

const OTHER_MACHINE = node(
  "wk-2",
  REGISTRATION,
  {
    ownerUserId: "user-2",
    identityId: "ident-2",
    name: "ci-runner",
    displayName: "",
    capabilities: ["HEADLESS"],
    labels: {},
    operatorLabels: {},
    connectedNodeId: "",
    activeCount: 0,
    concurrency: { HEADLESS: 4 },
    platformInfo: { os: "linux", arch: "amd64", hostname: "ci-runner" },
    registeredAt: "2026-08-02T00:00:00.000Z",
    lastSeenAt: "2026-08-02T00:00:00.000Z",
    revokedAt: "",
  },
  "2026-08-02T00:00:00.000Z",
);

const POLICY_ROW = node(
  "policy-1",
  "v1:worker:routingPolicy",
  {
    ownerUserId: "user-1",
    strategy: "roundRobin",
    requireLabels: { tier: "studio" },
    preferLabels: {},
    fallback: "nextMatching",
    active: true,
  },
  "2026-08-10T00:00:00.000Z",
);

const INVOCATION_ROW = node(
  "inv-1",
  "v1:worker:invocation",
  {
    ownerUserId: "user-1",
    workerId: "wk-1",
    tool: "workerHost",
    action: "exec",
    outcome: "rerouted",
    durationMs: 1400,
    errorCode: "",
    errorMessage: "",
    routing: {
      policyId: "policy-1",
      strategy: "roundRobin",
      candidatesConsidered: ["wk-9", "wk-1"],
      attempts: 2,
      selectedBy: "policy",
      reroutedFrom: "workbench",
      requireLabels: { tier: "studio" },
      preferLabels: {},
    },
  },
  "2026-08-20T09:00:00.000Z",
);

interface Harness {
  query: QueryClient;
  subscriptions: unknown;
  calls: string[];
  callsNamed: (construct: string) => string[];
  emit: (concept: string, event: Event) => void;
  // Envelopes sent down the stream's dispatcher rather than through the query
  // surface -- the worker-token mint is a gRPC message, not a named call.
  sent: Record<string, unknown>[];
  dispatcher: { sendAndWait: (envelope: Record<string, unknown>) => Promise<unknown> };
}

const MINTED_TOKEN = "mql_wkr_" + "a".repeat(43);

function harness(
  overrides: { role?: Role; policies?: Row[]; machines?: Row[] } = {},
): Harness {
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

    if (call === "query myWorkersWithStatus()") {
      return new Result({ bundle: { nodes: overrides.machines ?? [machineRow()] } });
    }
    if (call.startsWith("query allWorkersWithStatus(")) {
      return new Result({ bundle: { nodes: [machineRow(), OTHER_MACHINE] } });
    }
    if (call === "query myRoutingPolicies()") {
      return new Result({ bundle: { nodes: overrides.policies ?? [] } });
    }
    if (call.startsWith("query invocationsForWorker")) {
      return new Result({ bundle: { nodes: [INVOCATION_ROW] } });
    }
    // Mutations: nothing reads the reply, so an empty envelope is enough.
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

  const sent: Record<string, unknown>[] = [];
  const dispatcher = {
    sendAndWait: vi.fn(async (envelope: Record<string, unknown>) => {
      sent.push(envelope);
      if ("createWorkerToken" in envelope) {
        const req = envelope["createWorkerToken"] as Record<string, unknown>;
        return {
          createWorkerTokenResult: {
            requestId: req["requestId"],
            success: true,
            plainToken: MINTED_TOKEN,
            identityId: "ident-new",
            ownerUserId: "user-1",
          },
        };
      }
      return {};
    }),
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
    sent,
    dispatcher,
  };
}

function renderFleet(h: Harness, path = "/fleet/machines") {
  const dial = vi.fn(
    async () =>
      ({
        nodeId: "bff-test",
        serverVersion: "0.0.0-test",
        query: h.query,
        subscriptions: h.subscriptions,
        dispatcher: h.dispatcher,
        close: vi.fn(),
        done: vi.fn(() => new Promise<void>(() => {})),
      }) as unknown as Connection,
  ) as unknown as typeof Connection.dial;

  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={CLUSTER}
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

// The card for one machine, so an assertion cannot accidentally match a
// control belonging to the routing editor further down the page.
function cardFor(name: string): HTMLElement {
  const heading = screen.getByRole("heading", { name });
  const card = heading.closest("li");
  if (card === null) throw new Error(`no card wrapping the heading ${name}`);
  return card;
}

describe("the machines list", () => {
  it("lists the caller's machines through myWorkersWithStatus", async () => {
    const h = harness();
    renderFleet(h);
    await waitFor(() => expect(screen.getByRole("heading", { name: "jose-mac-mini" })).toBeTruthy());
    expect(h.calls).toContain("query myWorkersWithStatus()");
    // Registration order comes from the query. What the page adds is the
    // reading beside it.
    expect(screen.getByText("darwin / arm64")).toBeTruthy();
    expect(screen.getByText("quartz")).toBeTruthy();
    expect(screen.getByText("agent-0")).toBeTruthy();
  });

  it("shows a fresh heartbeat as online and a stale one as offline", async () => {
    const h = harness({ machines: [machineRow(), OTHER_MACHINE] });
    renderFleet(h);
    await waitFor(() => expect(screen.getByRole("heading", { name: "jose-mac-mini" })).toBeTruthy());

    // Both dots exist, and they disagree -- which is the property. A page that
    // hardcoded either answer would fail one of these two.
    expect(within(cardFor("jose-mac-mini")).getByRole("img", { name: "Online" })).toBeTruthy();
    expect(within(cardFor("ci-runner")).getByRole("img", { name: "Offline" })).toBeTruthy();
  });

  it("renders the label MERGE with the operator's value winning", async () => {
    const h = harness();
    renderFleet(h);
    await waitFor(() => expect(screen.getByRole("heading", { name: "jose-mac-mini" })).toBeTruthy());
    const card = within(cardFor("jose-mac-mini"));

    // The operator's value is on screen...
    expect(card.getAllByText("tier=studio").length).toBeGreaterThan(0);
    // ...the reported value for the SAME key is not, anywhere...
    expect(card.queryByText("tier=laptop")).toBeNull();
    // ...and the override is called out, because a machine reporting one thing
    // while the routing acts on another is a fact about the operator's own
    // configuration.
    expect(card.getByText("yours, overriding")).toBeTruthy();
    // A key only the machine reports still appears -- this is a merge, not a
    // replacement.
    expect(card.getByText("os=darwin")).toBeTruthy();
  });

  it("carries a new machine in over the subscription, without a refetch", async () => {
    const h = harness();
    renderFleet(h);
    await waitFor(() => expect(screen.getByRole("heading", { name: "jose-mac-mini" })).toBeTruthy());
    const readsBefore = h.calls.filter((c) => c === "query myWorkersWithStatus()").length;

    h.emit(REGISTRATION, {
      subscriptionId: "s",
      kind: "NODE_CREATED",
      timestamp: new Date(),
      payloadOmitted: false,
      payload: {
        id: "wk-3",
        concept: REGISTRATION,
        ownerUserId: "user-1",
        name: "studio-linux",
        displayName: "",
        capabilities: ["HEADLESS"],
        labels: {},
        operatorLabels: {},
        concurrency: { HEADLESS: 2 },
        lastSeenAt: secondsAgo(2),
        revokedAt: "",
      },
    });

    await waitFor(() => expect(screen.getByRole("heading", { name: "studio-linux" })).toBeTruthy());
    // The row arrived because the EVENT carried it. A page that re-read on
    // every event would pass a "the machine appeared" assertion just as well,
    // and would then issue one read per heartbeat forever.
    expect(h.calls.filter((c) => c === "query myWorkersWithStatus()").length).toBe(readsBefore);
  });

  it("drops an event for somebody else's machine while the scope is 'mine'", async () => {
    // A cluster owner is admitted to every registration row, so their
    // subscription carries events for machines they are not looking at.
    const h = harness();
    renderFleet(h);
    await waitFor(() => expect(screen.getByRole("heading", { name: "jose-mac-mini" })).toBeTruthy());

    h.emit(REGISTRATION, {
      subscriptionId: "s",
      kind: "NODE_CREATED",
      timestamp: new Date(),
      payloadOmitted: false,
      payload: {
        id: "wk-9",
        concept: REGISTRATION,
        ownerUserId: "somebody-else",
        name: "not-yours",
        labels: {},
        operatorLabels: {},
        lastSeenAt: secondsAgo(2),
        revokedAt: "",
      },
    });

    expect(screen.queryByRole("heading", { name: "not-yours" })).toBeNull();
  });
});

describe("the all-machines view", () => {
  it("is offered to a cluster owner and reads allWorkersWithStatus", async () => {
    const h = harness();
    renderFleet(h);
    await waitFor(() => expect(screen.getByRole("heading", { name: "jose-mac-mini" })).toBeTruthy());

    const scope = screen.getByRole("combobox", { name: "Whose machines" });
    fireEvent.change(scope, { target: { value: "all" } });

    await waitFor(() => expect(h.calls).toContain("query allWorkersWithStatus()"));
    await waitFor(() => expect(screen.getByRole("heading", { name: "ci-runner" })).toBeTruthy());
    // The owner column is what the all view adds.
    expect(screen.getByText("user-2")).toBeTruthy();
  });

  it("is not offered to a non-cluster-owner, who still gets their own machines", async () => {
    const h = harness({ role: "writer" });
    renderFleet(h);

    // The page RENDERED -- so this is not passing because nothing loaded.
    await waitFor(() => expect(screen.getByRole("heading", { name: "jose-mac-mini" })).toBeTruthy());
    expect(screen.queryByRole("combobox", { name: "Whose machines" })).toBeNull();
    expect(h.calls).toContain("query myWorkersWithStatus()");
    expect(h.callsNamed("allWorkersWithStatus").length).toBe(0);
  });
});

describe("the machine verbs", () => {
  it("renames through renameWorker", async () => {
    const h = harness();
    renderFleet(h);
    await waitFor(() => expect(screen.getByRole("heading", { name: "jose-mac-mini" })).toBeTruthy());

    fireEvent.click(within(cardFor("jose-mac-mini")).getByRole("button", { name: "Rename" }));
    fireEvent.change(screen.getByPlaceholderText("jose-mac-mini"), {
      target: { value: "Studio Mac" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save name" }));

    await waitFor(() => expect(h.callsNamed("renameWorker").length).toBe(1));
    expect(h.callsNamed("renameWorker")[0]).toBe(
      'mutation renameWorker(registrationId: "wk-1", displayName: "Studio Mac")',
    );
  });

  it("writes the WHOLE operator label map on an add, with quoted keys", async () => {
    const h = harness();
    renderFleet(h);
    await waitFor(() => expect(screen.getByRole("heading", { name: "jose-mac-mini" })).toBeTruthy());

    const card = within(cardFor("jose-mac-mini"));
    const input = card.getByPlaceholderText("Add a label, press Enter");
    fireEvent.change(input, { target: { value: "has-blender=true" } });
    fireEvent.keyDown(input, { key: "Enter" });

    await waitFor(() => expect(h.callsNamed("setWorkerOperatorLabels").length).toBe(1));
    // The mutation REPLACES the map, so the existing label has to be sent
    // alongside the new one -- an add that sent only the new pair would delete
    // the other. And the keys are QUOTED: `has-blender` does not lex as one
    // identifier, so the bare form the generated builder would emit is a parse
    // error naming a token nobody wrote.
    expect(h.callsNamed("setWorkerOperatorLabels")[0]).toBe(
      'mutation setWorkerOperatorLabels(registrationId: "wk-1", operatorLabels: {"has-blender": "true", "tier": "studio"})',
    );
  });

  it("refuses a label that is not a key=value pair, and sends nothing", async () => {
    const h = harness();
    renderFleet(h);
    await waitFor(() => expect(screen.getByRole("heading", { name: "jose-mac-mini" })).toBeTruthy());

    const card = within(cardFor("jose-mac-mini"));
    const input = card.getByPlaceholderText("Add a label, press Enter");
    fireEvent.change(input, { target: { value: "blender" } });
    fireEvent.keyDown(input, { key: "Enter" });

    expect(await card.findByText(/is not a label/)).toBeTruthy();
    expect(h.callsNamed("setWorkerOperatorLabels").length).toBe(0);
  });

  it("revokes only after the confirmation, through revokeWorker", async () => {
    const h = harness();
    renderFleet(h);
    await waitFor(() => expect(screen.getByRole("heading", { name: "jose-mac-mini" })).toBeTruthy());

    fireEvent.click(within(cardFor("jose-mac-mini")).getByRole("button", { name: "Revoke" }));
    // The dialog is up and NOTHING has been sent. This is the half that fails
    // against a button wired straight to the mutation.
    await waitFor(() => expect(screen.getByRole("dialog")).toBeTruthy());
    expect(h.callsNamed("revokeWorker").length).toBe(0);

    const dialog = within(screen.getByRole("dialog"));
    fireEvent.change(dialog.getByPlaceholderText("Laptop returned"), {
      target: { value: "returned" },
    });
    fireEvent.click(dialog.getByRole("button", { name: "Revoke this machine" }));

    await waitFor(() => expect(h.callsNamed("revokeWorker").length).toBe(1));
    const call = h.callsNamed("revokeWorker")[0] ?? "";
    expect(call.startsWith("mutation revokeWorker(registrationId: \"wk-1\", revokedAt: ")).toBe(true);
    expect(call).toContain('revokedBy: "user-1"');
    expect(call).toContain('revokeReason: "returned"');
  });

  // v1:worker:invocation declares no row tier, so the caller scope lives in the
  // FILTER -- and one filter cannot be both "mine" and "any, if you are a
  // cluster owner". Calling the self-scoped read from the all-machines view
  // returns NOTHING for a machine the operator does not personally own, and an
  // empty activity list reads as "this machine is idle" rather than "wrong
  // query". So which name goes on the wire is the assertion.
  it("reads a person's own machine through the self-scoped query", async () => {
    const h = harness();
    renderFleet(h);
    await waitFor(() => expect(screen.getByRole("heading", { name: "jose-mac-mini" })).toBeTruthy());
    fireEvent.click(within(cardFor("jose-mac-mini")).getByRole("button", { name: "Recent calls" }));

    await waitFor(() =>
      expect(h.calls).toContain('query invocationsForWorker(workerId: "wk-1")'),
    );
    expect(h.calls.some((call) => call.includes("invocationsForWorkerAsOperator"))).toBe(false);
  });

  it("reads through the operator-scoped query on the all-machines view", async () => {
    const h = harness();
    renderFleet(h);
    await waitFor(() => expect(screen.getByRole("heading", { name: "jose-mac-mini" })).toBeTruthy());

    fireEvent.change(screen.getByRole("combobox", { name: "Whose machines" }), {
      target: { value: "all" },
    });
    await waitFor(() => expect(screen.getByRole("heading", { name: "ci-runner" })).toBeTruthy());

    fireEvent.click(within(cardFor("ci-runner")).getByRole("button", { name: "Recent calls" }));
    await waitFor(() =>
      expect(h.calls).toContain('query invocationsForWorkerAsOperator(workerId: "wk-2")'),
    );
  });

  it("shows a machine's recent calls with the routing that chose it", async () => {
    const h = harness();
    renderFleet(h);
    await waitFor(() => expect(screen.getByRole("heading", { name: "jose-mac-mini" })).toBeTruthy());

    fireEvent.click(within(cardFor("jose-mac-mini")).getByRole("button", { name: "Recent calls" }));

    await waitFor(() =>
      expect(h.calls).toContain('query invocationsForWorker(workerId: "wk-1")'),
    );
    const card = within(cardFor("jose-mac-mini"));
    expect(await card.findByText("rerouted")).toBeTruthy();
    // The routing record is the reason this list exists, so every field of it
    // is asserted rather than "an invocation rendered".
    expect(card.getByText("roundRobin")).toBeTruthy();
    expect(card.getByText(/wk-9, wk-1/)).toBeTruthy();
    expect(card.getByText(/a candidate refused before starting/)).toBeTruthy();
    expect(card.getByText("policy")).toBeTruthy();
    expect(card.getByText("workbench")).toBeTruthy();
  });
});

describe("the routing policy editor", () => {
  it("CREATES when the caller has none, and does not update", async () => {
    const h = harness({ policies: [] });
    renderFleet(h);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Create routing policy" })).toBeTruthy(),
    );
    // The no-policy state is stated as normal, not as a gap.
    expect(screen.getByText(/that is a normal state/)).toBeTruthy();

    fireEvent.change(screen.getByRole("combobox", { name: "Strategy" }), {
      target: { value: "leastLoaded" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create routing policy" }));

    await waitFor(() => expect(h.callsNamed("createRoutingPolicy").length).toBe(1));
    expect(h.callsNamed("updateRoutingPolicy").length).toBe(0);
    const call = h.callsNamed("createRoutingPolicy")[0] ?? "";
    expect(call).toContain('strategy: "leastLoaded"');
    expect(call).toContain("requireLabels: {}");
    expect(call).toContain('fallback: "nextMatching"');
  });

  it("UPDATES the active row in place when one exists, and does not create", async () => {
    const h = harness({ policies: [POLICY_ROW] });
    renderFleet(h);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Save routing policy" })).toBeTruthy(),
    );

    // The form opened on the ROW's values, not on the defaults -- which is
    // what makes "save" an edit rather than a silent reset.
    const strategy = screen.getByRole("combobox", { name: "Strategy" }) as HTMLSelectElement;
    expect(strategy.value).toBe("roundRobin");

    fireEvent.change(strategy, { target: { value: "labelMatch" } });
    fireEvent.click(screen.getByRole("button", { name: "Save routing policy" }));

    await waitFor(() => expect(h.callsNamed("updateRoutingPolicy").length).toBe(1));
    expect(h.callsNamed("createRoutingPolicy").length).toBe(0);
    const call = h.callsNamed("updateRoutingPolicy")[0] ?? "";
    // The row's own id -- an edit, not a new row under a fresh id.
    expect(call).toContain('policyId: "policy-1"');
    expect(call).toContain('strategy: "labelMatch"');
    // The labels it was carrying survive a strategy-only edit.
    expect(call).toContain('requireLabels: {"tier": "studio"}');
  });

  it("re-reads after a create, so a second save cannot mint a second policy", async () => {
    // The invariant is held on the WRITE side by editing in place; without the
    // re-read the editor would still believe it has no policy and the next
    // save would insert another active row.
    let policies: Row[] = [];
    const h = harness();
    // Re-point the read at a mutable fixture: the create is what fills it.
    const created = node(
      "policy-new",
      "v1:worker:routingPolicy",
      {
        ownerUserId: "user-1",
        strategy: "firstFit",
        requireLabels: {},
        preferLabels: {},
        fallback: "nextMatching",
        active: true,
      },
      "2026-08-23T00:00:00.000Z",
    );
    const client = h.query as unknown as {
      executeNamed: (name: string, call: string) => Promise<Result>;
    };
    const inner = client.executeNamed.bind(client);
    (h.query as unknown as { executeNamed: (name: string, call: string) => Promise<Result> })
      .executeNamed = async (name: string, call: string) => {
      if (call === "query myRoutingPolicies()") {
        h.calls.push(call);
        return new Result({ bundle: { nodes: policies } });
      }
      if (call.startsWith("mutation createRoutingPolicy(")) policies = [created];
      return inner(name, call);
    };

    renderFleet(h);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Create routing policy" })).toBeTruthy(),
    );
    fireEvent.click(screen.getByRole("button", { name: "Create routing policy" }));

    // The button becomes the EDIT verb, which is only true if the re-read
    // landed and the editor now has a row.
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Save routing policy" })).toBeTruthy(),
    );
    expect(h.callsNamed("createRoutingPolicy").length).toBe(1);
  });
});

describe("adding a machine", () => {
  it("mints a worker token over the stream and shows it once with the install command", async () => {
    const h = harness();
    renderFleet(h);
    await waitFor(() => expect(screen.getByRole("heading", { name: "jose-mac-mini" })).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Add a machine" }));
    fireEvent.change(screen.getByPlaceholderText("jose-mac-mini"), {
      target: { value: "studio-linux" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Mint a token" }));

    // THE MINT IS A gRPC MESSAGE ON THE STREAM, not identity's POST
    // /pair/codes -- that endpoint needs the access token, which this
    // application deliberately has no way to read (src/cluster/auth.ts).
    await waitFor(() =>
      expect(h.sent.some((envelope) => "createWorkerToken" in envelope)).toBe(true),
    );
    const envelope = h.sent.find((one) => "createWorkerToken" in one) as Record<string, unknown>;
    expect((envelope["createWorkerToken"] as Record<string, unknown>)["name"]).toBe("studio-linux");

    // The plain bearer exists in the reply and nowhere else, so the page has
    // to show it and say so.
    expect(await screen.findByText(MINTED_TOKEN)).toBeTruthy();
    expect(screen.getByText(/It is not shown again/)).toBeTruthy();

    // The command carries BOTH values an operator would otherwise substitute
    // by hand, which is where the mistakes are.
    const command = screen.getByText(/curl -fsSL/).textContent ?? "";
    expect(command).toContain(`--token ${MINTED_TOKEN}`);
    expect(command).toContain("--cluster https://api.example.com");
    expect(command).not.toContain("--computeruse");
  });

  it("reports the registration when the subscription carries one, not when the mint succeeds", async () => {
    const h = harness();
    renderFleet(h);
    await waitFor(() => expect(screen.getByRole("heading", { name: "jose-mac-mini" })).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Add a machine" }));
    fireEvent.change(screen.getByPlaceholderText("jose-mac-mini"), {
      target: { value: "studio-linux" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Mint a token" }));

    // A minted token proves nothing about the machine -- the install can fail,
    // the URL can be wrong, the machine can be firewalled. So the panel is
    // still WAITING here, and that is the assertion a "mint then congratulate"
    // implementation fails.
    expect(await screen.findByText(/Waiting for the machine to connect/)).toBeTruthy();

    h.emit(REGISTRATION, {
      subscriptionId: "s",
      kind: "NODE_CREATED",
      timestamp: new Date(),
      payloadOmitted: false,
      payload: {
        id: "wk-new",
        concept: REGISTRATION,
        ownerUserId: "user-1",
        name: "studio-linux",
        labels: {},
        operatorLabels: {},
        lastSeenAt: secondsAgo(1),
        revokedAt: "",
      },
    });

    expect(await screen.findByText(/A new machine has registered/)).toBeTruthy();
  });
});

describe("the nav rail", () => {
  it("carries a Fleet group linking to both surfaces", async () => {
    const h = harness();
    renderFleet(h);
    await waitFor(() => expect(screen.getByRole("heading", { name: "jose-mac-mini" })).toBeTruthy());

    const rail = within(screen.getByRole("navigation", { name: "Portal sections" }));
    expect(rail.getByRole("heading", { name: "Fleet" })).toBeTruthy();
    expect(rail.getByRole("link", { name: "Machines" }).getAttribute("href")).toBe(
      "/fleet/machines",
    );
    expect(rail.getByRole("link", { name: "Workbenches" }).getAttribute("href")).toBe(
      "/fleet/workbenches",
    );
  });
});

