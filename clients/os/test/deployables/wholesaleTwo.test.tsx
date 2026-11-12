import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { readTheBar, useFakeDate } from "../readTheBar";
import { seededAccessFor } from "../seededAccess";
import type { EffectiveCapability } from "../../src/system/roles";

const h = vi.hoisted(() => ({ connection: null as unknown }));

// The connection hook is replaced, as in wholesale.test.tsx; the fake beneath
// answers at `executeNamed`, so the call STRING is what is asserted.
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { dayWords } from "../../src/apps/deployables/wholesale/model";
import { click, fakeConnection, siteRow, withSession, type FakeConnection, type FakeSeed } from "./harness";

// THE WHOLESALE SECTION, COMPLETE: the buyers, the documents and their
// versions, the whole of the settings, and on one application its terms and
// its emails (apps/deployables/wholesale).
//
// What this half can get wrong, in the order it would hurt: a document
// published that nobody chose to publish (so nothing leaves the browser until
// Publish); an act drawn that the engine does not offer (so the acts are the
// engine's, and a state that refuses one draws none); a reason the screen made
// up for something that did not happen (so every reason is the engine's
// sentence); and a buyer who cannot tell why their trade prices are not on.

const NOW = Date.parse("2026-11-11T12:00:00.000Z");
const HOST = "shop.memql.example.com";
const ACCOUNT = "acct-acme";
const SHOP = siteRow({ id: "site-shop", hostname: HOST, kind: "shopify_storefront", binding: { storeId: "store-live" }, packageId: "pkg-shop", packageDeployableName: "storefront", accountId: ACCOUNT });

const KESTREL_ID = "v1:wholesale:application:app-kestrel";
const CEDAR_ID = "v1:wholesale:application:app-cedar";
const MAPLE_ID = "v1:wholesale:application:app-maple";
const LARK_ID = "wholesale-staff-lark";

const KESTREL = {
  applicationId: KESTREL_ID, companyName: "Kestrel Cycles", applicantName: "Avery Stone", applicantEmail: "avery@kestrel.example.com",
  stage: "readyToReview", primary: "Repair workshop", secondary: "12 locations", createdAt: "2026-11-01T12:00:00Z",
};

const TERMS_SHA = "3f2a9c1e" + "b".repeat(56);
const OLD_TERMS_SHA = "77aa01cd" + "c".repeat(56);
const MAP_SHA = "a1b2c3d4" + "d".repeat(56);

/** wholesaleGet's answer for one application, ready to review unless told otherwise. */
function detail(company: string, id: string, over: Record<string, unknown> = {}): Row {
  return {
    application: { id, companyName: company, applicantName: "Avery Stone", applicantEmail: "avery@kestrel.example.com", source: "storefront", createdAt: "2026-11-01T12:00:00Z" },
    state: "submitted",
    stage: "readyToReview",
    details: { state: "shown", fields: [{ name: "shopType", label: "Shop type", value: "Repair workshop" }] },
    decisions: [],
    entitlement: null,
    acceptance: null,
    termsLink: null,
    emails: [],
    operator: false,
    acts: ["reject", "approve"],
    ...over,
  } as unknown as Row;
}

const SETTINGS = {
  exists: true, applicationsOpen: true, entitlementAdapter: "customerTag", introText: "",
  staffEmail: "", staffEmailAdmitted: false, termsRequired: true, linkLifetimeDays: 30,
  acceptanceText: "I have read and accept the Wholesale Terms and Conditions and the MAP Policy shown above.",
  senderIdentityId: "", termsTemplateId: "", approvedTemplateId: "", rejectedTemplateId: "", revokedTemplateId: "",
  templates: [{ id: "tpl-invite", name: "Trade terms invitation" }, { id: "tpl-welcome", name: "Trade welcome" }],
  documents: { terms: { label: "Trade terms 2026", sha256: TERMS_SHA }, map: { label: "MAP policy 2026", sha256: MAP_SHA }, priceSheet: null },
  adapters: ["customerTag", "shopifyB2B"],
  may: { setUp: false, update: true, setSender: false },
};

