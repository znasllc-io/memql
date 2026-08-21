// The sites surface (memql#3717), end to end against a fake cluster.
//
// WHAT THIS FILE OWNS. component/memql's Go tests prove the write guard
// actually refuses a systemOwned delete server-side
// (platform_site_delete_guard_test.go) and the dslconformance suite proves
// the DSL constructs are shaped correctly; neither can see what the browser
// sends or renders. This file asserts the wire form -- that the UI issues
// the named calls the DSL declares, with the arguments quoted -- and the
// three honesty properties the issue's own verification section turns on:
// a non-owner sees an EXPLANATION rather than an empty table, the kind
// selector is driven by the concept's DECLARED enum rather than a
// hardcoded pair, and the rollback picker offers a version that is
// genuinely a DIFFERENT, older row rather than the current one repeated.
// siteHistory.test.ts covers the version-walk LOGIC in isolation; this file
// covers that the picker is actually wired to it.
//
// EVERY TEST HERE IS WRITTEN TO FAIL ON THE VACUOUS PASS. A table that is
// merely empty is not the same claim as "explained to a non-owner", so the
// access test asserts the explanation text AND that the page body (create
// form, "Sites" heading) is absent. A rollback list with two identical rows
// would not exercise the walk, so the history test asserts the prior
// version's bundleRef/createdAt are DIFFERENT from the current one's, not
// merely that "a second item" rendered. A kind selector offering exactly
// spa/static would pass a lazy assertion just as well as a hardcoded
// constant would; the fixture schema here carries a third value ("server",
// the real followon epic #3718) so only a schema-driven selector can offer
// it.

import { describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import {
  Result,
  type AccessSummary,
  type Concept,
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

const SITE = "v1:platform:site";

// The REAL display card from dsl/platform/concepts.memql, because that is
// what RowList composes against -- see components/RowList.tsx's header on
// why nothing here may fork or reimplement that rendering.
const CONCEPTS: Concept[] = [
  {
    id: SITE,
    version: "v1",
    domain: "platform",
    entity: "site",
    type: "concept",
    description: "A web surface this cluster serves at a hostname",
    displayCard: { primary: "hostname", secondary: "kind", tertiary: "title", status: "status" },
  },
];

// Three kind values, not the two the create form happens to offer today --
// see the file header on why that is the property under test.
const SITE_SCHEMA = {
  type: "object",
  required: ["hostname", "kind", "bundleRef", "status"],
  properties: {
    hostname: { type: "string" },
    kind: { type: "string", enum: ["spa", "static", "server"] },
    bundleRef: { type: "string" },
    status: { type: "string", enum: ["draft", "live", "disabled"] },
    apiProxy: { type: "boolean" },
    systemOwned: { type: "boolean" },
    title: { type: "string" },
    notes: { type: "string" },
  },
};

// Wire-shaped node: payload NESTED alongside the intrinsics, matching what a
// generic browse (rawNodes()) returns and what a shaped named-query result
// (rows(), which flattens payload onto the top level and drops schema/type)
// collapses to. Both paths are exercised by different reads in this feature.
function node(id: string, payload: Record<string, unknown>, createdAt: string): Row {
  return {
    id,
    concept: SITE,
    type: "concept",
    createdBy: "system",
    createdAt,
    schema: SITE_SCHEMA,
    payload,
  };
}

const PORTAL_ROW = node(
  "site-portal",
  {
    hostname: "cockpit.example.com",
    kind: "spa",
    bundleRef: "file:///app/portal",
    status: "live",
    apiProxy: false,
    systemOwned: true,
    title: "Portal",
    notes: "",
  },
  "2026-08-01T00:00:00.000Z",
);

const SHOP_CURRENT = {
  bundleRef: "blob://sites/shop/v2/",
  createdAt: "2026-08-10T12:00:00.000Z",
  status: "live",
};
const SHOP_PRIOR = {
  bundleRef: "blob://sites/shop/v1/",
  createdAt: "2026-08-05T09:00:00.000Z",
  status: "live",
};

function shopPayload(version: typeof SHOP_CURRENT): Record<string, unknown> {
  return {
    hostname: "shop.example.com",
    kind: "static",
    bundleRef: version.bundleRef,
    status: version.status,
    apiProxy: false,
    systemOwned: false,
    title: "Shop",
    notes: "",
  };
}

const SHOP_ROW_CURRENT = node("site-shop", shopPayload(SHOP_CURRENT), SHOP_CURRENT.createdAt);

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
  emit: (event: Event) => void;
}

function harness(overrides: { role?: Role } = {}): Harness {
  const calls: string[] = [];
  let handler: ((event: Event) => void) | null = null;

  const access: AccessSummary = {
    requestId: "r1",
    userId: "user-1",
    primaryEmail: "owner@example.com",
    clusterRole: overrides.role ?? "owner",
  };

  const executeNamed = vi.fn(async (_name: string, call: string) => {
    calls.push(call);

    // The generic browse behind useConceptRows -- SitesPage's live list AND
    // the source the kind selector reads its schema from.
    if (call.startsWith("sort(paginate(concept==")) {
      if (!call.includes(SITE)) return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
      return new Result({ bundle: { nodes: [PORTAL_ROW, SHOP_ROW_CURRENT] }, meta: { cursor: "" } });
    }

    // The rollback walk's asOf-wrapped reads (sites/calls.ts's fetchSiteAsOf).
    const asOfMatch = /^asOf\(siteById\(siteId: "([^"]*)"\), "([^"]+)"\)$/.exec(call);
    if (asOfMatch) {
      const [, siteId, at] = asOfMatch;
      if (siteId === "site-shop" && at !== undefined && at < SHOP_CURRENT.createdAt) {
        return new Result({
          bundle: { nodes: [node("site-shop", shopPayload(SHOP_PRIOR), SHOP_PRIOR.createdAt)] },
        });
      }
      return new Result({ bundle: { nodes: [] } });
    }

    // The plain (unwrapped) siteById read -- the current row.
    const plainMatch = /^query siteById\(siteId: "([^"]*)"\)$/.exec(call);
    if (plainMatch) {
      const siteId = plainMatch[1];
      if (siteId === "site-portal") return new Result({ bundle: { nodes: [PORTAL_ROW] } });
      if (siteId === "site-shop") return new Result({ bundle: { nodes: [SHOP_ROW_CURRENT] } });
      return new Result({ bundle: { nodes: [] } });
    }

    // Mutations: nothing reads the reply (runMutation discards it), so an
    // empty envelope is enough.
    if (
      call.startsWith("mutation createSite(") ||
      call.startsWith("mutation updateSiteBundle(") ||
      call.startsWith("mutation updateSiteStatus(") ||
      call.startsWith("mutation deleteSite(")
    ) {
      return new Result({ bundle: { nodes: [] } });
    }

    return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
  });

  const query = asQueryClient({
    listConcepts: vi.fn(async () => CONCEPTS),
    getMyAccess: vi.fn(async () => access),
    executeNamed,
  });

  const subscriptions = {
    subscribeGraph: (fn: (event: Event) => void) => {
      handler = fn;
      return () => {
        handler = null;
      };
    },
  };

  return {
    query,
    subscriptions,
    calls,
    callsNamed: (construct: string) => calls.filter((c) => c.includes(`${construct}(`)),
    emit: (event: Event) => {
      act(() => {
        handler?.(event);
      });
    },
  };
}

function renderSites(h: Harness, path: string) {
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
          throw new Error("the sites tests must make no identity calls");
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

describe("the sites list", () => {
  it("lists sites through the shared row-list renderer, live", async () => {
    const h = harness();
    renderSites(h, "/sites");
    await waitFor(() => expect(screen.getAllByText("cockpit.example.com").length).toBeGreaterThan(0));
    expect(screen.getAllByText("shop.example.com").length).toBeGreaterThan(0);
  });

  // THE property that matters: not "spa and static appear" (a hardcoded
  // constant would pass that too) but that a THIRD, schema-only value is
  // offered. See the file header.
  it("renders the kind selector from the concept's declared enum, not a hardcoded pair", async () => {
    const h = harness();
    renderSites(h, "/sites");
    await waitFor(() => expect(screen.getAllByText("cockpit.example.com").length).toBeGreaterThan(0));

    const kindSelect = screen.getByRole("combobox");
    await waitFor(() => expect(within(kindSelect).queryByRole("option", { name: "server" })).toBeTruthy());
    expect(within(kindSelect).getByRole("option", { name: "spa" })).toBeTruthy();
    expect(within(kindSelect).getByRole("option", { name: "static" })).toBeTruthy();
  });

  it("creates a site through createSite and sees it appear live, without a refresh", async () => {
    const h = harness();
    renderSites(h, "/sites");
    await waitFor(() => expect(screen.getAllByText("cockpit.example.com").length).toBeGreaterThan(0));

    fireEvent.change(screen.getByPlaceholderText("shop.example.com"), {
      target: { value: "new.example.com" },
    });
    await waitFor(() =>
      expect(within(screen.getByRole("combobox")).queryByRole("option", { name: "server" })).toBeTruthy(),
    );
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "static" } });
    fireEvent.change(screen.getByPlaceholderText("blob://sites/shop/2026-08-13T00-00-00Z/"), {
      target: { value: "blob://sites/new/v1/" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create site" }));

    await waitFor(() => expect(h.callsNamed("createSite").length).toBe(1));
    const call = h.callsNamed("createSite")[0] ?? "";
    expect(call.startsWith("mutation createSite(")).toBe(true);
    expect(call).toContain('hostname: "new.example.com"');
    expect(call).toContain('kind: "static"');
    expect(call).toContain('bundleRef: "blob://sites/new/v1/"');

    // "Without a refresh" means the SUBSCRIPTION carries the new row, not a
    // manual reload -- so the test proves it by firing the CDC event the
    // engine would publish, exactly as conceptBrowser.test.tsx's live-update
    // tests do, rather than asserting on a reload the code must NOT call
    // (useSites.ts's header is explicit that createSite issues no reload()).
    h.emit({
      subscriptionId: "s",
      kind: "NODE_CREATED",
      timestamp: new Date(),
      payload: {
        id: "site-new",
        concept: SITE,
        hostname: "new.example.com",
        kind: "static",
        bundleRef: "blob://sites/new/v1/",
        status: "draft",
      },
    });

    await waitFor(() => expect(screen.getByText(/New since you opened this/)).toBeTruthy());
    expect(screen.getByText("new.example.com")).toBeTruthy();
  });
});

describe("access", () => {
  // The vacuous pass here is "the table is empty" -- true for a dead
  // connection, a genuinely site-less cluster, or a slow load, none of
  // which is this case. So this asserts the EXPLANATION text is present
  // AND that the ordinary page body (title, create form) is absent --
  // together they prove the refused branch rendered, not merely that
  // rows failed to appear.
  it("shows a non-owner an explanation, not an empty table", async () => {
    const h = harness({ role: "admin" });
    renderSites(h, "/sites");

    await waitFor(() => expect(screen.getByText(/cluster-owner surface/)).toBeTruthy());
    expect(screen.getByText(/resolved your role on this connection as admin/)).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Sites", level: 1 })).toBeNull();
    expect(screen.queryByRole("button", { name: "Create site" })).toBeNull();
  });

  it("shows the same explanation on the detail address for a non-owner", async () => {
    const h = harness({ role: "reader" });
    renderSites(h, "/sites/site-shop");
    await waitFor(() => expect(screen.getByText(/cluster-owner surface/)).toBeTruthy());
    expect(screen.getByText(/resolved your role on this connection as reader/)).toBeTruthy();
  });
});

describe("delete", () => {
  // The UI block is a courtesy -- component/memql/platform_site_delete_guard_test.go
  // proves the SERVER refuses regardless. This proves the courtesy control
  // actually reflects that: disabled AND explained, not merely present.
  it("disables delete for a systemOwned site and says why", async () => {
    const h = harness();
    renderSites(h, "/sites/site-portal");
    await waitFor(() => expect(screen.getByRole("heading", { name: "Portal" })).toBeTruthy());

    const deleteButton = screen.getByRole("button", { name: "Delete site" }) as HTMLButtonElement;
    expect(deleteButton.disabled).toBe(true);
    // "system-owned" also appears in the header subtitle badge, so this
    // checks the DELETE BAND'S OWN explanation specifically -- the sentence
    // that says WHY the control above it is disabled, not just that the
    // word appears somewhere on the page.
    expect(screen.getByText(/cannot be deleted/)).toBeTruthy();
    expect(screen.getByText(/re-seeded at boot/)).toBeTruthy();

    // Clicking a disabled control fires nothing -- the courtesy block is
    // not merely cosmetic on top of a live handler.
    fireEvent.click(deleteButton);
    expect(h.callsNamed("deleteSite").length).toBe(0);
  });

  it("deletes an ordinary site through deleteSite and returns to the list", async () => {
    const h = harness();
    renderSites(h, "/sites/site-shop");
    await waitFor(() => expect(screen.getByRole("heading", { name: "Shop" })).toBeTruthy());

    const deleteButton = screen.getByRole("button", { name: "Delete site" }) as HTMLButtonElement;
    expect(deleteButton.disabled).toBe(false);
    fireEvent.click(deleteButton);
    // Deleting confirms in a dialog that states what stops resolving (memql#4181).
    await waitFor(() => expect(screen.getByRole("dialog")).toBeTruthy());
    fireEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Delete site" }),
    );

    await waitFor(() => expect(h.callsNamed("deleteSite").length).toBe(1));
    expect(h.callsNamed("deleteSite")[0]).toBe('mutation deleteSite(siteId: "site-shop")');
    // The page title AND the list band both carry the label "Sites" once
    // back on the list screen, so this targets the level-1 page title
    // specifically -- proof of NAVIGATION, not just that the word appears.
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: "Sites", level: 1 })).toBeTruthy(),
    );
  });
});

