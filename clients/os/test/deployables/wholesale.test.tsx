import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { readTheBar, useFakeDate } from "../readTheBar";
import { seededAccessFor } from "../seededAccess";
import type { EffectiveCapability } from "../../src/system/roles";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection hook is replaced, as in reviews.test.tsx; the fake beneath
// answers at `executeNamed`, so the call STRING is what is asserted.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { click, fakeConnection, siteRow, withSession, type FakeConnection, type FakeSeed } from "./harness";

// A STORE'S WHOLESALE APPLICATIONS, AS A SECTION OF ITS DEPLOYABLE
// (apps/deployables/wholesale).
//
// What this surface can get wrong, in the order it would hurt: a decision the
// engine would refuse, or one made by somebody who decides none (so the acts
// are the engine's, and an operator is offered no decision); a decision that
// names who decided (so the decider is never sent); a buyer whose access did
// not land and nobody can tell (so every error code has its sentence); and a
// setting changed by the wrong person (so the store is set up by an operator
// alone, and changed only where the engine says it may be).

const NOW = Date.parse("2026-11-11T12:00:00.000Z");
const HOST = "shop.memql.example.com";
const ACCOUNT = "acct-acme";
const SHOP = siteRow({ id: "site-shop", hostname: HOST, kind: "shopify_storefront", binding: { storeId: "store-live" }, packageId: "pkg-shop", packageDeployableName: "storefront", accountId: ACCOUNT });

const NORTH_ID = "v1:wholesale:application:app-north";
const BLUE_ID = "v1:wholesale:application:app-blue";

const NORTH = {
  applicationId: NORTH_ID, companyName: "Northwind Dental Supply", applicantName: "Avery Stone", applicantEmail: "avery@northwind.example.com",
  stage: "readyToReview", primary: "Dental practice", secondary: "12 locations", createdAt: "2026-11-01T12:00:00Z",
};
const BLUE = {
  applicationId: BLUE_ID, companyName: "Bluebird Clinic", applicantName: "Jordan Reyes", applicantEmail: "jordan@bluebird.example.com",
  stage: "readyToReview", primary: "", secondary: "", createdAt: "2026-11-08T12:00:00Z",
};

/** wholesaleGet's answer for one application, ready to review unless told otherwise. */
function detail(row: typeof NORTH, over: Record<string, unknown> = {}): Row {
  return {
    application: { id: row.applicationId, companyName: row.companyName, applicantName: row.applicantName, applicantEmail: row.applicantEmail, source: "storefront", createdAt: row.createdAt },
    state: "submitted",
    stage: "readyToReview",
    details: { state: "shown", fields: [{ name: "practiceType", label: "Practice type", value: "Dental practice" }] },
    decisions: [],
    entitlement: null,
    operator: false,
    acts: ["reject", "approve"],
    ...over,
  } as unknown as Row;
}

const SET_UP = { exists: true, applicationsOpen: true, entitlementAdapter: "", introText: "", staffEmail: "", adapters: ["customerTag", "shopifyB2B"], may: { setUp: false, update: true } };

function wholesaleSeed(over: Partial<NonNullable<FakeSeed["wholesale"]>> = {}): FakeSeed {
  return {
    sites: [SHOP],
    wholesale: {
      list: { readyToReview: { rows: [NORTH, BLUE], counts: { waitingForTerms: 0, readyToReview: 2, approved: 3, rejected: 1, revoked: 0 }, next: "", truncated: false } as unknown as Row },
      get: { [NORTH_ID]: detail(NORTH), [BLUE_ID]: detail(BLUE) },
      settings: SET_UP as unknown as Row,
      ...over,
    },
  };
}

/** One of the store's own people: a member of its organization holding the wholesale part. */
const STAFF_PART: EffectiveCapability = { verb: "execute", resource: "app:deployables/wholesale", effect: "allow", source: "group" };
const STAFF = { role: "writer", everyAccount: false, accountIds: [ACCOUNT], capabilities: [...seededAccessFor("writer"), STAFF_PART] };
/** The same person without the part. */
const STAFF_WITHOUT = { ...STAFF, capabilities: seededAccessFor("writer") };
/** A cluster operator: the seeded owner, who reads every store's applications. */
const OPERATOR = { role: "owner" };

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({ getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) });
}

