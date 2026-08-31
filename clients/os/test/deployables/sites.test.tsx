import { render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read and its provider dials a real
// websocket, so the hook is replaced rather than the provider mounted.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { SITE_CONCEPT } from "../../src/apps/deployables/concepts";
import { siteFingerprint, siteFromRow } from "../../src/apps/deployables/rows";
import {
  DELETED,
  DOCS,
  PORTAL,
  SHOP,
  click,
  emit,
  fakeConnection,
  siteRow,
  withSession,
  type FakeConnection,
} from "./harness";

// The Sites section and the detail panel, through the real LiveCollection.

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

function mount(
  connection: FakeConnection,
  opts: { section?: string; role?: string; userId?: string } = {},
) {
  h.connection = connection;
  return render(
    withSession(
      <DeployablesApp
        sectionId={opts.section ?? "sites"}
        navigate={vi.fn()}
        askContext={vi.fn()}
        store={memStore()}
      />,
      { role: opts.role ?? "owner", userId: opts.userId ?? "u-me" },
    ),
  );
}

beforeEach(() => {
  h.connection = null;
});

describe("the deployables list", () => {
  it("reads sitesAll and renders a row per admitted deployable", async () => {
    const connection = fakeConnection({ sites: [PORTAL, SHOP, DOCS] });
    mount(connection);

    await screen.findByText("shop.memql.example.com");
    expect(screen.getByText("portal.memql.example.com")).toBeTruthy();
    expect(screen.getByText("docs.memql.example.com")).toBeTruthy();
    // The generated builder ran, and this is the text that reached the wire.
    expect(connection.calls).toContain("query sitesAll()");
  });

  it("EXCLUDES a soft-deleted row", async () => {
    // `sitesAll` carries isNotDeleted, but a soft delete arrives as an UPDATE,
    // so the same predicate has to hold on the client or the row vanishes on a
    // reseed and comes back on the next event.
    const connection = fakeConnection({ sites: [SHOP, DELETED] });
    mount(connection);
    await screen.findByText("shop.memql.example.com");
    expect(screen.queryByText("gone.memql.example.com")).toBeNull();
  });

  it("labels an EMPTY ownerUserId cluster-owned, and the viewer's own rows as theirs", async () => {
    const connection = fakeConnection({ sites: [PORTAL, SHOP] });
    mount(connection);

    const portal = (await screen.findByText("portal.memql.example.com")).closest(".os-row");
    const shop = screen.getByText("shop.memql.example.com").closest(".os-row");
    expect(within(portal as HTMLElement).getByText("cluster-owned")).toBeTruthy();
    expect(within(shop as HTMLElement).getByText("yours")).toBeTruthy();
  });

  it("labels each bundle reference by its usage form", async () => {
    const connection = fakeConnection({ sites: [PORTAL, SHOP, DOCS] });
    mount(connection);
    await screen.findByText("shop.memql.example.com");
    expect(screen.getByText("baked portal")).toBeTruthy();
    expect(screen.getByText("uploaded bundle")).toBeTruthy();
    expect(screen.getByText("baked site")).toBeTruthy();
  });

  it("renders the feed's own state rather than an empty cluster", async () => {
    // A list with no rows and no connection must not read as "no deployables".
    h.connection = null;
    render(
      withSession(
        <DeployablesApp sectionId="sites" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} />,
      ),
    );
    expect(await screen.findByText("Not connected to the cluster")).toBeTruthy();
  });
});

describe("the arrival cue", () => {
  it("PULSES a row whose bundle flipped somewhere else", async () => {
    // The epic's headline: a CI publish through POST /sites/{id}/bundles lands
    // on a node nobody here is talking to, `graph.node.updated` broadcasts, and
    // the row announces itself.
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    await screen.findByText("shop.memql.example.com");

    await emit(connection, SITE_CONCEPT, { ...SHOP, bundleRef: "blob://sites/site-shop/v9/" });

    await waitFor(() => {
      const row = document.querySelector("[data-arrival='updated']");
      expect(row).not.toBeNull();
    });
  });

  it("rises and ticks for a deployable that did not exist a moment ago", async () => {
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    await screen.findByText("shop.memql.example.com");

    await emit(
      connection,
      SITE_CONCEPT,
      siteRow({ id: "site-new", hostname: "new.memql.example.com" }),
      "NODE_CREATED",
    );

    expect(await screen.findByText("new.memql.example.com")).toBeTruthy();
    await waitFor(() => {
      expect(document.querySelector("[data-arrival='added']")).not.toBeNull();
    });
    expect(screen.getByText("new")).toBeTruthy();
  });
});

