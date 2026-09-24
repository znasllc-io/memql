import { render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read and its provider dials a real
// websocket, so the hook is replaced rather than the provider mounted.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import {
  DEV_STORE,
  SHOP,
  STORE,
  click,
  domainStateRow,
  emit,
  fakeConnection,
  previewReadinessRow,
  siteRow,
  storeHealthRow,
  withSession,
  type FakeConnection,
  type FakeSeed,
} from "./harness";

// THE STORE, ON THE DEPLOYABLE THAT FRONTS IT (epic memql#5530, issue
// memql#5541).
//
// ===========================================================================
// WHAT THIS FILE INHERITED, AND WHY IT WAS NOT REWRITTEN
// ===========================================================================
// Four of these cases are the deleted Stores app's own, moved rather than
// re-derived: the per-domain acts it refuses to carry, the lifecycle acts it
// never draws disabled, the absences it refuses to render as zeroes, and the
// sentence naming where the acts it does not carry actually live. Those were
// the substance of that surface, and an epic that re-homes a surface has to
// carry its substance or it has deleted a feature and called it a move.
//
// ===========================================================================
// THE STORE IS A CONNECTION, NOT A BUILD SETTING
// ===========================================================================
// It used to be a chip inside App configuration pointing at the What-it-is
// stop, which is the build report. The first case here is the assertion that
// it is not, in both directions: present in the connections column, absent
// from the build stop.

function memStore(): LocalDeployablesSettingsStore {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  });
}

function mount(connection: FakeConnection, opts: { role?: string } = {}) {
  h.connection = connection;
  return render(
    withSession(
      <DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} />,
      { role: opts.role ?? "owner", userId: "u-me" },
    ),
  );
}

/**
 * The Store slot, found by its own label rather than by an accessible-name
 * match.
 *
 * The slot label is the `<strong>`. Return null rather than throwing,
 * because absence is asserted for viewers without store access.
 */
function storeSlot(page: HTMLElement): HTMLElement | null {
  return (
    Array.from(page.querySelectorAll<HTMLElement>("button.deployable-slot")).find(
      (button) => button.querySelector("strong")?.textContent === "Store",
    ) ?? null
  );
}

async function openDeployable(hostname: string): Promise<HTMLElement> {
  await waitFor(() =>
    expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"),
  );
  await click((await screen.findByText(hostname)).closest("button"));
  return (await screen.findByRole("region", { name: `Deployable ${hostname}` })).closest(
    "[data-deployable-view]",
  ) as HTMLElement;
}

/** Opens a storefront and then its Store pane. */
async function openStore(seed: FakeSeed, opts: { role?: string } = {}) {
  const connection = fakeConnection(seed);
  mount(connection, opts);
  const page = await openDeployable("shop.memql.example.com");
  const slot = storeSlot(page);
  await click(slot);
  const pane = (await screen.findByRole("region", {
    name: "Store for shop.memql.example.com",
  })) as HTMLElement;
  return { connection, pane };
}

const BOUND: FakeSeed = {
  sites: [SHOP],
  stores: [STORE, DEV_STORE],
  storeHealth: [storeHealthRow({ storeId: "store-example", domain: "example.myshopify.com" })],
};

describe("the store is a connection on the deployable, not a build setting", () => {
  it("draws a Store slot in the connections column", async () => {
    const connection = fakeConnection(BOUND);
    mount(connection);
    const page = await openDeployable("shop.memql.example.com");
    const slot = storeSlot(page);
    expect(slot).not.toBeNull();
    // It reads the store, so the slot names the DOMAIN rather than an opaque
    // row id: the domain is what somebody came to check.
    await waitFor(() => expect(slot?.textContent).toContain("example.myshopify.com"));
  });

  it("carries no Shopify chip under App configuration", async () => {
    const connection = fakeConnection(BOUND);
    mount(connection);
    const page = await openDeployable("shop.memql.example.com");
    for (const chip of Array.from(page.querySelectorAll(".deployable-piece-chip"))) {
      expect(chip.textContent ?? "").not.toContain("myshopify");
      expect(chip.textContent ?? "").not.toContain("Shopify binding");
    }
  });

  it("is absent, not disabled, for somebody whose grants do not reach it", async () => {
    // `execute app:deployables/store` is seeded on owner and developer
    // (Connect Shopify, D3), not on a reader. DESIGN.md rule 12: a control the
    // effective set does not hold is ABSENT -- and here the row tier would
    // serve a reader nothing anyway, so a slot would be a refusal rendered as
    // an empty panel.
    const connection = fakeConnection(BOUND);
    mount(connection, { role: "reader" });
    const page = await openDeployable("shop.memql.example.com");
    expect(storeSlot(page)).toBeNull();
  });
});