async function openDeployable(seed: FakeSeed, session: Parameters<typeof withSession>[1] = STAFF): Promise<{ connection: FakeConnection; page: HTMLElement }> {
  const connection = fakeConnection(seed);
  h.connection = connection;
  render(withSession(<DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} />, { userId: "u-me", ...session }));
  await waitFor(() => expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"));
  await click((await screen.findByText(HOST)).closest("button"));
  return { connection, page: await screen.findByRole("region", { name: `Deployable ${HOST}` }) };
}

async function openWholesale(seed: FakeSeed = wholesaleSeed(), session: Parameters<typeof withSession>[1] = STAFF): Promise<FakeConnection> {
  const { connection, page } = await openDeployable(seed, session);
  await click(within(page).getByRole("button", { name: "Wholesale" }));
  await screen.findByRole("heading", { name: "Wholesale" });
  return connection;
}

const bar = () => document.querySelector(".os-actbar") as HTMLElement;
const barButtons = () => within(bar()).queryAllByRole("button").map((b) => (b.textContent ?? "").trim()).filter((t) => t !== "");
const tab = (name: string) => within(screen.getByRole("navigation", { name: "Wholesale views" })).getByRole("button", { name });
const applications = () => screen.findByRole("list", { name: "Applications" });

/** Asks the list for one stage behind Refine. */
async function refineTo(stage: string) {
  await click(screen.getByRole("button", { name: "Refine applications" }));
  await click(within(screen.getByRole("radiogroup", { name: "Stage" })).getByRole("radio", { name: stage }));
}

async function openApplication(company: string) {
  await click(within(await applications()).getByRole("button", { name: `Open ${company}` }));
  await screen.findByRole("heading", { name: company });
}

/** One of the store's settings, by the name beside it. */
function settingRow(label: string): HTMLElement {
  const row = [...document.querySelectorAll(".os-setting-row")].find((r) => r.querySelector(".os-setting-name")?.textContent === label);
  if (row === undefined) throw new Error(`no setting row named ${label}`);
  return row as HTMLElement;
}

useFakeDate();

beforeEach(() => {
  vi.setSystemTime(NOW);
  localStorage.clear();
  sessionStorage.clear();
  h.connection = null;
});

describe("the Wholesale button", () => {
  it("draws no Wholesale button for somebody without the wholesale part", async () => {
    const { page } = await openDeployable(wholesaleSeed(), STAFF_WITHOUT);
    expect(within(page).getByRole("button", { name: "Statistics" })).toBeTruthy();
    expect(within(page).queryByRole("button", { name: "Wholesale" })).toBeNull();
  });

  it("draws no Wholesale button for a deployable that fronts no store", async () => {
    const plain = siteRow({ id: "site-plain", hostname: HOST, kind: "spa", accountId: ACCOUNT });
    const { page } = await openDeployable({ ...wholesaleSeed(), sites: [plain] });
    expect(within(page).getByRole("button", { name: "Statistics" })).toBeTruthy();
    expect(within(page).queryByRole("button", { name: "Wholesale" })).toBeNull();
  });
});

