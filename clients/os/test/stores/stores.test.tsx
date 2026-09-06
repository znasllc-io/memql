import { act, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

// Type-only, so they are erased before the mock factories run.
import type { StoresSettingsStore } from "../../src/apps/stores/settings";
import type { OsAppSection } from "../../src/system/registry";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { StoresApp } = await import("../../src/apps/stores/StoresApp");
const { STORES_SECTIONS, STORES_SECTION_IDS, DEFAULT_STORES_SETTINGS, sanitizeStoresSettings } =
  await import("../../src/apps/stores/settings");
const { readingFor, apiVersionMismatch } = await import("../../src/apps/stores/words");
const { readStoreHealth } = await import("../../src/apps/stores/health");
const { fakeConnection, builtinReply, domainState, memStoresStore, storeHealth, withSession } =
  await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

async function click(el: Element) {
  await act(async () => {
    (el as HTMLElement).click();
  });
}

function mount(
  connection: Conn,
  {
    sectionId = "stores",
    store = memStoresStore(),
    navigate = vi.fn(),
  }: { sectionId?: string; store?: StoresSettingsStore; navigate?: ReturnType<typeof vi.fn> } = {},
) {
  h.connection = connection;
  const view = render(
    withSession(
      <StoresApp
        sectionId={sectionId}
        navigate={navigate}
        askContext={vi.fn()}
        store={store}
      />,
    ),
  );
  return { view, navigate };
}

/** Open one store's page from the list. */
async function openStore(domain: string) {
  await click(await screen.findByRole("button", { name: new RegExp(domain) }));
  return screen.getByRole("group", { name: "What you can do with this" });
}

const LIVE = storeHealth({
  storeId: "acme-widgets",
  domains: [
    domainState({ concept: "v1:shopify:order", phase: "idle", driftLast: 12, lastAppliedAt: "2026-09-05T09:00:00Z" }),
    domainState({ concept: "v1:shopify:product", phase: "idle", driftLast: 0 }),
  ],
  driftLast: 12,
  costBucket: { currentlyAvailable: 1834.5, maximumAvailable: 2000, restoreRate: 100 },
  health: { subscriptions: { desired: 40, existing: 38, failed: ["orders/edited"], at: "2026-09-05T03:15:00Z" } },
} as Partial<Row> & { storeId: string });

const PAUSED = storeHealth({ storeId: "quiet-shop", status: "paused" });

/** A store nothing has ever synced, called, or reconciled. Every figure on it
 *  is ABSENT, and the whole point of the fixture is that none of them is 0. */
const NEVER_RUN = storeHealth({ storeId: "fresh-shop", status: "configured" });

beforeEach(() => {
  h.connection = null;
});

// ---------------------------------------------------------------------------
// The manifest and the settings
// ---------------------------------------------------------------------------

describe("the Stores manifest", () => {
  it("floors the store sections at owner and the Logs section at admin", () => {
    // OWNER, NOT ADMIN. `v1:shopify:store` is clusterOwner-tier and both
    // declared reads carry actor.isClusterOwner as an explicit conjunct, so
    // below owner the list comes back EMPTY and every write is refused --
    // presentation over gates the engine holds, never the boundary itself.
    expect(STORES_SECTIONS.map((s) => [s.id, floorOf(s.roles)])).toEqual([
      ["stores", "owner"],
      ["logs", "admin"],
      ["settings", "owner"],
    ]);
    expect(STORES_SECTION_IDS).toEqual(["stores", "logs", "settings"]);
  });

  it("repairs a settings document field by field", () => {
    expect(sanitizeStoresSettings(null)).toEqual(DEFAULT_STORES_SETTINGS);
    expect(sanitizeStoresSettings({ version: 2, defaultSection: "logs" })).toEqual(DEFAULT_STORES_SETTINGS);
    // A garbage section must not cost the other preference.
    expect(sanitizeStoresSettings({ version: 1, defaultSection: "gone", hideQuietDomains: true })).toEqual({
      version: 1,
      defaultSection: "stores",
      hideQuietDomains: true,
    });
  });
});

// ---------------------------------------------------------------------------
// Adding a store -- the credential rule
// ---------------------------------------------------------------------------

describe("the add-a-store form", () => {
  it("asks for the NAME of a secret, and offers nothing that takes a token", async () => {
    mount(fakeConnection());
    await click(await screen.findByRole("button", { name: "Add a store" }));
    await screen.findByRole("heading", { name: "Add a store" });

    // The three credential fields are labelled as REFERENCES, on the visible
    // label AND on the accessible name -- a form that asked for "Admin token"
    // would put a merchant's Admin token in this browser's memory and in the
    // rendered MemQL call, which is the exact thing the reference
    // indirection exists to prevent.
    for (const label of [
      "Name of the secret holding the Admin API token. Not the token.",
      "Name of the secret holding the Headless channel's Storefront token. Not the token.",
      "Name of the secret holding the webhook signing secret. Not the secret.",
    ]) {
      expect(screen.getByLabelText(label)).toBeTruthy();
    }
    expect(screen.getByText("Admin token secret name")).toBeTruthy();
    expect(screen.getByText("Storefront token secret name")).toBeTruthy();
    expect(screen.getByText("Webhook secret name")).toBeTruthy();

    // NO FIELD ASKS FOR A TOKEN VALUE. Every control's accessible name is
    // checked rather than only the three above: this is the assertion that
    // survives somebody adding a fourth field.
    const named = screen
      .getAllByRole("textbox")
      .map((el) => el.getAttribute("aria-label") ?? labelTextFor(el));
    for (const name of named) {
      expect(name).toBeTruthy();
      if (/token|secret/i.test(name!)) {
        expect(name).toMatch(/Name of the secret/);
      }
    }
    // ...and nothing on the form is a password box, which is the other way a
    // surface says "paste the value here".
    expect(document.querySelectorAll("input[type=password]").length).toBe(0);

    // The help text says what a reference is and where the secret is made.
    expect(screen.getByText(/take the NAME of a secret, never a token/)).toBeTruthy();
  });

  it("renders the secret NAME the operator typed, and never a token-shaped value", async () => {
    const connection = fakeConnection();
    mount(connection);
    await click(await screen.findByRole("button", { name: "Add a store" }));

    await type(screen.getByLabelText("Store id, the URL segment its webhooks arrive on"), "acme-widgets");
    await type(screen.getByLabelText("The myshopify.com domain"), "acme-widgets.myshopify.com");
    await type(
      screen.getByLabelText("Name of the secret holding the Admin API token. Not the token."),
      "SHOPIFY_ACME_ADMIN_TOKEN",
    );

    await click(screen.getByRole("button", { name: "Add the store" }));

    const [call] = connection.callsNamed("createStore");
    expect(call).toBeTruthy();
    // The wire carries the REFERENCE. This is the assertion that would fail
    // if the field were ever rewired to a token.
    expect(call).toContain('adminTokenRef: "SHOPIFY_ACME_ADMIN_TOKEN"');
    expect(call).not.toMatch(/shpat_/);
    // Blank optional fields are omitted rather than sent as "": an enum
    // field would fail its check on "" rather than being left unset.
    expect(call).not.toContain("protectedDataLevel");
  });

  it("does not offer Add the store until the two required fields are given", async () => {
    mount(fakeConnection());
    await click(await screen.findByRole("button", { name: "Add a store" }));

    // ABSENT, NOT DISABLED (DESIGN.md rule 12).
    expect(screen.queryByRole("button", { name: "Add the store" })).toBeNull();
    expect(screen.getByText(/A store id and a domain are what the engine requires/)).toBeTruthy();

    await type(screen.getByLabelText("Store id, the URL segment its webhooks arrive on"), "acme");
    expect(screen.queryByRole("button", { name: "Add the store" })).toBeNull();
    expect(screen.getByText("A domain is still needed.")).toBeTruthy();

    await type(screen.getByLabelText("The myshopify.com domain"), "acme.myshopify.com");
    expect(screen.getByRole("button", { name: "Add the store" })).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// Absent is not zero
// ---------------------------------------------------------------------------

describe("a store nothing has reported on", () => {
  it("renders an em dash for drift on the list, never a zero", async () => {
    mount(fakeConnection({ stores: [NEVER_RUN] }));

    const line = await screen.findByRole("button", { name: /fresh-shop.myshopify.com/ });
    // The dash is there, carrying its reason...
    const dash = within(line).getByTitle("Nothing has reported this yet.");
    expect(dash.textContent).toBe("—");
    // ...and the line never says the last reconcile found nothing, because
    // no reconcile has run. A store that has never synced has not synced
    // cleanly, and `?? 0` is how those two become one sentence.
    expect(line.textContent).not.toMatch(/0 rows of drift/);
  });

  it("renders an em dash for a cost bucket nobody has observed, never 0 of 0 points", async () => {
    mount(fakeConnection({ stores: [NEVER_RUN] }));
    await openStore("fresh-shop.myshopify.com");

    expect(screen.getByTitle("No Admin API call has reported a cost bucket yet.").textContent).toBe("—");
    expect(screen.getByText("not observed yet")).toBeTruthy();
    // The reading a `?? 0` would produce is the most alarming one this page
    // can show -- a store at its rate limit -- for a store that is idle.
    expect(document.body.textContent).not.toContain("0 of 0 points");
  });

  it("says a subscription reconcile has never run rather than reporting none registered", async () => {
    mount(fakeConnection({ stores: [NEVER_RUN] }));
    await openStore("fresh-shop.myshopify.com");
    expect(screen.getByText(/No subscription reconcile has been recorded/)).toBeTruthy();
    expect(document.body.textContent).not.toContain("0 registered of 0");
  });

  it("measures what the report did measure", async () => {
    mount(fakeConnection({ stores: [LIVE] }));
    const line = await screen.findByRole("button", { name: /acme-widgets.myshopify.com/ });
    // A measured figure is the number, not a dash -- the distinction runs
    // both ways or it is not a distinction.
    expect(line.textContent).toContain("2 mirrored domains");
    expect(line.textContent).toContain("12 rows of drift on the last reconcile");
    expect(within(line).queryByTitle("Nothing has reported this yet.")).toBeNull();

    await openStore("acme-widgets.myshopify.com");
    expect(screen.getByText(/registered of/).textContent).toContain("38");
    // 1834.5 points is rounded for reading, never floored to a dash.
    expect(screen.getByText(/of.*points, restoring/).textContent).toContain("1835");
  });
});

// ---------------------------------------------------------------------------
// The action bar (DESIGN.md rule 12)
// ---------------------------------------------------------------------------

describe("the store's action bar", () => {
  it("offers Pause on a live store, and only Pause", async () => {
    mount(fakeConnection({ stores: [LIVE] }));
    const bar = await openStore("acme-widgets.myshopify.com");

    const pause = within(bar).getByRole("button", { name: "Pause ingestion for acme-widgets.myshopify.com" });
    expect((pause as HTMLButtonElement).disabled).toBe(false);
    expect(within(bar).queryByRole("button", { name: /Resume ingestion/ })).toBeNull();
    expect(within(bar).getByText("Live")).toBeTruthy();
  });

  it("offers Resume on a paused store, and only Resume", async () => {
    mount(fakeConnection({ stores: [PAUSED] }));
    const bar = await openStore("quiet-shop.myshopify.com");

    const resume = within(bar).getByRole("button", { name: "Resume ingestion for quiet-shop.myshopify.com" });
    expect((resume as HTMLButtonElement).disabled).toBe(false);
    expect(within(bar).queryByRole("button", { name: /Pause ingestion/ })).toBeNull();
    // A pause still STAGES deliveries, and the bar is where that is said:
    // left unsaid, a pause reads as a risk and does not get used.
    expect(within(bar).getByText(/loses telemetry rather than events/)).toBeTruthy();
  });

  it("never renders a disabled lifecycle act, whatever the status", () => {
    // The decision is a PURE function, so the rule can be asserted over
    // every status this shell knows without mounting five pages.
    for (const status of ["live", "backfilling", "configured", "paused", "error"]) {
      const reading = readingFor({ ...toHealth(LIVE), status });
      expect(reading.acts).toHaveLength(1);
      expect(reading.acts.map((a) => a.name)).toEqual([
        status === "paused" ? "Resume ingestion" : "Pause ingestion",
      ]);
    }
    // A status this shell has no copy for offers NEITHER: an act is a claim
    // about the state it acts from, and "pause it" asserts it is not paused.
    const unknown = readingFor({ ...toHealth(LIVE), status: "some_future_status" });
    expect(unknown.acts).toEqual([]);
    expect(unknown.detail).toContain("some_future_status");
  });

  it("writes the status the act names, on the store the page is about", async () => {
    const connection = fakeConnection({ stores: [LIVE] });
    mount(connection);
    const bar = await openStore("acme-widgets.myshopify.com");
    await click(within(bar).getByRole("button", { name: /Pause ingestion/ }));

    expect(connection.callsNamed("setStoreStatus")).toEqual([
      'mutation setStoreStatus(storeId: "acme-widgets", status: "paused")',
    ]);
  });
});

// ---------------------------------------------------------------------------
// The API version mismatch
// ---------------------------------------------------------------------------

describe("a store pinned to a version the mirror was not generated from", () => {
  const STALE = storeHealth({ storeId: "old-pin", apiVersion: "2026-04", mirrorApiVersion: "2026-07" });

  it("warns on the list line", async () => {
    mount(fakeConnection({ stores: [STALE] }));
    const line = await screen.findByRole("button", { name: /old-pin.myshopify.com/ });
    expect(within(line).getByText("pinned 2026-04, mirror 2026-07")).toBeTruthy();
  });

  it("says on the page that a call is refused rather than attempted", async () => {
    mount(fakeConnection({ stores: [STALE] }));
    await openStore("old-pin.myshopify.com");
    const notice = screen.getByText(/pinned to 2026-04/);
    expect(notice.textContent).toContain("REFUSED rather than attempted");
    expect(notice.textContent).toContain("2026-07");
  });

  it("is not a mismatch when the store pins nothing", () => {
    // A blank pin means the store runs at the mirror's own version, which is
    // the AGREEING case -- reading it as a mismatch would warn on every
    // store that never pinned one.
    expect(apiVersionMismatch({ ...toHealth(LIVE), apiVersion: "", mirrorApiVersion: "2026-07" })).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// The boundary with Data origins
// ---------------------------------------------------------------------------

describe("what this surface deliberately does not carry", () => {
  it("offers no per-domain act, and names where they live", async () => {
    mount(fakeConnection({ stores: [LIVE] }));
    await openStore("acme-widgets.myshopify.com");

    for (const forbidden of [/backfill/i, /retry/i, /discard/i, /dead letter/i]) {
      expect(screen.queryByRole("button", { name: forbidden })).toBeNull();
    }
    expect(screen.getByText(/live in Cluster . Data origins/)).toBeTruthy();
  });

  it("says that reconciling subscriptions is wider than this store", async () => {
    const connection = fakeConnection({ stores: [LIVE] });
    mount(connection);
    await openStore("acme-widgets.myshopify.com");

    // The builtin takes NO store argument -- it walks every ingesting store
    // -- so a button reading "reconcile this store" would be a claim it
    // cannot keep.
    expect(screen.getByText(/Reconciles every ingesting store, not only this one/)).toBeTruthy();
    await click(screen.getByRole("button", { name: /Reconcile now/ }));
    expect(connection.callsNamed("shopifyEnsureSubscriptions")).toEqual([
      "builtin shopifyEnsureSubscriptions()",
    ]);
  });
});

// ---------------------------------------------------------------------------
// The read itself
// ---------------------------------------------------------------------------

describe("the health read", () => {
  it("reads the builtin's id-keyed node map, not a row set", () => {
    // The shape the ENGINE sends. A reader that only understood flat rows
    // would be green here against a shape the wire never carries.
    const reply = builtinReply("shopifyStoreHealth", [
      { status: "ok", stores: [storeHealth({ storeId: "acme-widgets" })] } as unknown as Row,
    ]);
    const stores = readStoreHealth(reply.rows());
    expect(stores.map((s) => s.storeId)).toEqual(["acme-widgets"]);
  });

  it("says when it looked, and looks again on demand", async () => {
    const connection = fakeConnection({ stores: [LIVE] });
    mount(connection);
    await screen.findByRole("button", { name: /acme-widgets.myshopify.com/ });
    expect(screen.getByText(/this is not a live feed/)).toBeTruthy();
    expect(connection.callsNamed("shopifyStoreHealth")).toHaveLength(1);

    await click(screen.getByRole("button", { name: "Re-read every store's health" }));
    await waitFor(() => expect(connection.callsNamed("shopifyStoreHealth")).toHaveLength(2));
  });

  it("reports a refusal in the server's own words rather than as an empty list", async () => {
    mount(fakeConnection({ refuse: { shopifyStoreHealth: "not authorized: cluster owner required" } }));
    expect(await screen.findByText("This cluster did not report its stores.")).toBeTruthy();
    expect(screen.getByText("not authorized: cluster owner required")).toBeTruthy();
    // An empty list would be this window inventing the fact that a cluster
    // mirrors no store.
    expect(screen.queryByText(/No store is configured/)).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// The sync table, and the report keys that do not name what they carry
// ---------------------------------------------------------------------------

describe("the mirror sync table", () => {
  const DRIFTING = storeHealth({
    storeId: "busy-shop",
    domains: [
      // The WIRE keys, verbatim. They NAME what they carry now: three of
      // them did not (`staleWrites` carried lagSeconds, `tombstoned` carried
      // outboxDepth, `driftTotal` duplicated driftLast), and epic memql#5009
      // repaired that in the Go handler rather than renaming at the render.
      domainState({ concept: "v1:shopify:order", driftLast: 40, lagSeconds: 45, outboxDepth: 3 }),
      domainState({ concept: "v1:shopify:product", driftLast: 2 }),
      domainState({ concept: "v1:shopify:customer", driftLast: 9 }),
    ],
    driftLast: 51,
  } as Partial<Row> & { storeId: string });

  it("names the two columns for what they carry, not for the key they arrive under", async () => {
    mount(fakeConnection({ stores: [DRIFTING] }));
    await openStore("busy-shop.myshopify.com");

    // A latency drawn as "Stale" and a queue depth drawn as "Tombstoned" are
    // numbers under names that mean something else -- the same class of error
    // as a zero standing in for an absent measurement, one level up. The
    // repair belongs in the Go handler; until it lands, the surface tells the
    // truth about what it is showing.
    expect(screen.getByRole("columnheader", { name: "Lag" })).toBeTruthy();
    expect(screen.getByRole("columnheader", { name: "Outbox" })).toBeTruthy();
    expect(screen.queryByRole("columnheader", { name: "Stale" })).toBeNull();
    expect(screen.queryByRole("columnheader", { name: "Tombstoned" })).toBeNull();

    const first = screen.getAllByRole("row")[1]!;
    expect(first.textContent).toContain("45s");
  });

  it("sorts by drift descending, so the first row is the worst one", async () => {
    mount(fakeConnection({ stores: [DRIFTING] }));
    await openStore("busy-shop.myshopify.com");
    const concepts = screen
      .getAllByRole("row")
      .slice(1)
      .map((r) => r.querySelector("td")?.textContent);
    expect(concepts).toEqual(["v1:shopify:order", "v1:shopify:customer", "v1:shopify:product"]);
  });

  it("says the table is the connector's rows rather than this store's", async () => {
    mount(fakeConnection({ stores: [DRIFTING] }));
    await openStore("busy-shop.myshopify.com");
    // `v1:platform:syncState` is keyed by (concept, connector) with no store
    // in the key, and the handler gives every store in the report the same
    // slice -- so captioning this "this store's domains" would put a number
    // under the wrong subject.
    expect(screen.getByText(/keyed by concept and connector rather than by store/)).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// The shape of the surface (DESIGN.md rules 11 and 12)
// ---------------------------------------------------------------------------

describe("the section's shape", () => {
  it("shows one Head at a time, and the detail REPLACES the list", async () => {
    mount(fakeConnection({ stores: [LIVE] }));
    await screen.findByRole("button", { name: /acme-widgets.myshopify.com/ });
    // TWO HEADS IN ONE SCROLLER IS THE TELL that a page was appended beneath
    // the list it was selected from, which is what rule 11 exists to stop.
    expect(document.querySelectorAll(".os-head")).toHaveLength(1);

    await openStore("acme-widgets.myshopify.com");
    expect(document.querySelectorAll(".os-head")).toHaveLength(1);
    // The list is gone, not scrolled past.
    expect(screen.queryByRole("heading", { name: "Stores" })).toBeNull();
    expect(screen.getByRole("button", { name: /Stores/ })).toBeTruthy();
  });

  it("carries one action bar, on the surfaces that have a lifecycle", async () => {
    mount(fakeConnection({ stores: [LIVE] }));
    await screen.findByRole("button", { name: /acme-widgets.myshopify.com/ });
    // The list has no lifecycle, so it has no bar.
    expect(document.querySelectorAll(".os-actbar")).toHaveLength(0);

    await openStore("acme-widgets.myshopify.com");
    expect(document.querySelectorAll(".os-actbar")).toHaveLength(1);
    // ...and nothing that changes this store's STATUS lives anywhere else.
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    const lifecycle = screen
      .getAllByRole("button")
      .filter((b) => /(Pause|Resume) ingestion/.test(b.getAttribute("aria-label") ?? b.textContent ?? ""));
    expect(lifecycle).toHaveLength(1);
    expect(bar.contains(lifecycle[0]!)).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// The app shell
// ---------------------------------------------------------------------------

describe("the Stores app shell", () => {
  it("routes each section to its own surface", async () => {
    const store = memStoresStore();
    const first = mount(fakeConnection(), { sectionId: "stores", store });
    expect(await screen.findByRole("heading", { name: "Stores" })).toBeTruthy();
    first.view.unmount();

    mount(fakeConnection(), { sectionId: "settings", store });
    expect(await screen.findByRole("heading", { name: "Stores settings" })).toBeTruthy();
  });

  it("navigates to the stored default section once, on open", async () => {
    const store = memStoresStore({ defaultSection: "settings" });
    const { navigate } = mount(fakeConnection(), { sectionId: "stores", store });
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("settings"));
    expect(navigate).toHaveBeenCalledTimes(1);
  });

  it("keeps quiet-domain hiding in Settings rather than over the table", async () => {
    const store = memStoresStore();
    const view = mount(fakeConnection(), { sectionId: "settings", store });
    await click(await screen.findByLabelText("Hide domains with nothing to say"));
    expect(store.saved.at(-1)?.hideQuietDomains).toBe(true);
    view.view.unmount();

    // The same store, a fresh window: the preference is what the table reads,
    // and the empty state points back at it.
    mount(fakeConnection({ stores: [storeHealth({ storeId: "acme-widgets", domains: [domainState({ concept: "v1:shopify:product" })] } as Partial<Row> & { storeId: string })] }), {
      sectionId: "stores",
      store,
    });
    await openStore("acme-widgets.myshopify.com");
    expect(screen.queryByText("v1:shopify:product")).toBeNull();
    expect(screen.getByRole("button", { name: "a setting in this app" })).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------

/** A section's floor, or "" when it declares none. `RoleRequirement` is a
 *  union -- `{ min }` is the ladder floor, `{ any }` the non-monotonic set --
 *  and every floor this app declares is the first form. */
function floorOf(roles: OsAppSection["roles"]): string {
  return roles !== undefined && "min" in roles ? roles.min : "";
}

/** The label text of a control whose name comes from a `<label for=...>`. */
function labelTextFor(el: Element): string {
  const id = el.getAttribute("id") ?? "";
  return document.querySelector(`label[for="${id}"]`)?.textContent ?? "";
}

async function type(el: Element, value: string) {
  const input = el as HTMLInputElement;
  const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, "value")?.set;
  await act(async () => {
    setter?.call(input, value);
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

/** A wire fixture read back through the app's own reader, so the pure-function
 *  assertions run against exactly what the surface renders from. */
function toHealth(row: Row) {
  const [store] = readStoreHealth(
    builtinReply("shopifyStoreHealth", [{ status: "ok", stores: [row] } as unknown as Row]).rows(),
  );
  return store!;
}