const DOCUMENTS = {
  kinds: [
    {
      kind: "terms",
      current: { id: "doc-terms-2", label: "Trade terms 2026", publishedAt: "2026-11-02T09:00:00Z", publishedBy: "v1:identity:user:u-dana", publishedByName: "Dana Lee", sha256: TERMS_SHA, body: "# Trade terms\n\nNet 30." },
      history: [
        { id: "doc-terms-2", label: "Trade terms 2026", publishedAt: "2026-11-02T09:00:00Z", publishedBy: "v1:identity:user:u-dana", publishedByName: "Dana Lee", sha256: TERMS_SHA },
        { id: "doc-terms-1", label: "Trade terms 2025", publishedAt: "2025-10-01T09:00:00Z", publishedBy: "v1:identity:user:u-dana", publishedByName: "Dana Lee", sha256: OLD_TERMS_SHA },
      ],
    },
    {
      kind: "map",
      current: { id: "doc-map-1", label: "MAP policy 2026", publishedAt: "2026-11-02T09:05:00Z", publishedBy: "v1:identity:user:u-dana", publishedByName: "Dana Lee", sha256: MAP_SHA, body: "No advertised price below MAP." },
      history: [{ id: "doc-map-1", label: "MAP policy 2026", publishedAt: "2026-11-02T09:05:00Z", publishedBy: "v1:identity:user:u-dana", publishedByName: "Dana Lee", sha256: MAP_SHA }],
    },
    { kind: "priceSheet", current: null, history: [] },
  ],
};

function seedWith(over: Partial<NonNullable<FakeSeed["wholesale"]>> = {}): FakeSeed {
  return {
    sites: [SHOP],
    wholesale: {
      list: { readyToReview: { rows: [KESTREL], counts: { waitingForTerms: 1, readyToReview: 1, approved: 2, rejected: 0, revoked: 0 }, next: "", truncated: false } as unknown as Row },
      get: { [KESTREL_ID]: detail("Kestrel Cycles", KESTREL_ID) },
      settings: SETTINGS as unknown as Row,
      documents: DOCUMENTS as unknown as Row,
      ...over,
    },
  };
}

/** One of the store's own people: a member of its organization holding the wholesale part. */
const STAFF_PART: EffectiveCapability = { verb: "execute", resource: "app:deployables/wholesale", effect: "allow", source: "group" };
const STAFF = { role: "writer", everyAccount: false, accountIds: [ACCOUNT], capabilities: [...seededAccessFor("writer"), STAFF_PART] };

function memStore() {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({ getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) });
}

async function openWholesale(seed: FakeSeed = seedWith()): Promise<FakeConnection> {
  const connection = fakeConnection(seed);
  h.connection = connection;
  render(withSession(<DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} />, { userId: "u-me", ...STAFF }));
  await waitFor(() => expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"));
  await click((await screen.findByText(HOST)).closest("button"));
  const page = await screen.findByRole("region", { name: `Deployable ${HOST}` });
  await click(within(page).getByRole("button", { name: "Wholesale" }));
  await screen.findByRole("heading", { name: "Wholesale" });
  return connection;
}

const bar = () => document.querySelector(".os-actbar") as HTMLElement;
const barButtons = () => within(bar()).queryAllByRole("button").map((b) => (b.textContent ?? "").trim()).filter((t) => t !== "");
const tab = (name: string) => within(screen.getByRole("navigation", { name: "Wholesale views" })).getByRole("button", { name });
const region = (name: string) => screen.getByRole("region", { name });

async function openTab(name: string) {
  await click(tab(name));
  await waitFor(() => expect(tab(name).getAttribute("aria-current")).toBe("page"));
}

async function openApplication(company: string) {
  await click(within(await screen.findByRole("list", { name: "Applications" })).getByRole("button", { name: `Open ${company}` }));
  await screen.findByRole("heading", { name: company });
}

async function fill(label: string, value: string) {
  const field = screen.getByLabelText(label) as HTMLInputElement | HTMLTextAreaElement;
  await act(async () => {
    fireEvent.change(field, { target: { value } });
  });
}

/** One of the store's settings, by the name beside it. */
function settingRow(label: string): HTMLElement {
  const row = [...document.querySelectorAll(".os-setting-row")].find((r) => r.querySelector(".os-setting-name")?.textContent === label);
  if (row === undefined) throw new Error(`no setting row named ${label}`);
  return row as HTMLElement;
}

/** Hands the page's file input a file, as choosing one in the browser's picker does. */
async function chooseFile(file: File) {
  const input = document.querySelector('input[type="file"]') as HTMLInputElement;
  expect(input).toBeTruthy();
  await act(async () => {
    Object.defineProperty(input, "files", { configurable: true, value: [file] });
    fireEvent.change(input);
  });
}