describe("the applications", () => {
  it("lists Ready to review by default, oldest first, and asks the stage behind Refine", async () => {
    const connection = await openWholesale();
    expect(tab("Applications").getAttribute("aria-current")).toBe("page");
    const first = connection.callsNamed("wholesaleList")[0];
    expect(first).toContain('siteId: "site-shop"');
    expect(first).toContain('storeId: "store-live"');
    expect(first).toContain('stage: "readyToReview"');
    const list = await applications();
    // The engine's order, oldest first: how long each has waited, in days.
    expect(within(list).getAllByRole("button").map((r) => r.getAttribute("aria-label"))).toEqual(["Open Northwind Dental Supply", "Open Bluebird Clinic"]);
    expect(within(list).getByText("waited 10 days")).toBeTruthy();
    expect(within(list).getByText("waited 3 days")).toBeTruthy();
    expect(within(list).getByText("Avery Stone · avery@northwind.example.com")).toBeTruthy();
    // The view names its stage once; no row says it again.
    expect(within(list).queryByText("Ready to review")).toBeNull();
    expect(bar().textContent).toContain("2 ready to review");

    // The stage is a question behind Refine, never a tab or a standing control.
    expect(screen.queryByRole("radiogroup", { name: "Stage" })).toBeNull();
    await refineTo("Approved");
    await waitFor(() => expect(connection.callsNamed("wholesaleList").some((c) => c.includes('stage: "approved"'))).toBe(true));
    expect(await screen.findByRole("region", { name: "Nothing approved" })).toBeTruthy();
  });

  it("offers no Waiting for terms under Refine", async () => {
    await openWholesale();
    await click(screen.getByRole("button", { name: "Refine applications" }));
    const stages = within(screen.getByRole("radiogroup", { name: "Stage" })).getAllByRole("radio").map((r) => r.textContent);
    expect(stages).toEqual(["Ready to review", "Approved", "Rejected", "Removed", "All"]);
    expect(screen.queryByText("Waiting for terms")).toBeNull();
  });

  it("draws the extension's two display facts as plain text, with no badge", async () => {
    await openWholesale();
    const list = await applications();
    const north = within(list).getByRole("button", { name: "Open Northwind Dental Supply" });
    expect(within(north).getByText("Dental practice").tagName).toBe("SPAN");
    expect(within(north).getByText("12 locations").tagName).toBe("SPAN");
    expect(list.querySelector(".os-chip, .os-badge, .os-record-status")).toBeNull();
  });

  it("says Closed to new applications in the Head when the store is closed", async () => {
    await openWholesale(wholesaleSeed({ settings: { ...SET_UP, applicationsOpen: false } as unknown as Row }));
    expect(await screen.findByText("Closed to new applications")).toBeTruthy();
  });

  it("tells an operator once that the store's people decide, and draws no decide act", async () => {
    const seed = wholesaleSeed({ get: { [NORTH_ID]: detail(NORTH, { operator: true, acts: [] }), [BLUE_ID]: detail(BLUE, { operator: true, acts: [] }) } });
    await openWholesale(seed, OPERATOR);
    await applications();
    expect(screen.getAllByText("You can read this store's applications. Its own people decide them.")).toHaveLength(1);
    await openApplication("Northwind Dental Supply");
    expect(within(bar()).queryByRole("button", { name: "Reject" })).toBeNull();
    expect(within(bar()).queryByRole("button", { name: "Approve" })).toBeNull();
    expect(within(bar()).queryByRole("button", { name: "Remove" })).toBeNull();
  });

  it("says when the store has more applications than one read covers", async () => {
    const seed = wholesaleSeed();
    (seed.wholesale!.list!.readyToReview as Record<string, unknown>).truncated = true;
    await openWholesale(seed);
    await applications();
    expect(screen.getByText("This store has more applications than one read covers, so the oldest are not listed.")).toBeTruthy();
  });

  it("shows a failed read as a Notice with Try again", async () => {
    const seed = wholesaleSeed({ listError: "wholesale: site \"site-shop\" does not serve store \"store-live\"" });
    const connection = await openWholesale(seed);
    expect(await screen.findByText(/does not serve store/)).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Nothing ready to review" })).toBeNull();
    const reads = connection.callsNamed("wholesaleList").length;
    delete seed.wholesale!.listError;
    await click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(connection.callsNamed("wholesaleList").length).toBeGreaterThan(reads));
    expect(await applications()).toBeTruthy();
  });
});