const unbound = siteRow({
  id: "site-unbound",
  hostname: "new.memql.example.com",
  kind: "shopify_storefront",
  status: "draft",
  bundleRef: "blob://sites/site-unbound/pending/",
  binding: {},
});

describe("a storefront with no store", () => {
  it("reads Not connected", async () => {
    const connection = fakeConnection({ sites: [unbound], stores: [STORE] });
    mount(connection);
    const page = await openDeployable("new.memql.example.com");
    expect(storeSlot(page)?.textContent).toContain("Not connected");
  });

  // A BUILT STOREFRONT WITH NO STORE IS NOT OFFERED GO LIVE (Connect Shopify,
  // D5). The first deploy of a storefront whose store could not be attached
  // lands here, and the engine refuses taking it live -- so the bar draws no
  // Go live, and the page draws the engine's sentence with the way to the
  // store, where the act that clears it is.
  it("offers no Go live, and says why beside the way to the store", async () => {
    const built = { ...unbound, bundleRef: "blob://sites/site-unbound/v1/" } as typeof unbound;
    const connection = fakeConnection({
      sites: [built],
      stores: [STORE],
      previewReadiness: {
        "site-unbound": previewReadinessRow({
          siteId: "site-unbound",
          status: "draft",
          canGoLive: false,
          goLiveRefusal: {
            code: "storefront_not_connected",
            message: "this storefront is not attached to a Shopify store, so it has no catalog to put in front of shoppers.",
            remedy: "Connect Shopify on the Store panel.",
          },
        } as never),
      },
    });
    mount(connection);
    await openDeployable("new.memql.example.com");
    expect(await screen.findByText(/not attached to a Shopify store/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Go live/ })).toBeNull();
    await click(screen.getByRole("button", { name: "Open the store" }));
    expect(await screen.findByRole("region", { name: "Store for new.memql.example.com" })).toBeTruthy();
  });

  // THE ANSWER FOLLOWS THE BINDING. Readiness is read once, so it has to be
  // read again when what it judges changes: attaching a store on the Store
  // panel and coming back must offer Go live without a reload, and the old
  // storefront_not_connected sentence must be gone.
  it("offers Go live once a store is attached and the person comes back", async () => {
    const built = { ...unbound, title: "New shop", bundleRef: "blob://sites/site-unbound/v1/" } as typeof unbound;
    const previewReadiness: Record<string, ReturnType<typeof previewReadinessRow>> = {
      "site-unbound": previewReadinessRow({
        siteId: "site-unbound",
        status: "draft",
        canGoLive: false,
        goLiveRefusal: {
          code: "storefront_not_connected",
          message: "this storefront is not attached to a Shopify store, so it has no catalog to put in front of shoppers.",
          remedy: "Connect Shopify on the Store panel.",
        },
      } as never),
    };
    const connection = fakeConnection({ sites: [built], stores: [STORE], previewReadiness });
    mount(connection);
    const page = await openDeployable("new.memql.example.com");
    expect(await screen.findByText(/not attached to a Shopify store/)).toBeTruthy();

    await click(storeSlot(page));
    const pane = await screen.findByRole("region", { name: "Store for new.memql.example.com" });
    const label = await within(pane).findByText("example.myshopify.com");
    await click(label.closest<HTMLElement>('[role="radio"]'));
    // What the engine answers once the store is bound, and the row's own
    // event carrying the binding back, as the cluster sends it.
    previewReadiness["site-unbound"] = previewReadinessRow({
      siteId: "site-unbound",
      status: "draft",
      storeId: "store-example",
      canGoLive: true,
    } as never);
    await click(within(pane).getByRole("button", { name: "Attach" }));
    await emit(connection, "v1:platform:site", { ...built, binding: { storeId: "store-example" } });

    await click(screen.getByRole("button", { name: "Back to New shop" }));
    const back = (await screen.findByRole("region", { name: "Deployable new.memql.example.com" })).closest(
      "[data-deployable-view]",
    ) as HTMLElement;
    await waitFor(() => expect(storeSlot(back)?.textContent).not.toContain("Not connected"));
    expect(await screen.findByRole("button", { name: /^Go live/ })).toBeTruthy();
    expect(screen.queryByText(/not attached to a Shopify store/)).toBeNull();
  });

  it("opens straight into the picker rather than an empty panel", async () => {
    const connection = fakeConnection({ sites: [unbound], stores: [STORE] });
    mount(connection);
    const page = await openDeployable("new.memql.example.com");
    await click(storeSlot(page));
    const pane = await screen.findByRole("region", { name: "Store for new.memql.example.com" });
    expect(await within(pane).findByText("example.myshopify.com")).toBeTruthy();
  });

  it("names the store rather than copying it", async () => {
    const connection = fakeConnection({ sites: [unbound], stores: [STORE] });
    mount(connection);
    const page = await openDeployable("new.memql.example.com");
    await click(storeSlot(page));
    const pane = await screen.findByRole("region", { name: "Store for new.memql.example.com" });
    // THE LABEL, THEN THE CHOICE IT NAMES. A choice's accessible name is its
    // label AND its description -- the domain, then "Example Shop · live ·
    // Shopify Plus" -- so asking findByRole for a `name` means matching PART
    // of a longer string, and every spelling of that (an unanchored
    // /example\.myshopify\.com/, a `.includes`) reads to a scanner as a URL
    // check with arbitrary hosts free to sit either side of it. Both spellings
    // were tried and each raised its own alert. The domain is a WHOLE text
    // node -- ChoiceStack renders the label in its own span inside the button
    // that carries role="radio" -- so matching it exactly and walking up to
    // the choice asserts the same thing with nothing partial anywhere in it.
    const label = await within(pane).findByText("example.myshopify.com");
    await click(label.closest<HTMLElement>('[role="radio"]'));
    await click(within(pane).getByRole("button", { name: "Attach" }));
    const call = connection.callsNamed("updateSiteStoreBinding")[0] ?? "";
    expect(call).toContain('storeId: "store-example"');
    // THE ROW ID AND NOTHING ELSE. A domain or a token reference written here
    // would be the second record of a store this epic exists to end.
    expect(call).not.toContain("myshopify.com");
    expect(call).not.toContain("TokenRef");
  });
});