useFakeDate();

beforeEach(() => {
  vi.setSystemTime(NOW);
  localStorage.clear();
  sessionStorage.clear();
  h.connection = null;
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("the buyers", () => {
  const BUYERS = {
    rows: [
      { applicationId: CEDAR_ID, companyName: "Cedar Spoke Studio", applicantEmail: "riley@cedarspoke.example.com", accessState: "granted", errorCode: "", grantedAt: "2026-11-03T10:00:00Z", shopifyCustomerId: "" },
      { applicationId: MAPLE_ID, companyName: "Maple Street Bicycles", applicantEmail: "casey@maplestreet.example.com", accessState: "failed", errorCode: "plan_ceiling", grantedAt: "", shopifyCustomerId: "" },
    ],
    next: "",
    truncated: false,
  };

  it("lists approved buyers with their access in words, and opens the application", async () => {
    const connection = await openWholesale(seedWith({ buyers: BUYERS as unknown as Row, get: { [CEDAR_ID]: detail("Cedar Spoke Studio", CEDAR_ID, { state: "approved", stage: "approved", acts: ["remove"] }) } }));
    await openTab("Buyers");
    const list = await screen.findByRole("list", { name: "Buyers" });
    const call = connection.callsNamed("wholesaleBuyers")[0];
    expect(call).toContain('siteId: "site-shop"');
    expect(call).toContain('storeId: "store-live"');
    const cedar = within(list).getByRole("button", { name: "Open Cedar Spoke Studio" });
    expect(within(cedar).getByText("riley@cedarspoke.example.com")).toBeTruthy();
    expect(within(cedar).getByText(`Trade prices on since ${dayWords("2026-11-03T10:00:00Z")}`)).toBeTruthy();
    const maple = within(list).getByRole("button", { name: "Open Maple Street Bicycles" });
    expect(within(maple).getByText("Not on yet: the store's plan allows no more catalogs.")).toBeTruthy();
    expect(list.querySelector(".os-chip, .os-badge, .os-record-status")).toBeNull();
    await click(cedar);
    await screen.findByRole("heading", { name: "Cedar Spoke Studio" });
    expect(connection.callsNamed("wholesaleGet").some((c) => c.includes(`applicationId: "${CEDAR_ID}"`))).toBe(true);
    // Back leads to the tab it was opened from.
    await click(screen.getByRole("button", { name: "Back to Buyers" }));
    expect(await screen.findByRole("list", { name: "Buyers" })).toBeTruthy();
  });

  it("adds a buyer and opens the new application, waiting for terms", async () => {
    const lark = detail("Lark Cycles", LARK_ID, { stage: "waitingForTerms", acts: ["reject", "resendTerms"], application: { id: LARK_ID, companyName: "Lark Cycles", applicantName: "Robin Vale", applicantEmail: "robin@lark.example.com", source: "staff", createdAt: "2026-11-11T11:00:00Z" } });
    const connection = await openWholesale(seedWith({ addedId: LARK_ID, get: { [LARK_ID]: lark } }));
    await openTab("Buyers");
    await screen.findByText("No buyers yet.");
    await click(screen.getByRole("button", { name: "Add buyer" }));
    await screen.findByRole("heading", { name: "Add buyer" });
    expect(screen.getByText("They will be emailed the terms. They can buy at trade prices once they accept and someone approves.")).toBeTruthy();
    await fill("Company", "Lark Cycles");
    await fill("Name", "Robin Vale");
    await fill("Email", "robin@lark.example.com");
    await readTheBar();
    expect(barButtons()).toEqual(["Cancel", "Add and send the terms"]);
    // Nothing is written before the press.
    expect(connection.callsNamed("wholesaleAddBuyer")).toHaveLength(0);
    await click(within(bar()).getByRole("button", { name: "Add and send the terms" }));
    await waitFor(() => expect(connection.callsNamed("wholesaleAddBuyer")).toHaveLength(1));
    expect(connection.callsNamed("wholesaleAddBuyer")[0]).toBe('builtin wholesaleAddBuyer(siteId: "site-shop", storeId: "store-live", companyName: "Lark Cycles", applicantName: "Robin Vale", applicantEmail: "robin@lark.example.com")');
    await screen.findByRole("heading", { name: "Lark Cycles" });
    expect(connection.callsNamed("wholesaleGet").some((c) => c.includes(`applicationId: "${LARK_ID}"`))).toBe(true);
    await waitFor(() => expect(bar().textContent).toContain("Waiting for terms"));
  });

  it("shows the engine's refusal for an address that already applied", async () => {
    await openWholesale(seedWith({ addError: "wholesale: This address already has an application on this store." }));
    await openTab("Buyers");
    await click(await screen.findByRole("button", { name: "Add buyer" }));
    await fill("Company", "Kestrel Cycles");
    await fill("Name", "Avery Stone");
    await fill("Email", "avery@kestrel.example.com");
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Add and send the terms" }));
    const sentence = await screen.findByText("This address already has an application on this store.");
    expect(sentence.closest(".os-notice")).toBeTruthy();
    // What was typed stays, on the same page.
    expect(screen.getByRole("heading", { name: "Add buyer" })).toBeTruthy();
    expect((screen.getByLabelText("Company") as HTMLInputElement).value).toBe("Kestrel Cycles");
    expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe("Avery Stone");
    expect((screen.getByLabelText("Email") as HTMLInputElement).value).toBe("avery@kestrel.example.com");
  });
});

describe("the documents", () => {
  it("draws each document kind with its fingerprint, and says when one is not published", async () => {
    const connection = await openWholesale();
    await openTab("Documents");
    const terms = await screen.findByRole("region", { name: "Wholesale terms" });
    expect(connection.callsNamed("wholesaleDocuments")[0]).toContain('storeId: "store-live"');
    expect(within(terms).getAllByText("Trade terms 2026").length).toBeGreaterThan(0);
    expect(within(terms).getByText("3f2a9c1e")).toBeTruthy();
    // Eight characters on screen; the whole of it is never a line of its own.
    expect(within(terms).queryByText(TERMS_SHA)).toBeNull();
    expect(within(terms).getAllByText("Dana Lee").length).toBeGreaterThan(0);
    expect(within(terms).getByRole("button", { name: "Publish a new version" })).toBeTruthy();
    const map = region("MAP policy");
    expect(within(map).getByText("a1b2c3d4")).toBeTruthy();
    const sheet = region("Price sheet");
    expect(within(sheet).getByText("Not published. Applications cannot open while terms are required.")).toBeTruthy();
    expect(within(sheet).getByRole("button", { name: "Publish a new version" })).toBeTruthy();
    expect(document.querySelector(".os-chip, .os-badge")).toBeNull();
  });

  it("loads a document from a file and publishes it", async () => {
    const connection = await openWholesale();
    await openTab("Documents");
    await click(within(await screen.findByRole("region", { name: "MAP policy" })).getByRole("button", { name: "Publish a new version" }));
    await screen.findByRole("heading", { name: "Publish a new version" });
    expect(screen.getByRole("button", { name: "Load from a file" })).toBeTruthy();

    // A file the engine would refuse is refused here, in the engine's words, and nothing is loaded.
    await chooseFile(new File(["x".repeat(204801)], "huge.md", { type: "text/markdown" }));
    expect(await screen.findByText("a document's body is 1 to 204800 bytes; this one is 204801")).toBeTruthy();
    expect((screen.getByLabelText("Text") as HTMLTextAreaElement).value).toBe("");

    const text = "# MAP policy\n\nNo advertised price below **MAP**.";
    await chooseFile(new File([text], "map-policy.md", { type: "text/markdown" }));
    await waitFor(() => expect((screen.getByLabelText("Text") as HTMLTextAreaElement).value).toBe(text));
    expect(screen.queryByText("a document's body is 1 to 204800 bytes; this one is 204801")).toBeNull();
    // The preview draws the text as elements, never as HTML set from it.
    const preview = region("Preview");
    expect(within(preview).getByRole("heading", { name: "MAP policy" })).toBeTruthy();
    expect(within(preview).getByText("MAP").tagName).toBe("STRONG");
    await fill("Label", "MAP policy 2027");
    // Nothing leaves the browser until Publish.
    expect(connection.callsNamed("wholesalePublishDocument")).toHaveLength(0);
    await readTheBar();
    expect(barButtons()).toEqual(["Cancel", "Publish"]);
    const reads = connection.callsNamed("wholesaleDocuments").length;
    await click(within(bar()).getByRole("button", { name: "Publish" }));
    await waitFor(() => expect(connection.callsNamed("wholesalePublishDocument")).toHaveLength(1));
    const call = connection.callsNamed("wholesalePublishDocument")[0];
    expect(call).toContain('siteId: "site-shop"');
    expect(call).toContain('storeId: "store-live"');
    expect(call).toContain('kind: "map"');
    expect(call).toContain('label: "MAP policy 2027"');
    expect(call).toContain("No advertised price below **MAP**.");
    // Back on the documents, read again.
    await waitFor(() => expect(tab("Documents").getAttribute("aria-current")).toBe("page"));
    await waitFor(() => expect(connection.callsNamed("wholesaleDocuments").length).toBeGreaterThan(reads));
  });

  it("opens an earlier version read-only", async () => {
    const version = { id: "doc-terms-1", kind: "terms", label: "Trade terms 2025", publishedAt: "2025-10-01T09:00:00Z", publishedBy: "v1:identity:user:u-dana", publishedByName: "Dana Lee", sha256: OLD_TERMS_SHA, body: "# Trade terms\n\nNet **45**." };
    const connection = await openWholesale(seedWith({ versions: { "doc-terms-1": version as unknown as Row } }));
    await openTab("Documents");
    const terms = await screen.findByRole("region", { name: "Wholesale terms" });
    const earlier = within(terms).getByRole("list", { name: "Earlier versions" });
    // The current version is not an earlier one.
    expect(within(earlier).getAllByRole("button").map((b) => b.getAttribute("aria-label"))).toEqual(["Open Trade terms 2025"]);
    await click(within(earlier).getByRole("button", { name: "Open Trade terms 2025" }));
    await screen.findByRole("heading", { name: "Trade terms 2025" });
    expect(connection.callsNamed("wholesaleDocuments").some((c) => c.includes('versionId: "doc-terms-1"'))).toBe(true);
    expect(await screen.findByText("45")).toBeTruthy();
    expect(screen.getByText("77aa01cd")).toBeTruthy();
    expect(screen.queryAllByRole("textbox")).toHaveLength(0);
    expect(barButtons()).toEqual([]);
  });
});

describe("the settings, complete", () => {
  async function openSettings(seed: FakeSeed = seedWith()): Promise<FakeConnection> {
    const connection = await openWholesale(seed);
    await openTab("Settings");
    await waitFor(() => settingRow("Terms required"));
    return connection;
  }

  it("refuses turning terms on before the documents exist, in the engine's words", async () => {
    const connection = await openSettings(seedWith({
      settings: { ...SETTINGS, termsRequired: false, documents: { terms: null, map: null, priceSheet: null } } as unknown as Row,
      settingsError: "wholesale: Publish the terms, the MAP policy and the price sheet first.",
    }));
    const box = within(settingRow("Terms required")).getByRole("checkbox") as HTMLInputElement;
    expect(box.checked).toBe(false);
    await click(box);
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(connection.callsNamed("wholesaleUpdateSettings")).toHaveLength(1));
    expect(connection.callsNamed("wholesaleUpdateSettings")[0]).toContain("termsRequired: true");
    const sentence = await screen.findByText("Publish the terms, the MAP policy and the price sheet first.");
    expect(sentence.closest(".os-notice")).toBeTruthy();
    // The draft stands.
    expect((within(settingRow("Terms required")).getByRole("checkbox") as HTMLInputElement).checked).toBe(true);
  });

  it("saves the staff address and says whether the outbox delivers to it", async () => {
    const connection = await openSettings();
    const field = within(settingRow("Staff address")).getByRole("textbox") as HTMLInputElement;
    await act(async () => {
      fireEvent.change(field, { target: { value: "orders@lark-cycles.example.com" } });
    });
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(connection.callsNamed("wholesaleUpdateSettings")).toHaveLength(1));
    expect(connection.callsNamed("wholesaleUpdateSettings")[0]).toBe('builtin wholesaleUpdateSettings(storeId: "store-live", siteId: "site-shop", staffEmail: "orders@lark-cycles.example.com")');
    // The engine's answer, read again: the allowlist does not admit it yet.
    expect(await screen.findByText("The outbox does not deliver to this address yet. The cluster's operators add it.")).toBeTruthy();
    cleanup();

    await openSettings(seedWith({ settings: { ...SETTINGS, staffEmail: "orders@lark-cycles.example.com", staffEmailAdmitted: true } as unknown as Row }));
    expect(within(settingRow("Staff address")).getByText("The outbox delivers to this address.")).toBeTruthy();
    expect(screen.queryByText("The outbox does not deliver to this address yet. The cluster's operators add it.")).toBeNull();
  });

  it("offers only published transactional templates, with Off first", async () => {
    const connection = await openSettings();
    for (const name of ["Terms email", "Approval email", "Rejection email", "Removal email"]) {
      expect(screen.getByRole("combobox", { name }).textContent).toBe("Off");
    }
    await click(screen.getByRole("combobox", { name: "Terms email" }));
    const listbox = document.body.querySelector('[role="listbox"]') as HTMLElement;
    expect(within(listbox).getAllByRole("option").map((o) => o.textContent)).toEqual(["Off", "Trade terms invitation", "Trade welcome"]);
    await click(within(listbox).getByRole("option", { name: "Trade terms invitation" }));
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(connection.callsNamed("wholesaleUpdateSettings")).toHaveLength(1));
    expect(connection.callsNamed("wholesaleUpdateSettings")[0]).toBe('builtin wholesaleUpdateSettings(storeId: "store-live", siteId: "site-shop", termsTemplateId: "tpl-invite")');
  });

  it("downloads every record as one file", async () => {
    const made: Blob[] = [];
    const saved: string[] = [];
    vi.spyOn(URL, "createObjectURL").mockImplementation((blob) => {
      made.push(blob as Blob);
      return "blob:wholesale-export";
    });
    vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      saved.push(this.download);
    });
    const connection = await openSettings(seedWith({ exportFile: { fileName: "wholesale-store-live-20261111.json", json: "{\"applications\":[{\"companyName\":\"Kestrel Cycles\"}]}" } as unknown as Row }));
    await click(screen.getByRole("button", { name: "Download every record" }));
    await waitFor(() => expect(saved).toEqual(["wholesale-store-live-20261111.json"]));
    expect(connection.callsNamed("wholesaleExportStore")).toHaveLength(1);
    expect(connection.callsNamed("wholesaleExportStore")[0]).toContain('storeId: "store-live"');
    expect(made).toHaveLength(1);
    expect(await made[0]!.text()).toBe("{\"applications\":[{\"companyName\":\"Kestrel Cycles\"}]}");
  });
});