describe("one application", () => {
  async function open(seed: FakeSeed = wholesaleSeed(), company = "Northwind Dental Supply", session: Parameters<typeof withSession>[1] = STAFF): Promise<FakeConnection> {
    const connection = await openWholesale(seed, session);
    await openApplication(company);
    return connection;
  }

  it("draws Reject and Approve, Approve last, for an application ready to review", async () => {
    await open();
    expect(barButtons()).toEqual(["Reject", "Approve"]);
    expect(bar().textContent).toContain("Ready to review");
    expect(screen.getByText("Practice type")).toBeTruthy();
  });

  it("draws only Reject for an application waiting for terms", async () => {
    await open(wholesaleSeed({ get: { [NORTH_ID]: detail(NORTH, { stage: "waitingForTerms", acts: ["reject"] }) } }));
    expect(barButtons()).toEqual(["Reject"]);
    expect(bar().textContent).toContain("Waiting for terms");
  });

  it("draws no button for an act it does not know", async () => {
    await open(wholesaleSeed({ get: { [NORTH_ID]: detail(NORTH, { acts: ["reject", "resendTerms", "approve", "resendApproved"] }) } }));
    expect(barButtons()).toEqual(["Reject", "Approve"]);
  });

  it("sends neither principalKind nor decidedBy with a decision", async () => {
    const connection = await open();
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(connection.callsNamed("wholesaleRecordDecision")).toHaveLength(1));
    const call = connection.callsNamed("wholesaleRecordDecision")[0];
    expect(call).toContain(`applicationId: "${NORTH_ID}"`);
    expect(call).toContain('transition: "approve"');
    expect(call).not.toContain("principalKind");
    expect(call).not.toContain("decidedBy");
    // What the engine holds is read again.
    await waitFor(() => expect(connection.callsNamed("wholesaleGet").length).toBeGreaterThan(1));
  });

  it("asks in place before rejecting and sends the note", async () => {
    const connection = await open();
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Reject" }));
    expect(barButtons()).toEqual(["Keep it", "Reject it"]);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(connection.callsNamed("wholesaleRecordDecision")).toHaveLength(0);
    await act(async () => {
      fireEvent.change(screen.getByLabelText("Note"), { target: { value: "Not a dental business" } });
    });
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Reject it" }));
    await waitFor(() => expect(connection.callsNamed("wholesaleRecordDecision")).toHaveLength(1));
    const call = connection.callsNamed("wholesaleRecordDecision")[0];
    expect(call).toContain('transition: "reject"');
    expect(call).toContain('note: "Not a dental business"');
  });

  it("shows the engine's refusal as a Notice above the bar when somebody decided first", async () => {
    const refusal = "wholesale: an application that is approved is final; \"reject\" is refused.";
    const connection = await open(wholesaleSeed({ decideError: refusal }));
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Approve" }));
    const sentence = await screen.findByText(/an application that is approved is final/);
    const notice = sentence.closest(".os-notice") as HTMLElement;
    expect(notice).toBeTruthy();
    // The last thing in the page, standing on the bar.
    const body = document.querySelector(".os-action-body") as HTMLElement;
    expect(body.lastElementChild).toBe(notice);
    await waitFor(() => expect(connection.callsNamed("wholesaleGet").length).toBeGreaterThan(1));
  });

  it("offers Retry access on an approved application with no access recorded", async () => {
    const approved = detail(NORTH, { state: "approved", stage: "approved", entitlement: null, acts: ["remove", "retryAccess"] });
    const connection = await open(wholesaleSeed({ get: { [NORTH_ID]: approved } }));
    expect(barButtons()).toEqual(["Remove", "Retry access"]);
    expect(screen.getByText("Not granted yet")).toBeTruthy();
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Retry access" }));
    await waitFor(() => expect(connection.callsNamed("wholesaleRetryAccess")).toHaveLength(1));
    expect(connection.callsNamed("wholesaleRetryAccess")[0]).toBe(`builtin wholesaleRetryAccess(applicationId: "${NORTH_ID}")`);
  });

  it("asks in place before removing a buyer, and removes them as a revoke", async () => {
    const approved = detail(NORTH, { state: "approved", stage: "approved", entitlement: { state: "granted", errorCode: "", error: "" }, acts: ["remove"] });
    const connection = await open(wholesaleSeed({ get: { [NORTH_ID]: approved } }));
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Remove" }));
    expect(barButtons()).toEqual(["Keep them", "Remove them"]);
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Remove them" }));
    await waitFor(() => expect(connection.callsNamed("wholesaleRecordDecision")).toHaveLength(1));
    expect(connection.callsNamed("wholesaleRecordDecision")[0]).toContain('transition: "revoke"');
  });

  it("draws Retry access on a removed application only while the tag is still on", async () => {
    const stillOn = { ...NORTH, stage: "revoked" };
    const off = { ...BLUE, stage: "revoked" };
    const seed = wholesaleSeed({
      list: { revoked: { rows: [stillOn, off], counts: { revoked: 2 }, next: "", truncated: false } as unknown as Row },
      get: {
        [NORTH_ID]: detail(NORTH, { state: "revoked", stage: "revoked", entitlement: { state: "granted", errorCode: "", error: "" }, acts: ["retryAccess"] }),
        [BLUE_ID]: detail(BLUE, { state: "revoked", stage: "revoked", entitlement: { state: "revoked", errorCode: "", error: "" }, acts: [] }),
      },
    });
    await openWholesale(seed);
    await refineTo("Removed");
    await openApplication("Northwind Dental Supply");
    expect(bar().textContent).toContain("Removed");
    expect(barButtons()).toEqual(["Retry access"]);
    await click(screen.getByRole("button", { name: "Back to Applications" }));
    await openApplication("Bluebird Clinic");
    expect(barButtons()).toEqual([]);
  });

  it("says why access failed for each error code, and Trade prices on when granted", async () => {
    const cases: [Record<string, unknown> | null, string][] = [
      [{ state: "failed", errorCode: "no_route", error: "" }, "No access route is chosen in Settings."],
      [{ state: "failed", errorCode: "no_settings", error: "" }, "This store has no wholesale settings."],
      [{ state: "pending", errorCode: "scope_missing", error: "" }, "The store has not allowed MemQL to change customers. Its owner approves the new permissions from the store's admin."],
      [{ state: "failed", errorCode: "plan_ceiling", error: "" }, "The store's plan allows no more catalogs."],
      [{ state: "failed", errorCode: "customer_refused", error: "Email has already been taken" }, "Shopify refused: Email has already been taken"],
      [{ state: "pending", errorCode: "shopify_error", error: "Throttled" }, "Shopify refused: Throttled"],
      [{ state: "granted", errorCode: "", error: "" }, "Trade prices on"],
    ];
    for (const [entitlement, sentence] of cases) {
      await open(wholesaleSeed({ get: { [NORTH_ID]: detail(NORTH, { state: "approved", stage: "approved", entitlement, acts: [] }) } }));
      const access = screen.getByRole("region", { name: "Access" });
      expect(within(access).getByText(sentence)).toBeTruthy();
      cleanup();
    }
  });

  it("draws No further details when the client's fields hold nothing, and says when they cannot be read", async () => {
    await open(wholesaleSeed({ get: { [NORTH_ID]: detail(NORTH, { details: { state: "shown", fields: [] } }), [BLUE_ID]: detail(BLUE, { details: { state: "unreadable", fields: [] } }) } }));
    expect(within(screen.getByRole("region", { name: "Their details" })).getByText("No further details")).toBeTruthy();
    await click(screen.getByRole("button", { name: "Back to Applications" }));
    await openApplication("Bluebird Clinic");
    expect(within(screen.getByRole("region", { name: "Their details" })).getByText("These details are not readable to you")).toBeTruthy();
  });
});

