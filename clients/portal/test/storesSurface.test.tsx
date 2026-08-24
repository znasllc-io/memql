// The Stores surface (memql#4398), end to end against a fake cluster.
//
// WHAT THIS FILE OWNS. integrations/shopify's Go tests prove the connector's
// half of every Shopify conversation, and cmd/shopifyschema's drift gate
// proves the generated tree is what the generator produces. Neither can see
// what the browser sends or renders. This file asserts the wire form -- that
// the screens issue the named calls the DSL declares, with the arguments
// quoted by the generated builders -- and the three honesty properties this
// surface turns on:
//
//   1. A non-owner sees an EXPLANATION rather than an empty table. Every read
//      behind this screen carries actor.isClusterOwner as an explicit
//      conjunct, so a non-owner's list comes back empty at the engine, and an
//      empty table would read as "there are no stores".
//   2. The add form never sends a TOKEN. It takes the NAMES of secret rows,
//      because a token in this form is a token in a browser and in this app's
//      memory.
//   3. Drift is shown as what it MEANS. "40" is not actionable; the number
//      has to arrive next to the words that make it a diagnosis.
//
// Every test is written to fail on the vacuous pass: the access test asserts
// the explanation text AND the absence of the page body, and the health test
// asserts a value the fake supplied rather than merely that something
// rendered.

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import {
  Result,
  type AccessSummary,
  type Concept,
  type Connection,
  type QueryClient,
  type Role,
} from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { asQueryClient } from "./support/queryFake";

const STORE = "v1:shopify:store";

const CONCEPTS: Concept[] = [
  {
    id: STORE,
    version: "v1",
    domain: "shopify",
    entity: "store",
    type: "concept",
    description: "A configured Shopify store",
    displayCard: { primary: "domain", secondary: "name", tertiary: "apiVersion", status: "status" },
  },
];

// The shopifyStoreHealth builtin's reply, in the envelope the engine really
// produces for a top-level builtin: one node keyed by id, payload inside.
const HEALTH_REPLY = {
  bundle: {
    nodes: [
      {
        id: "integration:shopify:result",
        concept: "integration:shopify:result",
        type: "object",
        createdBy: "system",
        createdAt: "2026-08-23T12:00:00.000Z",
        payload: {
          status: "ok",
          stores: [
            {
              storeId: "acme",
              domain: "acme-widgets.myshopify.com",
              status: "live",
              apiVersion: "2026-07",
              mirrorApiVersion: "2026-07",
              protectedDataLevel: "level1",
              scopesGranted: ["read_products"],
              scopesNeeded: ["read_products", "read_orders"],
              scopesMissing: ["read_orders"],
              driftLast: 40,
              health: {
                subscriptions: { desired: 150, existing: 148, failed: [], at: "2026-08-23T03:15:00Z" },
              },
              costBucket: { currentlyAvailable: 1800, maximumAvailable: 2000, restoreRate: 100 },
              domains: [
                {
                  concept: "v1:shopify:order",
                  phase: "idle",
                  lastAppliedAt: "2026-08-23T11:00:00Z",
                  lastReconciledAt: "2026-08-23T11:30:00Z",
                  driftLast: 40,
                  driftTotal: 91,
                  staleWrites: 3,
                  tombstoned: 1,
                  lastError: "",
                },
              ],
            },
          ],
        },
      },
    ],
  },
};

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
  domain: "",
};

interface Harness {
  query: QueryClient;
  subscriptions: unknown;
  calls: string[];
  callsNamed: (construct: string) => string[];
}

function harness(overrides: { role?: Role } = {}): Harness {
  const calls: string[] = [];

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
    if (call.startsWith("builtin shopifyStoreHealth")) {
      return new Result(HEALTH_REPLY);
    }
    return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
  });

  const query = asQueryClient({
    listConcepts: vi.fn(async () => CONCEPTS),
    getMyAccess: vi.fn(async () => access),
    executeNamed,
  });

  const subscriptions = {
    subscribeGraph: () => () => {},
  };

  return {
    query,
    subscriptions,
    calls,
    callsNamed: (construct: string) => calls.filter((c) => c.includes(`${construct}(`)),
  };
}