describe("the applications, completed", () => {
  it("offers Waiting for terms under Refine", async () => {
    const connection = await openWholesale();
    await click(screen.getByRole("button", { name: "Refine applications" }));
    const stages = within(screen.getByRole("radiogroup", { name: "Stage" })).getAllByRole("radio");
    expect(stages.map((r) => r.textContent)).toEqual(["Ready to review", "Waiting for terms", "Approved", "Rejected", "Removed", "All"]);
    await click(within(screen.getByRole("radiogroup", { name: "Stage" })).getByRole("radio", { name: "Waiting for terms" }));
    await waitFor(() => expect(connection.callsNamed("wholesaleList").some((c) => c.includes('stage: "waitingForTerms"'))).toBe(true));
  });

  it("says under Ready to review that applications appear once the terms are accepted", async () => {
    const empty = { readyToReview: { rows: [], counts: { readyToReview: 0 }, next: "", truncated: false } as unknown as Row };
    await openWholesale(seedWith({ list: empty }));
    const state = await screen.findByRole("region", { name: "Nothing ready to review" });
    expect(within(state).getByText("Applications appear here once the applicant has accepted the terms.")).toBeTruthy();
    cleanup();
    // A store that requires no terms has nothing to wait for.
    await openWholesale(seedWith({ list: empty, settings: { ...SETTINGS, termsRequired: false } as unknown as Row }));
    await screen.findByRole("region", { name: "Nothing ready to review" });
    expect(screen.queryByText("Applications appear here once the applicant has accepted the terms.")).toBeNull();
  });
});