describe("publish and rollback", () => {
  it("publishes a new bundle through updateSiteBundle", async () => {
    const h = harness();
    renderSites(h, "/sites/site-shop");
    await waitFor(() => expect(screen.getByText(SHOP_CURRENT.bundleRef)).toBeTruthy());

    fireEvent.change(screen.getByPlaceholderText(SHOP_CURRENT.bundleRef), {
      target: { value: "blob://sites/shop/v3/" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Publish" }));

    await waitFor(() => expect(h.callsNamed("updateSiteBundle").length).toBe(1));
    const call = h.callsNamed("updateSiteBundle")[0] ?? "";
    expect(call).toContain('siteId: "site-shop"');
    expect(call).toContain('bundleRef: "blob://sites/shop/v3/"');
  });

  // THE property that matters: the picker offers a version that is a
  // DIFFERENT row from the current one -- distinct bundleRef, distinct
  // timestamp -- not a second copy of the same data. See siteHistory.test.ts
  // for the walk logic itself; this proves the picker is wired to it and
  // that clicking it actually rolls back.
  it("offers a prior version distinct from the current one, and rolling back publishes it", async () => {
    const h = harness();
    renderSites(h, "/sites/site-shop");
    await waitFor(() => expect(screen.getByText(SHOP_CURRENT.bundleRef)).toBeTruthy());

    await waitFor(() => expect(screen.getByText(SHOP_PRIOR.bundleRef)).toBeTruthy());
    expect(screen.getByText(SHOP_PRIOR.bundleRef)).not.toBe(screen.getByText(SHOP_CURRENT.bundleRef));
    expect(screen.getByText(SHOP_PRIOR.createdAt)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Roll back to this" }));
    await waitFor(() => expect(h.callsNamed("updateSiteBundle").length).toBe(1));
    const call = h.callsNamed("updateSiteBundle")[0] ?? "";
    expect(call).toContain('siteId: "site-shop"');
    expect(call).toContain(`bundleRef: "${SHOP_PRIOR.bundleRef}"`);
  });

  it("changes status through updateSiteStatus", async () => {
    const h = harness();
    renderSites(h, "/sites/site-shop");
    await waitFor(() => expect(screen.getByText(SHOP_CURRENT.bundleRef)).toBeTruthy());

    fireEvent.change(screen.getByRole("combobox"), { target: { value: "disabled" } });

    await waitFor(() => expect(h.callsNamed("updateSiteStatus").length).toBe(1));
    expect(h.callsNamed("updateSiteStatus")[0]).toBe(
      'mutation updateSiteStatus(siteId: "site-shop", status: "disabled")',
    );
  });
});