describe("the settings", () => {
  async function openSettings(seed: FakeSeed = wholesaleSeed(), session: Parameters<typeof withSession>[1] = STAFF): Promise<FakeConnection> {
    const connection = await openWholesale(seed, session);
    await click(tab("Settings"));
    await waitFor(() => expect(tab("Settings").getAttribute("aria-current")).toBe("page"));
    return connection;
  }

  it("saves the route and draws no staff address", async () => {
    const connection = await openSettings();
    const route = within(await waitFor(() => settingRow("Route"))).getByRole("radiogroup", { name: "Route" });
    expect(within(route).getAllByRole("radio").map((r) => r.textContent)).toEqual(["Customer tag", "Shopify B2B", "None"]);
    expect(within(route).getByRole("radio", { name: "None" }).getAttribute("aria-checked")).toBe("true");
    await click(within(route).getByRole("radio", { name: "Customer tag" }));
    await readTheBar();
    expect(barButtons()).toEqual(["Discard", "Save"]);
    await click(within(bar()).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(connection.callsNamed("wholesaleUpdateSettings")).toHaveLength(1));
    expect(connection.callsNamed("wholesaleUpdateSettings")[0]).toBe('builtin wholesaleUpdateSettings(storeId: "store-live", siteId: "site-shop", entitlementAdapter: "customerTag")');
    // Nobody is told anything in this release, so nothing asks where.
    expect(screen.queryByText(/staff/i)).toBeNull();
    expect(document.querySelector(".os-review-settings input, .os-wholesale-settings input")).toBeNull();
    await waitFor(() => expect(bar().textContent).toContain("Saved"));
  });

  it("reads the settings again when asked, after they changed while somebody was editing", async () => {
    const seed = wholesaleSeed({ settingsError: "These settings changed while you were editing. Reload and try again." });
    const connection = await openSettings(seed);
    const route = within(await waitFor(() => settingRow("Route"))).getByRole("radiogroup", { name: "Route" });
    await click(within(route).getByRole("radio", { name: "Shopify B2B" }));
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Save" }));
    expect(await screen.findByText("These settings changed while you were editing. Reload and try again.")).toBeTruthy();
    const reads = connection.callsNamed("wholesaleStoreSettings").length;
    // Nothing is read behind the person's back: the draft stands until they ask.
    expect(within(route).getByRole("radio", { name: "Shopify B2B" }).getAttribute("aria-checked")).toBe("true");
    seed.wholesale!.settings = { ...SET_UP, entitlementAdapter: "customerTag" } as unknown as Row;
    delete seed.wholesale!.settingsError;
    await click(screen.getByRole("button", { name: "Reload" }));
    await waitFor(() => expect(connection.callsNamed("wholesaleStoreSettings").length).toBeGreaterThan(reads));
    await waitFor(() => expect(within(settingRow("Route")).getByRole("radio", { name: "Customer tag" }).getAttribute("aria-checked")).toBe("true"));
    expect(screen.queryByText("These settings changed while you were editing. Reload and try again.")).toBeNull();
  });

  it("offers an operator Set up wholesale on a store with no settings, and creates it closed with no route", async () => {
    const seed = wholesaleSeed({ settings: { exists: false, applicationsOpen: false, entitlementAdapter: "", adapters: ["customerTag", "shopifyB2B"], may: { setUp: true, update: false } } as unknown as Row });
    const connection = await openSettings(seed, OPERATOR);
    expect(await screen.findByText("This store's wholesale program is not set up yet. The cluster's operators set it up.")).toBeTruthy();
    expect(barButtons()).toEqual(["Set up wholesale for this store"]);
    const reads = connection.callsNamed("wholesaleStoreSettings").length;
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Set up wholesale for this store" }));
    await waitFor(() => expect(connection.callsNamed("wholesaleSetSettings")).toHaveLength(1));
    expect(connection.callsNamed("wholesaleSetSettings")[0]).toBe('builtin wholesaleSetSettings(storeId: "store-live", siteId: "site-shop", applicationsOpen: false, entitlementAdapter: "")');
    await waitFor(() => expect(connection.callsNamed("wholesaleStoreSettings").length).toBeGreaterThan(reads));
    expect(await waitFor(() => settingRow("Route"))).toBeTruthy();
    expect(screen.queryByText("This store's wholesale program is not set up yet. The cluster's operators set it up.")).toBeNull();
  });

  it("shows the client's staff only the not-set-up sentence on a store with no settings", async () => {
    const seed = wholesaleSeed({ settings: { exists: false, applicationsOpen: false, entitlementAdapter: "", adapters: ["customerTag", "shopifyB2B"], may: { setUp: false, update: false } } as unknown as Row });
    await openSettings(seed);
    expect(await screen.findByText("This store's wholesale program is not set up yet. The cluster's operators set it up.")).toBeTruthy();
    expect(barButtons()).toEqual([]);
    expect(screen.queryAllByRole("radio")).toHaveLength(0);
    expect(document.querySelector(".os-setting-row")).toBeNull();
  });
});