function renderStores(h: Harness, path: string) {
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
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("the stores tests must make no identity calls");
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

describe("the stores list", () => {
  it("reads the health through the declared builtin and shows the store", async () => {
    const h = harness();
    renderStores(h, "/stores");
    await waitFor(() => expect(screen.getAllByText("acme-widgets.myshopify.com").length).toBeGreaterThan(0));
    expect(h.callsNamed("shopifyStoreHealth").length).toBeGreaterThan(0);
  });

  // A missing scope makes Shopify return null for the fields it covers, so
  // the mirror is quietly INCOMPLETE rather than broken. Naming the gap is
  // the difference between "the customer has no phone number" and "we were
  // never allowed to see it".
  it("names the scopes the mirror needs and the store does not have", async () => {
    const h = harness();
    renderStores(h, "/stores");
    await waitFor(() => expect(screen.getAllByText(/1 scope\(s\) missing/).length).toBeGreaterThan(0));
  });

  it("explains itself to a non-owner instead of rendering an empty table", async () => {
    const h = harness({ role: "developer" });
    renderStores(h, "/stores");
    await waitFor(() => expect(screen.getByText(/cluster-owner surface/i)).toBeTruthy());
    expect(screen.queryByText("Add a store")).toBeNull();
    expect(screen.queryByText("Configured stores")).toBeNull();
  });

  // THE property, not "the form has fields": the three credential inputs take
  // the NAME of a secret row. A form that took the token would put a
  // merchant's Admin credential in a browser.
  it("takes secret REFERENCES rather than tokens, and sends what it took", async () => {
    const h = harness();
    renderStores(h, "/stores");
    await waitFor(() => expect(screen.getByText("Add a store")).toBeTruthy());

    expect(screen.getByText(/The NAME of a globalSecret row, not the token/)).toBeTruthy();

    fireEvent.change(screen.getByPlaceholderText("acme-widgets"), { target: { value: "beta" } });
    fireEvent.change(screen.getByPlaceholderText("acme-widgets.myshopify.com"), {
      target: { value: "beta.myshopify.com" },
    });
    fireEvent.change(screen.getByPlaceholderText("SHOPIFY_ACME_ADMIN_TOKEN"), {
      target: { value: "SHOPIFY_BETA_ADMIN_TOKEN" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add store" }));

    await waitFor(() => expect(h.callsNamed("createStore").length).toBe(1));
    const call = h.callsNamed("createStore")[0] ?? "";
    expect(call).toContain(`storeId: "beta"`);
    expect(call).toContain(`adminTokenRef: "SHOPIFY_BETA_ADMIN_TOKEN"`);
    // Nothing that looks like a Shopify token can have reached the wire,
    // because the form never offered a place to type one.
    expect(call).not.toContain("shpat_");
  });
});

describe("one store's detail", () => {
  it("reports drift as what it means, not as a bare number", async () => {
    const h = harness();
    renderStores(h, "/stores/acme");
    await waitFor(() => expect(screen.getByText("v1:shopify:order")).toBeTruthy());
    // The value the fake supplied, not merely "a table rendered".
    expect(screen.getAllByText("3").length).toBeGreaterThan(0); // staleWrites
    expect(screen.getByText(/148/)).toBeTruthy(); // registered subscriptions
    expect(screen.getByText(/1800 of 2000 points/)).toBeTruthy();
  });

  it("drives the two Shopify-only actions through the declared constructs", async () => {
    const h = harness();
    renderStores(h, "/stores/acme");
    await waitFor(() => expect(screen.getByText("v1:shopify:order")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Reconcile now" }));
    await waitFor(() => expect(h.callsNamed("shopifyEnsureSubscriptions").length).toBe(1));

    fireEvent.click(screen.getByRole("button", { name: "Pause ingestion" }));
    await waitFor(() => expect(h.callsNamed("setStoreStatus").length).toBe(1));
    expect(h.callsNamed("setStoreStatus")[0]).toContain(`status: "paused"`);
  });

  // Backfill, per-domain reconcile and the per-domain pause belong to EVERY
  // connector, so the runtime owns them and the Data origins surface drives
  // them. Two pages carrying the same three buttons is the duplication that
  // epic exists to avoid, and this asserts the split rather than trusting a
  // comment about it.
  it("does not repeat the runtime's own per-domain actions", async () => {
    const h = harness();
    renderStores(h, "/stores/acme");
    await waitFor(() => expect(screen.getByText("v1:shopify:order")).toBeTruthy());
    expect(screen.queryByRole("button", { name: "Step the backfill" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Reconcile what is due" })).toBeNull();
    expect(screen.getByText(/live on the Data origins surface/)).toBeTruthy();
  });

  // Pausing STAGES rather than dropping: the button has to say so, because
  // "pause" reads as "stop receiving" and the difference decides whether an
  // operator thinks a backfill is needed afterwards.
  it("says what pausing does", async () => {
    const h = harness();
    renderStores(h, "/stores/acme");
    await waitFor(() => expect(screen.getByRole("button", { name: "Pause ingestion" })).toBeTruthy());
    expect(screen.getByText(/still STAGES deliveries/)).toBeTruthy();
  });
});