describe("one deployable, in full", () => {
  async function openShop() {
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    await click((await screen.findByText("shop.memql.example.com")).closest("button"));
    return connection;
  }

  it("shows every row fact", async () => {
    await openShop();
    const panel = await screen.findByRole("region", {
      name: "Deployable shop.memql.example.com",
    });
    const facts = within(panel);
    expect(facts.getByText("Hostname")).toBeTruthy();
    expect(facts.getByText("blob://sites/site-shop/v1/")).toBeTruthy();
    expect(facts.getByText("uploaded bundle")).toBeTruthy();
    expect(facts.getByText("artifact-zip")).toBeTruthy();
    expect(facts.getByText("Published from the Library.")).toBeTruthy();
  });

  it("names the storefront's secret and NEVER fetches its value", async () => {
    const connection = await openShop();
    const panel = await screen.findByRole("region", {
      name: "Deployable shop.memql.example.com",
    });
    expect(within(panel).getByText("example.myshopify.com")).toBeTruthy();
    expect(within(panel).getByText("shopify-storefront-token")).toBeTruthy();
    // The token itself lives in a globalSecret. Nothing here reads one.
    expect(connection.calls.some((c) => c.includes("globalSecret"))).toBe(false);
    expect(connection.calls.some((c) => c.toLowerCase().includes("secret"))).toBe(false);
  });

  it("links to where the deployable actually is, over https", async () => {
    await openShop();
    const link = await screen.findByRole("link", { name: /Open/ });
    expect(link.getAttribute("href")).toBe("https://shop.memql.example.com/");
    expect(link.getAttribute("rel")).toContain("noopener");
  });

  it("MARKS a bundle that flipped while it was open", async () => {
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    await click((await screen.findByText("shop.memql.example.com")).closest("button"));
    await screen.findByText("blob://sites/site-shop/v1/");
    expect(screen.queryByText("changed just now")).toBeNull();

    await emit(connection, SITE_CONCEPT, { ...SHOP, bundleRef: "blob://sites/site-shop/v9/" });

    expect(await screen.findByText("changed just now")).toBeTruthy();
    expect(screen.getByText("blob://sites/site-shop/v9/")).toBeTruthy();
  });

  it("does NOT mark a change that left the bundle alone", async () => {
    // An `updated` tick fires for a rename too. What is claimed here is
    // specific, so what is watched is specific.
    const connection = fakeConnection({ sites: [SHOP] });
    mount(connection);
    await click((await screen.findByText("shop.memql.example.com")).closest("button"));
    await screen.findByText("blob://sites/site-shop/v1/");

    await emit(connection, SITE_CONCEPT, { ...SHOP, title: "Renamed" });

    await waitFor(() => expect(screen.getByText("Renamed")).toBeTruthy());
    expect(screen.queryByText("changed just now")).toBeNull();
  });
});

describe("reachable positives", () => {
  it("the fixtures really do exercise the branches asserted above", async () => {
    // Without this, "no secret call was made" and "cluster-owned is rendered"
    // are statements about a page that rendered nothing.
    const connection = fakeConnection({ sites: [PORTAL, SHOP, DOCS] });
    mount(connection);
    await screen.findByText("shop.memql.example.com");
    expect(document.querySelectorAll(".os-row").length).toBe(3);
    expect(connection.calls.length).toBeGreaterThan(0);
  });
});

describe("the list and the map agree about what counts as news", () => {
  it("share one fingerprint, so a publish cannot pulse on one surface and not the other", () => {
    const before = siteFromRow(SHOP);
    const after = siteFromRow({ ...SHOP, bundleRef: "blob://sites/site-shop/v9/" });
    expect(siteFingerprint(before)).not.toBe(siteFingerprint(after));
    // ...and a field nobody would call a change does not move it.
    expect(siteFingerprint(siteFromRow({ ...SHOP, notes: "scratch" }))).toBe(
      siteFingerprint(before),
    );
  });
});

describe("the density setting is not decorative", () => {
  it("reaches the DOM, so the stylesheet has something to select on", async () => {
    // A preference nothing renders is a preference that does nothing, and a
    // settings panel that offers one is worse than a panel that does not.
    const data = new Map<string, string>();
    const store = new LocalDeployablesSettingsStore({
      getItem: (k) => data.get(k) ?? null,
      setItem: (k, v) => void data.set(k, v),
    });
    store.save({ version: 1, defaultSection: "sites", density: "compact" });

    h.connection = fakeConnection({ sites: [SHOP] });
    render(
      withSession(
        <DeployablesApp sectionId="sites" navigate={vi.fn()} askContext={vi.fn()} store={store} />,
      ),
    );
    await screen.findByText("shop.memql.example.com");
    expect(document.querySelector('.os-app-stack[data-density="compact"]')).not.toBeNull();
  });
});
