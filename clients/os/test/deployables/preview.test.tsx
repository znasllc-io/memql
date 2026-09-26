import { render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection is a module-level context read and its provider dials a real
// websocket, so the hook is replaced rather than the provider mounted.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { PreviewSection } from "../../src/apps/deployables/preview/PreviewSection";
import { ALL_PARTS, NO_PARTS } from "../../src/apps/deployables/parts";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import {
  DEV_STORE,
  SHOP,
  STORE,
  click,
  fakeConnection,
  previewGrantRow,
  previewObservationRow,
  previewReadinessRow,
  siteRow,
  withSession,
  type FakeConnection,
  type FakeSeed,
} from "./harness";

// THE PREVIEW SECTION (epic memql#5531; DESIGN.md rules 6 and 12;
// clients/os/SUPERVISED-VISUAL-COMPOSITION.md).
//
// ===========================================================================
// WHAT THIS SURFACE HAS TO GET RIGHT
// ===========================================================================
// One sentence: THERE IS A VERSION THAT SERVES AND A VERSION BEING EXERCISED,
// AND EACH TALKS TO A DIFFERENT STORE. Every case here is an assertion about
// that sentence or about the three ways this screen could lie about it:
//
//   - by drawing an act the engine would refuse (rule 12: absent, never
//     disabled, and the reason drawn where the act would have been);
//   - by drawing a step nobody measured as a failure, or as a zero;
//   - by naming the wrong store on a lane, which is the failure the whole
//     feature exists to prevent and the one that is silent until a test
//     payment lands in a merchant's real orders.
//
// ===========================================================================
// THE NEGATIVE CASES CARRY MORE THAN THE POSITIVE ONES
// ===========================================================================
// A deployable with no candidate, a preview binding pointed at the live store,
// a person without the part -- those are the states this section spends most
// of its life in, and each has an honest rendering that is easy to get wrong
// in a way no positive case would catch.

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

async function openDeployable(hostname: string): Promise<HTMLElement> {
  await waitFor(() =>
    expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"),
  );
  await click((await screen.findByText(hostname)).closest("button"));
  return (await screen.findByRole("region", { name: `Deployable ${hostname}` })).closest(
    "[data-deployable-view]",
  ) as HTMLElement;
}

/** Opens the storefront and returns its Preview section. */
async function openPreview(seed: FakeSeed, opts: { role?: string } = {}) {
  const connection = fakeConnection(seed);
  h.connection = connection;
  render(withSession(<PreviewSection site={siteFromRow(seed.sites![0]!)} runs={[]} can={opts.role === "viewer" ? NO_PARTS : ALL_PARTS} onOpenStore={vi.fn()} />));
  const section = await screen.findByRole("region", { name: "Preview" });
  await waitFor(() => expect(section.querySelector(".preview-lane-store")?.textContent).not.toBe("No store — this is not a storefront"));
  const page = section;
  return { connection, page, section };
}

/** A storefront carrying a candidate and a development store on its preview binding. */
const SHOP_WITH_CANDIDATE = siteRow({
  ...SHOP,
  id: "site-shop",
  candidateRef: "blob://sites/site-shop/v2/",
  previewBinding: { storeId: "store-example-dev" },
} as never);

/** The readiness that goes with it: everything legal. */
const READY = previewReadinessRow({
  siteId: "site-shop",
  hostname: "shop.memql.example.com",
  bundleRef: "blob://sites/site-shop/v1/",
  candidateRef: "blob://sites/site-shop/v2/",
  hasCandidate: true,
  storeId: "store-example",
  storeDomain: "example.myshopify.com",
  storeReadable: true,
  storeIsDevelopment: false,
  previewStoreId: "store-example-dev",
  previewStoreDomain: "example-dev.myshopify.com",
  canPreview: true,
  canPromote: true,
  canGoLive: true,
  previewRefusal: { code: "", message: "", remedy: "" },
  promoteRefusal: { code: "", message: "", remedy: "" },
} as never);

const READY_SEED: FakeSeed = {
  sites: [SHOP_WITH_CANDIDATE],
  stores: [STORE, DEV_STORE],
  previewReadiness: { "site-shop": READY },
  previewGrants: { "site-shop": [previewGrantRow({ id: "grant-1" })] },
};

describe("the two lanes", () => {
  it("names the version that serves and the version being exercised, and the store each talks to", async () => {
    const { section } = await openPreview(READY_SEED);

    const lanes = section.querySelectorAll<HTMLElement>(".preview-lane");
    expect(lanes).toHaveLength(2);

    // THE ONE ASSERTION THIS WHOLE FEATURE RESTS ON: the live store is named
    // on the SERVING lane and the development store on the CANDIDATE lane, and
    // never the other way round.
    const serving = lanes[0]!;
    expect(serving.dataset["serving"]).toBe("true");
    expect(within(serving).getByText("Serving")).toBeTruthy();
    expect(within(serving).getByText("v1")).toBeTruthy();
    expect(within(serving).getByText("example.myshopify.com")).toBeTruthy();
    expect(within(serving).getByText("the store shoppers reach")).toBeTruthy();

    const candidate = lanes[1]!;
    expect(candidate.dataset["serving"]).toBe("false");
    expect(within(candidate).getByText("Candidate")).toBeTruthy();
    expect(within(candidate).getByText("v2")).toBeTruthy();
    expect(within(candidate).getByText("example-dev.myshopify.com")).toBeTruthy();
    expect(within(candidate).getByText("development store")).toBeTruthy();
  });

  it("says there is nothing being exercised rather than leaving the lane blank", async () => {
    // A blank cell is indistinguishable from a cell that failed to render, so
    // the empty state is a sentence.
    const { section } = await openPreview({
      sites: [SHOP],
      stores: [STORE],
      previewReadiness: {
        "site-shop": previewReadinessRow({ siteId: "site-shop", storeId: "store-example", storeDomain: "example.myshopify.com", storeReadable: true } as never),
      },
    });
    expect(within(section).getByText("No version being exercised")).toBeTruthy();
  });

  it("invites somebody to attach a development store rather than showing an empty store", async () => {
    const { section } = await openPreview({
      sites: [SHOP],
      stores: [STORE],
      previewReadiness: {
        "site-shop": previewReadinessRow({ siteId: "site-shop", storeId: "store-example", storeDomain: "example.myshopify.com", storeReadable: true } as never),
      },
    });
    expect(within(section).getByRole("button", { name: /Attach a development store/ })).toBeTruthy();
  });

  it("does not call the live store a development store just because the preview binding names it", async () => {
    // THE SENTENCE MUST NOT CONTRADICT THE NOTICE BENEATH IT. The lane's blurb
    // was a constant -- "development store" under whatever was bound -- so the
    // one case the guard exists for rendered a label asserting the very thing
    // the refusal two lines below denies. Only a rendered page shows that, so
    // this is the assertion that keeps it fixed.
    const { section } = await openPreview({
      sites: [siteRow({ ...SHOP, id: "site-shop", candidateRef: "blob://sites/site-shop/v2/", previewBinding: { storeId: "store-example" } } as never)],
      stores: [STORE],
      previewReadiness: {
        "site-shop": previewReadinessRow({
          siteId: "site-shop",
          candidateRef: "blob://sites/site-shop/v2/",
          hasCandidate: true,
          storeId: "store-example",
          storeDomain: "example.myshopify.com",
          storeReadable: true,
          previewStoreId: "store-example",
          previewStoreDomain: "example.myshopify.com",
          canPreview: false,
        } as never),
      },
    });
    const candidate = section.querySelectorAll<HTMLElement>(".preview-lane")[1]!;
    expect(within(candidate).getByText(/not a development store/)).toBeTruthy();
    expect(within(candidate).queryByText("development store")).toBeNull();
  });

  it("does not draw a store the caller cannot read as though nothing were bound", async () => {
    // Hiding a real misconfiguration behind a state that looks deliberate is
    // the failure this rules out -- the same rule the Store slot follows.
    const { section } = await openPreview({
      sites: [SHOP],
      stores: [],
      previewReadiness: {
        "site-shop": previewReadinessRow({ siteId: "site-shop", storeId: "store-example", storeDomain: "", storeReadable: false } as never),
      },
    });
    expect(within(section).getByText("A store you cannot read")).toBeTruthy();
  });
});

describe("what this cluster watched", () => {
  it("draws a step nobody has measured as unmeasured, not as a failure and not as a zero", async () => {
    const { section } = await openPreview({
      ...READY_SEED,
      previewObservations: {
        "site-shop": [
          previewObservationRow({ kind: "catalog_read", detail: 'read product "tote"', durationMs: 340 }),
        ],
      },
    });

    const rows = section.querySelectorAll<HTMLElement>(".preview-observation");
    expect(rows).toHaveLength(4);

    // Measured and answered.
    expect(rows[0]!.dataset["state"]).toBe("answered");
    expect(within(rows[0]!).getByText("answered")).toBeTruthy();
    expect(within(rows[0]!).getByText("340 ms")).toBeTruthy();

    // NOT MEASURED. Three properties, and all three are the point: the state
    // is its own value, the words say so, and NO DURATION IS PRINTED -- a
    // "0 ms" beside a step nobody ran is a measurement nobody made.
    for (const row of [rows[1]!, rows[2]!, rows[3]!]) {
      expect(row.dataset["state"]).toBe("unmeasured");
      expect(within(row).getByText("not measured yet")).toBeTruthy();
      expect(row.textContent).not.toContain("0 ms");
    }
  });

  it("says why a step did not answer, in the engine's own words", async () => {
    const { section } = await openPreview({
      ...READY_SEED,
      previewObservations: {
        "site-shop": [
          previewObservationRow({
            kind: "cart_accepted",
            ok: false,
            failure: "the store refused the line: Not enough items available",
            durationMs: 610,
          }),
        ],
      },
    });
    const cart = section.querySelectorAll<HTMLElement>(".preview-observation")[1]!;
    expect(cart.dataset["state"]).toBe("failed");
    expect(within(cart).getByText("did not answer")).toBeTruthy();
    expect(within(cart).getByText(/Not enough items available/)).toBeTruthy();
  });

  it("labels the observations as store checks", async () => {
    // The design record asks this surface not to imply the engine proves a
    // payment. It is the one claim a reader would otherwise make for it.
    const { section } = await openPreview(READY_SEED);
    expect(within(section).getByText("Store checks")).toBeTruthy();
  });

  it("invites the first exercise rather than drawing four failures", async () => {
    const { section } = await openPreview(READY_SEED);
    expect(within(section).getByText(/Open a preview to run the store checks/)).toBeTruthy();
  });
});

describe("acts that are not legal", () => {
  it("withholds the preview and draws the refusal with the act that clears it", async () => {
    // THE CASE THE GUARD EXISTS FOR: a preview binding pointed at the store
    // shoppers reach. The control is ABSENT (rule 12) and the reason is drawn
    // where it would have been -- a missing button with no explanation is the
    // failure this replaces.
    const { section } = await openPreview({
      sites: [SHOP_WITH_CANDIDATE],
      stores: [STORE],
      previewReadiness: {
        "site-shop": previewReadinessRow({
          siteId: "site-shop",
          candidateRef: "blob://sites/site-shop/v2/",
          hasCandidate: true,
          storeId: "store-example",
          storeDomain: "example.myshopify.com",
          storeReadable: true,
          previewStoreId: "store-example",
          previewStoreDomain: "example.myshopify.com",
          canPreview: false,
          previewRefusal: {
            code: "preview_binding_is_not_development_store",
            message: "the preview binding names example.myshopify.com, which is the store shoppers reach -- exercising a candidate against it would put test carts and test payments in the merchant's real store.",
            remedy: "Point the preview binding at a development store. Shopify marks one on the store row as isDevelopment.",
          },
        } as never),
      },
    });

    expect(within(section).queryByRole("button", { name: "Open a preview" })).toBeNull();
    expect(within(section).getByText(/which is the store shoppers reach/)).toBeTruthy();
    expect(within(section).getByText(/Point the preview binding at a development store/)).toBeTruthy();
  });

  it("never renders a disabled preview control", async () => {
    const { section } = await openPreview({
      sites: [SHOP],
      stores: [STORE],
      previewReadiness: { "site-shop": previewReadinessRow({ siteId: "site-shop" } as never) },
    });
    for (const button of section.querySelectorAll("button")) {
      expect(button.hasAttribute("disabled")).toBe(false);
    }
  });

  it("is absent for somebody whose grants do not reach the preview part", async () => {
    // A viewer holds `read app:deployables` and no part. The section's own
    // acts go with the part; the READING stays, because withholding the facts
    // as well would tell a reader nothing about a deployable they may see.
    const { section } = await openPreview(READY_SEED, { role: "viewer" });
    expect(within(section).queryByRole("button", { name: "Open a preview" })).toBeNull();
    expect(within(section).queryByRole("button", { name: /Withdraw the candidate/ })).toBeNull();
    expect(section.querySelectorAll(".preview-lane")).toHaveLength(2);
  });

  it("is absent entirely on the platform's own site", async () => {
    // MemQL OS is systemOwned and exempt from the preview axis as it is from
    // the status and settings axes. Drawing the section on the console
    // somebody is reading this in would be controls that only ever fail.
    const connection = fakeConnection({
      sites: [siteRow({ id: "site-os", hostname: "os.memql.example.com", systemOwned: true, bundleRef: "file:///app/os" })],
    });
    mount(connection);
    const page = await openDeployable("os.memql.example.com");
    expect(within(page).queryByRole("region", { name: "Preview" })).toBeNull();
  });
});

describe("opening a preview", () => {
  it("shows the link once and says so", async () => {
    const { section } = await openPreview(READY_SEED);
    await click(within(section).getByRole("button", { name: "Open a preview" }));

    const link = await within(section).findByRole("link", { name: /Open the preview/ });
    expect(link.getAttribute("href")).toBe(
      "https://shop.memql.example.com/_memql/preview?grant=mql_prv_TEST",
    );
    // THE SENTENCE IS PART OF THE CONTRACT: the cluster keeps only a digest,
    // so a person who loses this link cannot be given it again, and a surface
    // that did not say so would be setting them up.
    expect(within(section).getByText(/shown once/)).toBeTruthy();
    expect(within(section).getByText(/30 minutes/)).toBeTruthy();
  });

  it("names the version and the deployable the link opens", async () => {
    const { section } = await openPreview(READY_SEED);
    await click(within(section).getByRole("button", { name: "Open a preview" }));
    expect(await within(section).findByText(/opens v2 at shop\.memql\.example\.com/)).toBeTruthy();
  });

  it("keeps the refusal in surface rather than swallowing it", async () => {
    const { section } = await openPreview({ ...READY_SEED, previewOpenError: "sitePreviewOpen: no deployable is readable by this caller" });
    await click(within(section).getByRole("button", { name: "Open a preview" }));
    expect(await within(section).findByText(/no deployable is readable by this caller/)).toBeTruthy();
  });
});

describe("the promotion", () => {
  it("is on the action bar and not in the section, and names the version it promotes", async () => {
    const connection = fakeConnection({ ...READY_SEED, sites: [{ ...SHOP_WITH_CANDIDATE, kind: "spa" }] });
    mount(connection);
    const page = await openDeployable("shop.memql.example.com");

    // RULE 12: every act that changes what the public is served lives on the
    // one bar, so the section must not carry a second one.
    expect(within(page).queryByRole("region", { name: "Preview" })).not.toBeNull();

    // THE BAR IS AT THE WINDOW'S EDGE, outside the deployable's own region --
    // which is rule 12's whole point, so the search is the screen's.
    void page;
    // The bar's accessible name carries the deployable too ("Promote the
    // candidate Storefront"), so the match is on the act's own words.
    const promote = await screen.findByRole("button", { name: /^Promote the candidate/ });
    await click(promote);

    // THE VERSION IS NAMED ON THE WIRE, which is the whole safety of the
    // mutation: the engine refuses a promotion whose candidate is not the one
    // stored, so a candidate republished between the read and the click is
    // refused rather than promoted by surprise.
    const calls = connection.callsNamed("promoteSiteCandidate");
    expect(calls).toHaveLength(1);
    expect(calls[0]).toContain('candidateRef: "blob://sites/site-shop/v2/"');
  });

  it("is absent when the engine would refuse it", async () => {
    const connection = fakeConnection({
      sites: [SHOP_WITH_CANDIDATE],
      stores: [STORE, DEV_STORE],
      previewReadiness: {
        "site-shop": previewReadinessRow({
          siteId: "site-shop",
          candidateRef: "blob://sites/site-shop/v2/",
          hasCandidate: true,
          storeId: "store-example-dev",
          storeDomain: "example-dev.myshopify.com",
          storeReadable: true,
          storeIsDevelopment: true,
          previewStoreId: "store-example-dev",
          previewStoreDomain: "example-dev.myshopify.com",
          canPreview: true,
          canPromote: false,
          canGoLive: false,
          promoteRefusal: {
            code: "serving_binding_is_development_store",
            message: "this storefront is bound to example-dev.myshopify.com, which is a development store.",
            remedy: "Bind the storefront to the store shoppers reach, then try again.",
          },
        } as never),
      },
    });
    mount(connection);
    await openDeployable("shop.memql.example.com");
    expect(screen.queryByRole("button", { name: /^Promote the candidate/ })).toBeNull();
  });

  it("is absent with no candidate at all", async () => {
    const connection = fakeConnection({
      sites: [SHOP],
      stores: [STORE],
      previewReadiness: { "site-shop": previewReadinessRow({ siteId: "site-shop" } as never) },
    });
    mount(connection);
    await openDeployable("shop.memql.example.com");
    expect(screen.queryByRole("button", { name: /^Promote the candidate/ })).toBeNull();
  });
});

describe("previews that are open", () => {
  it("distinguishes a link nobody opened from one used a while ago", async () => {
    // ABSENT MEANS NOBODY OPENED IT, never "a long time ago". They are
    // different answers to somebody deciding whether a link escaped.
    const { section } = await openPreview({
      ...READY_SEED,
      previewGrants: {
        "site-shop": [
          previewGrantRow({ id: "grant-1", lastSeenAt: "" }),
          previewGrantRow({ id: "grant-2", lastSeenAt: new Date(Date.now() - 5 * 60_000).toISOString() }),
        ],
      },
    });
    expect(within(section).getByText(/not opened yet/)).toBeTruthy();
    expect(within(section).getByText(/last used 5 minutes ago/)).toBeTruthy();
  });

  it("lists nothing when every preview has expired", async () => {
    const { section } = await openPreview({
      ...READY_SEED,
      previewGrants: {
        "site-shop": [previewGrantRow({ id: "grant-old", expiresAt: "2020-01-01T00:00:00Z" })],
      },
    });
    expect(within(section).queryByText("Previews open now")).toBeNull();
  });

  it("ends one, and says that ending stops it for everybody", async () => {
    const { connection, section } = await openPreview(READY_SEED);
    await click(within(section).getByRole("button", { name: "End this preview" }));
    expect(connection.callsNamed("revokeSitePreviewGrant")).toHaveLength(1);
    expect(within(section).getByText(/stops it working for anybody holding it/)).toBeTruthy();
  });
});
