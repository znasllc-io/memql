import { render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

// THE SHELL IS REAL FOR THE WORKSPACE AND STUBBED FOR THE PAGE: the page is
// rendered alone, and what it asks of the shell is to open Settings, which
// the stub records; the workspace needs the whole shell to open a deployable.
const h = vi.hoisted(() => ({ connection: null as unknown, openApp: vi.fn(), shell: false }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
vi.mock("../../src/chrome/state", async (importOriginal) => {
  const real = await importOriginal<typeof import("../../src/chrome/state")>();
  return {
    ...real,
    useOs: () => (h.shell ? real.useOs() : { actions: { openApp: h.openApp } }),
    useOsIfPresent: () => (h.shell ? real.useOsIfPresent() : null),
  };
});

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { siteFromRow } from "../../src/apps/deployables/rows";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { ShopifyStorePanel } from "../../src/apps/deployables/store/ShopifyStorePanel";
import { readTheBar } from "../readTheBar";
import {
  click,
  fakeConnection,
  previewReadinessRow,
  rowsResult,
  SHOP,
  siteRow,
  storeVerdictRow,
  verdictReasonRow,
  withSession,
  type FakeConnection,
  type FakeSeed,
} from "./harness";

// THE STORE PAGE SAYS WHETHER A STORE IS BEING RECORDED (U7).
//
// The verdict is the engine's: its state, each reason's sentence, detail and
// act label, the refusals. The page draws them as they arrive and adds no word
// of its own to any of them -- so these cases assert the engine's words where
// they belong and the absence of everything else.

afterEach(() => {
  h.connection = null;
  h.shell = false;
  h.openApp.mockClear();
});

const OWN: Row = {
  id: "acme-widgets",
  domain: "acme-widgets.myshopify.com",
  adminTokenRef: "SHOPIFY_ACME-WIDGETS_ADMIN_TOKEN",
  storefrontTokenRef: "SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN",
  status: "live",
  isDevelopment: false,
};

const ID = "acme-widgets";

function verdict(over: Partial<Row> = {}): Record<string, Row> {
  return { [ID]: storeVerdictRow({ storeId: ID, domain: "acme-widgets.myshopify.com", ...over }) };
}

function seedFor(over: Partial<FakeSeed> & { both?: boolean; store?: Row } = {}): FakeSeed {
  const { both = false, store = OWN, ...rest } = over;
  const site = siteRow({ ...SHOP, binding: { storeId: ID }, previewBinding: both ? { storeId: ID } : {} } as never);
  return {
    sites: [site],
    stores: [store as never],
    previewReadiness: {
      "site-shop": previewReadinessRow({ siteId: "site-shop", storeId: ID, storeReadable: true, storeHasStorefrontToken: true, previewStoreHasStorefrontToken: both } as never),
    },
    ...rest,
  };
}

function connect(seed: FakeSeed): FakeConnection {
  const connection = fakeConnection(seed);
  const original = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
  vi.spyOn(connection.query, "executeNamed").mockImplementation(async (name, call, options) => {
    if (name === "externalConnectionsMine") return rowsResult([]);
    return original(name, call, options);
  });
  h.connection = connection;
  return connection;
}

function openPage(seed: FakeSeed): FakeConnection {
  const connection = connect(seed);
  const site = (seed.sites ?? [])[0]!;
  render(withSession(<ShopifyStorePanel site={siteFromRow(site)} canBind trail={[]} back={{ label: "Storefront", onSelect: vi.fn() }} onWritten={vi.fn()} />));
  return connection;
}

async function productionRow(word: string): Promise<HTMLElement> {
  const row = await screen.findByRole("button", { name: "Configure production store" });
  await waitFor(() => expect(row.textContent).toContain(word));
  return row;
}

function bar(): HTMLElement {
  return document.querySelector(".os-actbar") as HTMLElement;
}

function noticeWith(text: string | RegExp): HTMLElement {
  return screen.getByText(text).closest(".os-notice") as HTMLElement;
}

function memStore(): LocalDeployablesSettingsStore {
  const data = new Map<string, string>();
  return new LocalDeployablesSettingsStore({ getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) });
}

function mountApp(connection: FakeConnection, role = "owner") {
  h.connection = connection;
  h.shell = true;
  render(withSession(<DeployablesApp sectionId="deployables" navigate={vi.fn()} askContext={vi.fn()} store={memStore()} />, { role, userId: "u-me" }));
}

function storeSlot(page: HTMLElement): HTMLElement | null {
  return Array.from(page.querySelectorAll<HTMLElement>("button.deployable-slot")).find((b) => b.querySelector("strong")?.textContent === "Store") ?? null;
}

async function openDeployable(hostname: string): Promise<HTMLElement> {
  await waitFor(() => expect(document.querySelector("[data-os-livelist]")?.getAttribute("data-state")).toBe("live"));
  await click((await screen.findByText(hostname)).closest("button"));
  return (await screen.findByRole("region", { name: `Deployable ${hostname}` })).closest("[data-deployable-view]") as HTMLElement;
}

describe("the store's word", () => {
  it("draws Recording where the page said Connected, on the slot and on the row", async () => {
    mountApp(connect(seedFor({ storeVerdict: verdict() })));
    const page = await openDeployable("shop.memql.example.com");
    const slot = storeSlot(page)!;
    await waitFor(() => expect(slot.textContent).toContain("Recording"));
    expect(slot.textContent).not.toContain("Connected");
    expect(slot.hasAttribute("data-os-setup")).toBe(false);
    await click(slot);
    const row = await productionRow("Recording");
    expect(row.textContent).not.toContain("Connected");
    await waitFor(() => expect(bar().querySelector(".os-actbar-word")?.textContent).toBe("Recording"));
  });

  it("draws Starting while a backfill runs", async () => {
    openPage(seedFor({ storeVerdict: verdict({ state: "starting" }) }));
    const row = await productionRow("Starting");
    expect(row.textContent).not.toContain("Connected");
  });

  it("draws Paused and offers Resume, and only Resume", async () => {
    const connection = openPage(seedFor({ storeVerdict: verdict({ state: "paused" }) }));
    await productionRow("Paused");
    await waitFor(() => expect(within(bar()).getByRole("button", { name: "Resume" })).toBeTruthy());
    expect(within(bar()).queryByRole("button", { name: "Pause recording" })).toBeNull();
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Resume" }));
    await waitFor(() => expect(connection.callsNamed("shopifyStoreSetRecording")).toHaveLength(1));
    expect(connection.callsNamed("shopifyStoreSetRecording")[0]).toContain("recording: true");
    expect(connection.callsNamed("shopifyStoreSetRecording")[0]).toContain(`storeId: "${ID}"`);
  });

  it("offers Pause recording, and only that, to a store being recorded", async () => {
    const connection = openPage(seedFor({ storeVerdict: verdict() }));
    await productionRow("Recording");
    await waitFor(() => expect(within(bar()).getByRole("button", { name: "Pause recording" })).toBeTruthy());
    expect(within(bar()).queryByRole("button", { name: "Resume" })).toBeNull();
    await readTheBar();
    await click(within(bar()).getByRole("button", { name: "Pause recording" }));
    await waitFor(() => expect(connection.callsNamed("shopifyStoreSetRecording")[0]).toContain("recording: false"));
  });

  it("says Status unknown when the verdict cannot be read, and offers to read it again", async () => {
    const connection = openPage(seedFor({ storeVerdictError: "shopify: the store's sync state could not be read" }));
    const row = await productionRow("Status unknown");
    expect(row.textContent).not.toContain("Connected");
    expect(bar().getAttribute("data-tone")).not.toBe("live");
    const notice = await screen.findByText("Whether the store is being recorded could not be read.");
    const box = notice.closest(".os-notice") as HTMLElement;
    expect(box.textContent).toContain("shopify: the store's sync state could not be read");
    const before = connection.callsNamed("shopifyStoreVerdict").length;
    await click(within(box).getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(connection.callsNamed("shopifyStoreVerdict").length).toBeGreaterThan(before));
  });
});

describe("what needs attention", () => {
  const ATTENTION = {
    state: "needs attention",
    reasons: [
      verdictReasonRow({ code: "approval_outdated", sentence: "This store approved an older list of permissions.", detail: "read_returns", act: "approve", actLabel: "Open MemQL OS from the store's Shopify admin and approve", actUrl: "https://acme-widgets.myshopify.com/admin/apps" }),
      verdictReasonRow({ code: "events_not_registered", sentence: "Shopify is not sending every change.", detail: "orders/edited was not registered", act: "register", actLabel: "Register again" }),
      verdictReasonRow({ code: "changes_not_recorded", sentence: "At least 3 changes could not be recorded.", count: 3, atLeast: true, act: "retry", actLabel: "Try again" }),
    ],
  };

  it("draws each reason once for a store bound to both destinations", async () => {
    openPage(seedFor({ both: true, storeVerdict: verdict(ATTENTION) }));
    await productionRow("Needs attention");
    const testing = await screen.findByRole("button", { name: "Configure testing store" });
    await waitFor(() => expect(testing.textContent).toContain("Needs attention"));
    for (const sentence of ["This store approved an older list of permissions.", "Shopify is not sending every change.", "At least 3 changes could not be recorded."]) {
      expect(screen.getAllByText(sentence)).toHaveLength(1);
      expect(noticeWith(sentence).getAttribute("data-tone")).toBe("warn");
    }
  });

  it("gives approval_outdated a link to the store's Shopify admin", async () => {
    openPage(seedFor({ storeVerdict: verdict(ATTENTION) }));
    await productionRow("Needs attention");
    const notice = noticeWith("This store approved an older list of permissions.");
    expect(notice.textContent).toContain("read_returns");
    const link = within(notice).getByRole("link", { name: "Open MemQL OS from the store's Shopify admin and approve" });
    expect(link.getAttribute("href")).toBe("https://acme-widgets.myshopify.com/admin/apps");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toContain("noreferrer");
  });

  it("draws a reason that arrives with no act as its sentence and detail alone", async () => {
    // A store connected with its own app: the engine keeps approval_outdated
    // and names no act, because nothing in MemQL OS or Shopify's admin widens
    // that app's permissions. The notice has nothing to press and no empty
    // room where a button would be.
    openPage(seedFor({ storeVerdict: verdict({ state: "needs attention", reasons: [verdictReasonRow({ code: "approval_outdated", sentence: "This store's own app does not allow everything MemQL records.", detail: "read_returns" })] }) }));
    await productionRow("Needs attention");
    const notice = noticeWith("This store's own app does not allow everything MemQL records.");
    expect(notice.textContent).toContain("read_returns");
    expect(within(notice).queryByRole("button")).toBeNull();
    expect(within(notice).queryByRole("link")).toBeNull();
    expect(Array.from(notice.querySelectorAll("div")).some((div) => div.childElementCount === 0 && (div.textContent ?? "") === "")).toBe(false);
  });

  it("gives register and retry a button, and runs the builtin once", async () => {
    const connection = openPage(seedFor({ storeVerdict: verdict(ATTENTION) }));
    await productionRow("Needs attention");
    const register = noticeWith("Shopify is not sending every change.");
    expect(register.textContent).toContain("orders/edited was not registered");
    await click(within(register).getByRole("button", { name: "Register again" }));
    await waitFor(() => expect(connection.callsNamed("shopifyStoreRegisterEvents")).toHaveLength(1));
    expect(connection.callsNamed("shopifyStoreRegisterEvents")[0]).toContain(`storeId: "${ID}"`);
    await click(within(noticeWith("At least 3 changes could not be recorded.")).getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(connection.callsNamed("shopifyStoreRetryDeliveries")).toHaveLength(1));
    expect(connection.callsNamed("shopifyStoreRetryDeliveries")[0]).toContain(`storeId: "${ID}"`);
    expect(connection.callsNamed("shopifyStoreRegisterEvents")).toHaveLength(1);
  });

  it("shows the engine's refusal of Register again in its own words", async () => {
    const connection = openPage(seedFor({ storeVerdict: verdict(ATTENTION) }));
    await productionRow("Needs attention");
    const refusal = "shopify: this store's events were registered just now; try again in a minute";
    const execute = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
    vi.mocked(connection.query.executeNamed).mockImplementation(async (name, call, options) => {
      if (name === "shopifyStoreRegisterEvents") throw new Error(refusal);
      return execute(name, call, options);
    });
    await click(within(noticeWith("Shopify is not sending every change.")).getByRole("button", { name: "Register again" }));
    expect(await screen.findByText(refusal)).toBeTruthy();
  });

  it("draws no act for somebody who may not act on the store", async () => {
    openPage(seedFor({ storeVerdict: verdict({ ...ATTENTION, may: { act: false } }) }));
    await productionRow("Needs attention");
    expect(screen.getByText("This store approved an older list of permissions.")).toBeTruthy();
    expect(screen.getByText("Shopify is not sending every change.")).toBeTruthy();
    expect(screen.queryByRole("link", { name: /approve/ })).toBeNull();
    expect(screen.queryByRole("button", { name: "Register again" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    expect(within(bar()).queryByRole("button", { name: "Pause recording" })).toBeNull();
    expect(within(bar()).queryByRole("button", { name: "Resume" })).toBeNull();
  });
});

describe("what is recorded", () => {
  it("lists what is recorded, with orders since", async () => {
    openPage(seedFor({
      storeVerdict: verdict({
        recorded: [
          { kind: "catalog", lastChangeAt: "2026-11-11T09:00:00Z", lastCheckedAt: "2026-11-11T09:05:00Z" },
          { kind: "orders", lastChangeAt: "", lastCheckedAt: "2026-11-11T09:05:00Z" },
        ],
        ordersFrom: "2026-08-13",
      }),
    }));
    await productionRow("Recording");
    const panel = await screen.findByRole("region", { name: "What is recorded" });
    const catalog = within(panel).getByRole("group", { name: "Catalog" });
    expect(within(catalog).getByText("Last change")).toBeTruthy();
    expect(within(catalog).getByText("Last checked")).toBeTruthy();
    const orders = within(panel).getByRole("group", { name: "Orders" });
    // A change never recorded is a dash, never a time.
    expect(within(orders).getByText("--")).toBeTruthy();
    expect(panel.textContent).toMatch(/Orders since Aug 13, 2026/);
    expect(panel.querySelector(".os-notice")).toBeNull();
  });

  it("keeps what is outside the connection behind one closed line, never as an error", async () => {
    openPage(seedFor({
      storeVerdict: verdict({
        recorded: [{ kind: "orders", lastChangeAt: "2026-11-11T09:00:00Z", lastCheckedAt: "2026-11-11T09:05:00Z" }],
        outside: ["abandoned checkouts", "companies"],
      }),
    }));
    await productionRow("Recording");
    const panel = await screen.findByRole("region", { name: "What is recorded" });
    const details = panel.querySelector("details")!;
    expect(details.open).toBe(false);
    expect(details.querySelector("summary")?.textContent).toBe("Not part of this connection");
    expect(within(details).getByText("abandoned checkouts")).toBeTruthy();
    expect(within(details).getByText("companies")).toBeTruthy();
    expect(screen.getAllByText("companies")).toHaveLength(1);
    expect(document.querySelectorAll(".os-notice")).toHaveLength(0);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("names a field Shopify withholds without calling it an error", async () => {
    openPage(seedFor({
      storeVerdict: verdict({
        recorded: [{ kind: "catalog", lastChangeAt: "2026-11-11T09:00:00Z", lastCheckedAt: "2026-11-11T09:05:00Z" }],
        withheld: ["product variants: delivery profile", "orders: client ip"],
      }),
    }));
    await productionRow("Recording");
    const panel = await screen.findByRole("region", { name: "What is recorded" });
    expect(within(panel).getByText("Shopify withholds: product variants: delivery profile, orders: client ip")).toBeTruthy();
    expect(document.querySelectorAll(".os-notice")).toHaveLength(0);
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

describe("the alert", () => {
  it("says the alert could not be sent, and why", async () => {
    openPage(seedFor({
      storeVerdict: verdict({
        state: "needs attention",
        reasons: [verdictReasonRow({ code: "check_failing", sentence: "The last check of orders failed", detail: "the next check runs at 10:00" })],
        alert: { state: "failed", reason: "the mail server refused the message" },
      }),
    }));
    await productionRow("Needs attention");
    const notice = noticeWith("The alert could not be sent.");
    expect(notice.textContent).toContain("the mail server refused the message");
  });

  it("says nothing of a failed alert while the store is being recorded", async () => {
    openPage(seedFor({ storeVerdict: verdict({ alert: { state: "failed", reason: "the mail server refused the message" } }) }));
    await productionRow("Recording");
    expect(screen.queryByText("The alert could not be sent.")).toBeNull();
  });
});

describe("what speaks before the verdict", () => {
  it("keeps Uninstalled and its Reconnect notice", async () => {
    openPage(seedFor({
      store: { ...OWN, adminTokenRef: "", uninstalledAt: "2026-10-03T08:00:00Z" },
      storeVerdict: verdict({ state: "not connected" }),
    }));
    const row = await productionRow("Uninstalled");
    expect(row.textContent).not.toContain("Recording");
    const notice = noticeWith(/acme-widgets\.myshopify\.com was uninstalled in Shopify/);
    await click(within(notice).getByRole("button", { name: "Reconnect in Settings" }));
    expect(h.openApp).toHaveBeenCalledWith("deployables", "settings", { provider: "shopify" });
    // Neither recorded nor paused: nothing to pause or resume.
    expect(within(bar()).queryByRole("button", { name: "Pause recording" })).toBeNull();
    expect(within(bar()).queryByRole("button", { name: "Resume" })).toBeNull();
  });

  it("draws nothing of the store for somebody without the store part", async () => {
    const connection = connect(seedFor({ storeVerdict: verdict() }));
    mountApp(connection, "reader");
    const page = await openDeployable("shop.memql.example.com");
    expect(storeSlot(page)).toBeNull();
    expect(page.textContent).not.toContain("Recording");
    expect(connection.callsNamed("shopifyStoreVerdict")).toHaveLength(0);
  });
});

// FIX ROUND 1 (rulings 39 and 40, and the minors).

describe("a verdict that names no state is not Connected", () => {
  async function unknownEverywhere(connection: FakeConnection) {
    const row = await productionRow("Status unknown");
    expect(row.textContent).not.toContain("Connected");
    expect(row.querySelector('[data-tone="accent"]')).toBeNull();
    expect(bar().getAttribute("data-tone")).not.toBe("live");
    expect(bar().querySelector(".os-actbar-word")?.textContent).toBe("Status unknown");
    expect(within(bar()).queryByRole("button", { name: "Pause recording" })).toBeNull();
    expect(within(bar()).queryByRole("button", { name: "Resume" })).toBeNull();
    const box = (await screen.findByText("Whether the store is being recorded could not be read.")).closest(".os-notice") as HTMLElement;
    const before = connection.callsNamed("shopifyStoreVerdict").length;
    await click(within(box).getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(connection.callsNamed("shopifyStoreVerdict").length).toBeGreaterThan(before));
  }

  it("reads Status unknown for a reply that names no state", async () => {
    // Nothing seeded: the harness answers {status: "unconfigured"}, as an
    // engine with no Shopify connector does.
    await unknownEverywhere(openPage(seedFor()));
  });

  it("reads Status unknown for a reply naming a state this client does not know", async () => {
    await unknownEverywhere(openPage(seedFor({ storeVerdict: verdict({ state: "humming" }) })));
  });

  it("says Status unknown on the slot for a reply that names no state", async () => {
    mountApp(connect(seedFor()));
    const page = await openDeployable("shop.memql.example.com");
    const slot = storeSlot(page)!;
    await waitFor(() => expect(slot.textContent).toContain("Status unknown"));
    expect(slot.textContent).not.toContain("Connected");
  });

  it("draws Not connected over a row whose connection still reads connected", async () => {
    openPage(seedFor({ storeVerdict: verdict({ state: "not connected" }) }));
    const row = await productionRow("Not connected");
    expect(row.textContent?.replace("Not connected", "")).not.toContain("onnected");
    expect(bar().getAttribute("data-tone")).not.toBe("live");
    expect(bar().querySelector(".os-actbar-word")?.textContent).toBe("Not connected");
  });

  it("says Not connected on the slot over a connection that reads connected", async () => {
    mountApp(connect(seedFor({ storeVerdict: verdict({ state: "not connected" }) })));
    const page = await openDeployable("shop.memql.example.com");
    const slot = storeSlot(page)!;
    await waitFor(() => expect(slot.textContent).toContain("Not connected"));
    expect(slot.textContent?.replace("Not connected", "")).not.toContain("onnected");
  });
});

describe("an act that fails in its reply says so", () => {
  const REGISTER = { state: "needs attention", reasons: [verdictReasonRow({ code: "events_not_registered", sentence: "Shopify is not sending every change.", act: "register", actLabel: "Register again" })] };
  const RETRY = { state: "needs attention", reasons: [verdictReasonRow({ code: "changes_not_recorded", sentence: "2 changes could not be recorded.", count: 2, act: "retry", actLabel: "Try again" })] };

  it("shows the engine's detail of a failed register and runs the builtin once", async () => {
    const connection = openPage(seedFor({ storeVerdict: verdict(REGISTER), storeRegisterEventsReply: { status: "failed", detail: "orders/edited: Shopify answered 422", storeId: ID } as never }));
    await productionRow("Needs attention");
    const before = connection.callsNamed("shopifyStoreVerdict").length;
    await click(within(noticeWith("Shopify is not sending every change.")).getByRole("button", { name: "Register again" }));
    const refusal = await screen.findByText("orders/edited: Shopify answered 422");
    expect(refusal.closest(".os-notice")?.getAttribute("data-tone")).toBe("error");
    expect(connection.callsNamed("shopifyStoreRegisterEvents")).toHaveLength(1);
    await waitFor(() => expect(connection.callsNamed("shopifyStoreVerdict").length).toBeGreaterThan(before));
  });

  it("shows no error for a register that passed, and reads the verdict again", async () => {
    const connection = openPage(seedFor({ storeVerdict: verdict(REGISTER) }));
    await productionRow("Needs attention");
    const before = connection.callsNamed("shopifyStoreVerdict").length;
    await click(within(noticeWith("Shopify is not sending every change.")).getByRole("button", { name: "Register again" }));
    await waitFor(() => expect(connection.callsNamed("shopifyStoreVerdict").length).toBeGreaterThan(before));
    expect(connection.callsNamed("shopifyStoreRegisterEvents")).toHaveLength(1);
    expect(document.querySelector('.os-notice[data-tone="error"]')).toBeNull();
  });

  it("does not read a register an unconfigured engine answered as done", async () => {
    openPage(seedFor({ storeVerdict: verdict(REGISTER), storeRegisterEventsReply: { status: "unconfigured" } as never }));
    await productionRow("Needs attention");
    await click(within(noticeWith("Shopify is not sending every change.")).getByRole("button", { name: "Register again" }));
    await waitFor(() => expect(document.querySelector('.os-notice[data-tone="error"]')).not.toBeNull());
    expect(document.querySelector('.os-notice[data-tone="error"]')?.textContent).toContain("unconfigured");
  });

  it("does not read a retry an unconfigured engine answered as done", async () => {
    const connection = openPage(seedFor({ storeVerdict: verdict(RETRY), storeRetryDeliveriesReply: { status: "unconfigured" } as never }));
    await productionRow("Needs attention");
    await click(within(noticeWith("2 changes could not be recorded.")).getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(document.querySelector('.os-notice[data-tone="error"]')).not.toBeNull());
    expect(document.querySelector('.os-notice[data-tone="error"]')?.textContent).toContain("unconfigured");
    expect(connection.callsNamed("shopifyStoreRetryDeliveries")).toHaveLength(1);
  });
});

describe("each reason's act", () => {
  it("draws an act address that is not http or https as plain text, never a link", async () => {
    openPage(seedFor({ storeVerdict: verdict({ state: "needs attention", reasons: [verdictReasonRow({ code: "approval_outdated", sentence: "This store approved an older list of permissions.", act: "approve", actLabel: "Open MemQL OS from the store's Shopify admin and approve", actUrl: "javascript:alert(1)" })] }) }));
    await productionRow("Needs attention");
    const notice = noticeWith("This store approved an older list of permissions.");
    expect(within(notice).queryByRole("link")).toBeNull();
    expect(notice.textContent).toContain("Open MemQL OS from the store's Shopify admin and approve");
    expect(notice.querySelector("a")).toBeNull();
  });

  it("gives reconnect its label and leads to Settings", async () => {
    openPage(seedFor({ storeVerdict: verdict({ state: "needs attention", reasons: [verdictReasonRow({ code: "grant_lost", sentence: "MemQL can no longer reach this store.", detail: "401 Unauthorized", act: "reconnect", actLabel: "Reconnect in Settings" })] }) }));
    await productionRow("Needs attention");
    const notice = noticeWith("MemQL can no longer reach this store.");
    await click(within(notice).getByRole("button", { name: "Reconnect in Settings" }));
    expect(h.openApp).toHaveBeenCalledWith("deployables", "settings", { provider: "shopify" });
  });

  it("gives declare a link to its address, in a new tab", async () => {
    openPage(seedFor({ storeVerdict: verdict({ state: "needs attention", reasons: [verdictReasonRow({ code: "data_not_declared", domain: "customers", sentence: "Shopify refuses customers for this app, because the app has not declared the customer data it uses.", act: "declare", actLabel: "Declare the customer data", actUrl: "https://shopify.dev/docs/apps/launch/protected-customer-data" })] }) }));
    await productionRow("Needs attention");
    const link = within(noticeWith(/Shopify refuses customers/)).getByRole("link", { name: "Declare the customer data" });
    expect(link.getAttribute("href")).toBe("https://shopify.dev/docs/apps/launch/protected-customer-data");
    expect(link.getAttribute("target")).toBe("_blank");
  });

  it("sets the alert's reason as a caption, like every reason's detail", async () => {
    openPage(seedFor({ storeVerdict: verdict({ state: "needs attention", reasons: [verdictReasonRow({ code: "check_failing", sentence: "The last check of orders failed", detail: "the next check runs at 10:00" })], alert: { state: "failed", reason: "the mail server refused the message" } }) }));
    await productionRow("Needs attention");
    const reason = within(noticeWith("The alert could not be sent.")).getByText("the mail server refused the message");
    expect(reason.className).toBe("os-caption");
    expect(within(noticeWith("The last check of orders failed")).getByText("the next check runs at 10:00").className).toBe("os-caption");
  });
});

describe("which store a notice is about", () => {
  const OTHER: Row = { ...OWN, id: "acme-widgets-dev", domain: "acme-widgets-dev.myshopify.com", isDevelopment: true };
  const reasonFor = (sentence: string) => ({ state: "needs attention", reasons: [verdictReasonRow({ code: "check_failing", sentence })] });

  it("names the store on each notice when the destinations use two different stores", async () => {
    const site = siteRow({ ...SHOP, binding: { storeId: ID }, previewBinding: { storeId: "acme-widgets-dev" } } as never);
    openPage({
      sites: [site],
      stores: [OWN as never, OTHER as never],
      previewReadiness: { "site-shop": previewReadinessRow({ siteId: "site-shop", storeId: ID, storeReadable: true, storeHasStorefrontToken: true, previewStoreHasStorefrontToken: true } as never) },
      storeVerdict: {
        [ID]: storeVerdictRow({ storeId: ID, ...reasonFor("The last check of orders failed") }),
        "acme-widgets-dev": storeVerdictRow({ storeId: "acme-widgets-dev", ...reasonFor("The last check of products failed"), alert: { state: "failed", reason: "no owner address" } }),
      },
    });
    await productionRow("Needs attention");
    await waitFor(() => expect(screen.getByText("The last check of products failed")).toBeTruthy());
    expect(within(noticeWith("The last check of orders failed")).getByText("acme-widgets.myshopify.com")).toBeTruthy();
    expect(within(noticeWith("The last check of products failed")).getByText("acme-widgets-dev.myshopify.com")).toBeTruthy();
    expect(within(noticeWith("The alert could not be sent.")).getByText("acme-widgets-dev.myshopify.com")).toBeTruthy();
  });

  it("names no store when one store is bound to both", async () => {
    openPage(seedFor({ both: true, storeVerdict: verdict({ ...reasonFor("The last check of orders failed"), alert: { state: "failed", reason: "no owner address" } }) }));
    await productionRow("Needs attention");
    expect(noticeWith("The last check of orders failed").textContent).not.toContain("acme-widgets.myshopify.com");
    expect(noticeWith("The alert could not be sent.").textContent).not.toContain("acme-widgets.myshopify.com");
  });
});