describe("what this surface deliberately does not carry", () => {
  it("offers no per-domain act, and names where they live", async () => {
    const { pane } = await openStore({
      ...BOUND,
      storeHealth: [
        storeHealthRow({
          storeId: "store-example",
          domains: [domainStateRow({ concept: "v1:shopify:product", driftLast: 3 })],
        }),
      ],
    });
    // Backfill, retry, discard and the per-domain pause belong to EVERY
    // connector, so the data-origins runtime owns them. Two surfaces carrying
    // the same three buttons is the duplication this split exists to avoid,
    // and the two copies would disagree the first time one learned something.
    for (const forbidden of [/backfill/i, /retry/i, /discard/i, /dead letter/i]) {
      expect(within(pane).queryByRole("button", { name: forbidden })).toBeNull();
    }
    await waitFor(() => expect(within(pane).getByText(/live in Cluster . Data origins/)).toBeTruthy());
  });

  it("says that reconciling subscriptions is wider than this store", async () => {
    const { pane } = await openStore(BOUND);
    expect(await within(pane).findByRole("button", { name: /Reconcile subscriptions/ })).toBeTruthy();
    expect(within(pane).getByText(/Reconciles every ingesting store, not only this one/)).toBeTruthy();
  });
});

describe("the store's own acts", () => {
  it("never renders a disabled lifecycle act", async () => {
    const { pane } = await openStore(BOUND);
    const pause = await within(pane).findByRole("button", { name: /Pause ingestion/ });
    expect(pause.hasAttribute("disabled")).toBe(false);
    // A live store gets Pause and NOT Resume -- an act is a claim about the
    // state it acts from.
    expect(within(pane).queryByRole("button", { name: /Resume ingestion/ })).toBeNull();
  });

  it("offers Resume, and only Resume, on a paused store", async () => {
    const { pane } = await openStore({
      ...BOUND,
      storeHealth: [storeHealthRow({ storeId: "store-example", status: "paused" })],
    });
    expect(await within(pane).findByRole("button", { name: /Resume ingestion/ })).toBeTruthy();
    expect(within(pane).queryByRole("button", { name: /Pause ingestion/ })).toBeNull();
  });

  it("offers NEITHER for a status this shell has no copy for", async () => {
    const { pane } = await openStore({
      ...BOUND,
      storeHealth: [storeHealthRow({ storeId: "store-example", status: "reticulating" })],
    });
    // TWICE: the identity line's status word and the sentence that says this
    // shell has no copy for it. Both are the honest reading -- name the value
    // rather than assert a state -- so this asks for at least one.
    await waitFor(() => expect(within(pane).getAllByText(/reticulating/).length).toBeGreaterThan(0));
    expect(within(pane).queryByRole("button", { name: /Pause ingestion/ })).toBeNull();
    expect(within(pane).queryByRole("button", { name: /Resume ingestion/ })).toBeNull();
  });
});