describe("one application, completed", () => {
  async function openKestrel(over: Record<string, unknown>, seedOver: Partial<NonNullable<FakeSeed["wholesale"]>> = {}): Promise<FakeConnection> {
    const connection = await openWholesale(seedWith({ get: { [KESTREL_ID]: detail("Kestrel Cycles", KESTREL_ID, over) }, ...seedOver }));
    await openApplication("Kestrel Cycles");
    return connection;
  }

  it("shows the acceptance with both fingerprints, or when the link expires", async () => {
    await openKestrel({
      acceptance: { signedName: "Avery Stone", acceptedAt: "2026-11-05T09:30:00Z", termsLabel: "Trade terms 2026", termsSha256: TERMS_SHA, mapLabel: "MAP policy 2026", mapSha256: MAP_SHA, clientAddress: "203.0.113.7", userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0)" },
      termsLink: { expiresAt: "2026-12-01T00:00:00Z", expired: false },
    });
    const terms = region("Terms");
    expect(within(terms).getByText(`Accepted by Avery Stone on ${dayWords("2026-11-05T09:30:00Z")}`)).toBeTruthy();
    expect(within(terms).getByText("Trade terms 2026")).toBeTruthy();
    expect(within(terms).getByText("3f2a9c1e")).toBeTruthy();
    expect(within(terms).getByText("MAP policy 2026")).toBeTruthy();
    expect(within(terms).getByText("a1b2c3d4")).toBeTruthy();
    expect(within(terms).getByText("203.0.113.7")).toBeTruthy();
    expect(within(terms).getByText("Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0)")).toBeTruthy();
    expect(within(terms).queryByText(/Not accepted yet/)).toBeNull();
    cleanup();

    await openKestrel({ stage: "waitingForTerms", acts: ["reject"], termsLink: { expiresAt: "2026-12-01T00:00:00Z", expired: false } });
    expect(within(region("Terms")).getByText(`Not accepted yet. The link expires on ${dayWords("2026-12-01T00:00:00Z")}.`)).toBeTruthy();
    cleanup();

    await openKestrel({ stage: "waitingForTerms", acts: ["reject"], termsLink: { expiresAt: "2026-11-01T00:00:00Z", expired: true } });
    expect(within(region("Terms")).getByText("The link has expired.")).toBeTruthy();
  });

  it("shows each email's delivery, and why one was not sent", async () => {
    await openKestrel({
      state: "approved", stage: "approved", acts: ["remove"],
      entitlement: { state: "granted", errorCode: "", error: "" },
      emails: [
        { kind: "approved", attempt: 1, state: "skipped", reason: "“Trade welcome” is not a published transactional template.", sentAt: "", requestedBy: "", delivery: null },
        { kind: "terms", attempt: 2, state: "sent", reason: "", sentAt: "2026-11-04T08:00:00Z", requestedBy: "v1:identity:user:u-dana", delivery: { status: "sent", skipReason: "", providerStatus: "delivered", sentAt: "2026-11-04T08:00:01Z" } },
        { kind: "terms", attempt: 1, state: "sent", reason: "", sentAt: "2026-11-02T08:00:00Z", requestedBy: "", delivery: { status: "sent", skipReason: "", providerStatus: "bounced", sentAt: "2026-11-02T08:00:01Z" } },
      ],
    });
    const emails = region("Emails");
    // One line per kind: the newest attempt.
    expect(within(emails).getByText("Terms email")).toBeTruthy();
    expect(within(emails).getByText("Delivered")).toBeTruthy();
    expect(within(emails).queryByText("Bounced: the address does not accept mail")).toBeNull();
    expect(within(emails).getByText("Approval email")).toBeTruthy();
    expect(within(emails).getByText("Not sent: “Trade welcome” is not a published transactional template.")).toBeTruthy();
    cleanup();

    await openKestrel({
      state: "rejected", stage: "rejected", acts: [],
      emails: [
        { kind: "rejected", attempt: 1, state: "sent", reason: "", sentAt: "2026-11-06T08:00:00Z", requestedBy: "", delivery: null },
        { kind: "terms", attempt: 1, state: "sent", reason: "", sentAt: "2026-11-02T08:00:00Z", requestedBy: "", delivery: { status: "sent", skipReason: "", providerStatus: "bounced", sentAt: "2026-11-02T08:00:01Z" } },
      ],
    });
    const rejected = region("Emails");
    expect(within(rejected).getByText(`Sent on ${dayWords("2026-11-06T08:00:00Z")}`)).toBeTruthy();
    expect(within(rejected).getByText("Bounced: the address does not accept mail")).toBeTruthy();
  });

  it("draws Resend terms email while an application waits for terms", async () => {
    const connection = await openKestrel({ stage: "waitingForTerms", acts: ["reject", "resendTerms"] });
    expect(barButtons()).toEqual(["Reject", "Resend terms email"]);
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Resend terms email" }));
    await waitFor(() => expect(connection.callsNamed("wholesaleResendEmail")).toHaveLength(1));
    expect(connection.callsNamed("wholesaleResendEmail")[0]).toBe(`builtin wholesaleResendEmail(applicationId: "${KESTREL_ID}", kind: "terms")`);
    await waitFor(() => expect(connection.callsNamed("wholesaleGet").length).toBeGreaterThan(1));
  });

  it("draws Resend approval email only once it was sent", async () => {
    const granted = { state: "approved", stage: "approved", entitlement: { state: "granted", errorCode: "", error: "" } };
    await openKestrel({ ...granted, acts: ["remove"] });
    expect(barButtons()).toEqual(["Remove"]);
    cleanup();

    const connection = await openKestrel({ ...granted, acts: ["remove", "resendApproved"] }, { resendError: "wholesale: no approved email template is chosen" });
    expect(barButtons()).toEqual(["Remove", "Resend approval email"]);
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Resend approval email" }));
    await waitFor(() => expect(connection.callsNamed("wholesaleResendEmail")).toHaveLength(1));
    expect(connection.callsNamed("wholesaleResendEmail")[0]).toContain('kind: "approved"');
    // The engine's refusal, in its own words.
    expect(await screen.findByText("no approved email template is chosen")).toBeTruthy();
  });

  it("renders the client's own fields labelled in plain words", async () => {
    await openKestrel({ details: { state: "shown", fields: [{ name: "shopType", label: "Shop type", value: "Repair workshop" }, { name: "locationCount", label: "Locations", value: "12" }] } });
    const theirs = region("Their details");
    expect(within(theirs).getByText("Shop type")).toBeTruthy();
    expect(within(theirs).getByText("Locations")).toBeTruthy();
    expect(within(theirs).getByText("Repair workshop")).toBeTruthy();
    expect(within(theirs).queryByText("shopType")).toBeNull();
    expect(within(theirs).queryByText("locationCount")).toBeNull();
  });

  it("removes a buyer through wholesaleRemoveBuyer", async () => {
    const connection = await openKestrel({ state: "approved", stage: "approved", entitlement: { state: "granted", errorCode: "", error: "" }, acts: ["remove"] });
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Remove" }));
    await act(async () => {
      fireEvent.change(screen.getByLabelText("Note"), { target: { value: "Closed their account" } });
    });
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Remove them" }));
    await waitFor(() => expect(connection.callsNamed("wholesaleRemoveBuyer")).toHaveLength(1));
    expect(connection.callsNamed("wholesaleRemoveBuyer")[0]).toBe(`builtin wholesaleRemoveBuyer(applicationId: "${KESTREL_ID}", note: "Closed their account")`);
    expect(connection.callsNamed("wholesaleRecordDecision")).toHaveLength(0);
  });

  it("draws no act in a state that refuses it", async () => {
    const cases: [Record<string, unknown>, string[]][] = [
      [{ state: "rejected", stage: "rejected", acts: [] }, []],
      [{ state: "approved", stage: "approved", entitlement: { state: "granted", errorCode: "", error: "" }, acts: ["remove"] }, ["Remove"]],
      [{ stage: "waitingForTerms", acts: ["reject"] }, ["Reject"]],
    ];
    for (const [over, drawn] of cases) {
      await openKestrel(over);
      expect(barButtons()).toEqual(drawn);
      expect(screen.queryByRole("button", { name: "Resend terms email" })).toBeNull();
      expect(screen.queryByRole("button", { name: "Resend approval email" })).toBeNull();
      cleanup();
    }
  });
});

describe("a store that is not set up", () => {
  it("draws the not-set-up state on every tab, with no Try again", async () => {
    const refusal = "wholesale: This store's wholesale program is not set up yet. The cluster's operators set it up.";
    const seed = seedWith({
      listError: refusal, buyersError: refusal, documentsError: refusal,
      settings: { ...SETTINGS, exists: false, may: { setUp: false, update: false, setSender: false } } as unknown as Row,
    });
    await openWholesale(seed);
    for (const name of ["Applications", "Buyers", "Documents", "Settings"]) {
      if (name !== "Applications") await openTab(name);
      expect(await screen.findByText("This store's wholesale program is not set up yet. The cluster's operators set it up.")).toBeTruthy();
      expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
      expect(screen.queryByRole("alert")).toBeNull();
    }
  });
});