describe("absences are absences", () => {
  it("never renders an unreported cost bucket as a bucket at zero", async () => {
    const { pane } = await openStore(BOUND);
    // A store nothing has called reports no bucket at all. Coercing that to
    // zeroes renders a store at its rate limit -- the most alarming reading
    // this panel can show -- for a store that is simply idle.
    await waitFor(() => expect(within(pane).getByText(/not observed yet/)).toBeTruthy());
    expect(within(pane).queryByText(/0 of 0 points/)).toBeNull();
  });

  it("says no reconcile has been recorded rather than showing zero of zero", async () => {
    const { pane } = await openStore(BOUND);
    await waitFor(() =>
      expect(within(pane).getByText(/No subscription reconcile has been recorded/)).toBeTruthy(),
    );
  });

  it("says nothing has synced rather than drawing an empty table", async () => {
    const { pane } = await openStore(BOUND);
    await waitFor(() =>
      expect(within(pane).getByText(/No domain has synced yet/)).toBeTruthy(),
    );
  });
});

describe("the development store", () => {
  it("shows the one that stands in for this store", async () => {
    const { pane } = await openStore(BOUND);
    await waitFor(() =>
      expect(within(pane).getByText("example-dev.myshopify.com")).toBeTruthy(),
    );
  });

  it("says so when none does", async () => {
    const { pane } = await openStore({ ...BOUND, stores: [STORE] });
    await waitFor(() =>
      expect(within(pane).getByText(/No development store stands in for this one/)).toBeTruthy(),
    );
  });
});

describe("the credential references", () => {
  it("shows the NAME of each secret and fetches no value", async () => {
    const { connection, pane } = await openStore(BOUND);
    await waitFor(() => expect(within(pane).getByText("EXAMPLE_STOREFRONT_TOKEN")).toBeTruthy());
    expect(within(pane).getByText("EXAMPLE_ADMIN_TOKEN")).toBeTruthy();
    expect(within(pane).getByText("EXAMPLE_WEBHOOK_SECRET")).toBeTruthy();
    // THE CONTROL: nothing anywhere resolves one. The edge dereferences the
    // Storefront reference at serve time into the runtime-config document,
    // and that is the only place any of the three is resolved.
    expect(connection.calls.some((c) => c.toLowerCase().includes("secret("))).toBe(false);
  });
});

describe("a binding that does not resolve", () => {
  it("says the store is not on this cluster rather than showing it unbound", async () => {
    // A STORE THAT IS GONE AND A STOREFRONT NOBODY BOUND ARE DIFFERENT
    // ANSWERS. Drawing the first as the second hides a real misconfiguration
    // behind a state that looks deliberate.
    const { pane } = await openStore({ sites: [SHOP], stores: [], storeHealth: [] });
    await waitFor(() =>
      expect(within(pane).getByText(/names a store that is not on this cluster/)).toBeTruthy(),
    );
  });
});

describe("who is drawn which store act (Connect Shopify, D3 and D15)", () => {
  // THE STORE PART IS A DEVELOPER'S TOO, and it is the whole of what a
  // developer holds here. It attaches a store and changes which one a
  // storefront fronts. REGISTERING a store creates a v1:shopify:store row,
  // which the engine refuses below a cluster owner (D15), and PAUSING or
  // RESUMING one writes a store row that exists, which stays owner-only.
  // RECONCILING SUBSCRIPTIONS is an owner's too: shopifyEnsureSubscriptions
  // takes no store and walks every ingesting store on the cluster, so it is
  // not an act on the storefront a developer attaches. An act that is not
  // legal is absent, never disabled (DESIGN.md rule 12).
  async function openPicker(role: string) {
    const connection = fakeConnection({ sites: [unbound], stores: [STORE] });
    mount(connection, { role });
    const page = await openDeployable("new.memql.example.com");
    expect(storeSlot(page)?.textContent).toContain("Not connected");
    await click(storeSlot(page));
    const pane = await screen.findByRole("region", { name: "Store for new.memql.example.com" });
    const label = await within(pane).findByText("example.myshopify.com");
    await click(label.closest<HTMLElement>('[role="radio"]'));
    return pane;
  }

  it("draws a developer the slot and the attach, and not the register form", async () => {
    const pane = await openPicker("developer");
    expect(within(pane).getByRole("button", { name: "Attach" })).toBeTruthy();
    expect(within(pane).queryByRole("button", { name: "Register a store" })).toBeNull();
  });

  it("draws a developer no pause or resume, and still the change of store", async () => {
    const { pane } = await openStore(BOUND, { role: "developer" });
    await waitFor(() => expect(within(pane).getByText(/scopes the mirror needs are granted/)).toBeTruthy());
    expect(within(pane).getByRole("button", { name: /Change the store/ })).toBeTruthy();
    expect(within(pane).queryByRole("button", { name: /Pause ingestion/ })).toBeNull();
    expect(within(pane).queryByRole("button", { name: /Resume ingestion/ })).toBeNull();
  });

  it("draws a developer no subscription reconcile, which walks every store on the cluster", async () => {
    const { pane } = await openStore(BOUND, { role: "developer" });
    await waitFor(() => expect(within(pane).getByText(/scopes the mirror needs are granted/)).toBeTruthy());
    expect(within(pane).queryByRole("button", { name: /Reconcile subscriptions/ })).toBeNull();
  });

  it("draws an owner the attach, the register form, pause and the reconcile", async () => {
    const pane = await openPicker("owner");
    expect(within(pane).getByRole("button", { name: "Attach" })).toBeTruthy();
    expect(within(pane).getByRole("button", { name: "Register a store" })).toBeTruthy();

    const { pane: bound } = await openStore(BOUND, { role: "owner" });
    expect(await within(bound).findByRole("button", { name: /Pause ingestion/ })).toBeTruthy();
    expect(within(bound).getByRole("button", { name: /Reconcile subscriptions/ })).toBeTruthy();
  });
});
