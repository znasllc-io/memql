import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { intersect } from "../intersectionObserver";
import { readTheBarNow, useFakeDate } from "../readTheBar";
import { installSeededAccess, seededAccessWithout } from "../seededAccess";
import { setEffectiveCapabilities } from "../../src/system/roles";
import { AuthSourceProvider } from "../../src/auth/context";
import { installAccountPageDisplayRules } from "./pageDisplay";

// THE EXPORT TAB AND "EXPORT THIS ACCOUNT" (design record
// 2026-11-12-account-export, sections F, G and K). Everything here belongs to
// `execute app:accounts/export`, which the seeds give owner and developer; the
// tab is absent without it.
//
// The seven calls are the engine's, called by name; the connection is faked as
// the rest of the accounts suite fakes it. The download is a bearer fetch on
// the OS origin, faked at `fetch`.

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { AccountsApp } = await import("../../src/apps/accounts/AccountsApp");
const { LocalAccountsSettingsStore } = await import("../../src/apps/accounts/settings");
const { SELF_ACCOUNT_ID } = await import("../../src/apps/accounts/rows");
const { accountRow, accountTabNames, fakeConnection, rowsResult, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;
type Answer = Row[] | Error | Promise<Row[]>;
type Handler = (call: string) => Answer;
type Name =
  | "accountExportReadiness"
  | "accountExportPrepare"
  | "accountExportStart"
  | "accountExportCancel"
  | "accountExportRemove"
  | "accountExportStatus"
  | "accountExports";

const ACCOUNTS = [
  accountRow({ id: SELF_ACCOUNT_ID, name: "Our Studio", createdAt: "2026-08-03T00:00:00Z" }),
  accountRow({ id: "a1", name: "Acme Consulting", createdAt: "2026-08-02T00:00:00Z" }),
];

const INVENTORY = [
  { kind: "concept", concept: "v1:library:artifact", namespace: "library", name: "artifact", rows: 3, versions: 3 },
  { kind: "concept", concept: "v1:notes:note", namespace: "notes", name: "note", rows: 2, versions: 2 },
  { kind: "files", bytes: 2048 },
];

const PASSPHRASE = "ABCDE-FGHJK-MNPQR-STVWX-YZ234";

const condition = (name: string, met: boolean, extra: Record<string, unknown> = {}) => ({ kind: "condition", name, met, ...extra });

const READY = [
  condition("recording", true), condition("assigned", true), condition("declared", true), condition("decided", true), condition("followed", true),
  { kind: "unplaced", rows: 7 },
  { kind: "ready", ready: true },
];

const NOT_READY = [
  condition("recording", true),
  condition("assigned", false, { kindsTotal: 9, kindsFinished: 4, waiting: 12 }),
  condition("declared", false, { concepts: ["v1:zebra:stripe"] }),
  condition("decided", false, { rows: 2 }),
  condition("followed", true),
  { kind: "unplaced", rows: 0 },
  { kind: "ready", ready: false },
];

function exportRow(id: string, over: Record<string, unknown> = {}) {
  return {
    kind: "export", id, accountId: "a1", accountName: "Acme Consulting", requestedBy: "u-dana", requestedByName: "Dana Reyes",
    requestedAt: "2026-11-12T09:00:00Z", state: "complete", phase: "", protected: true, attempt: 1,
    startedAt: "2026-11-12T09:00:05Z", finishedAt: "2026-11-12T09:03:00Z", kindsTotal: 2, kindsDone: 2,
    rowsWritten: 5, versionsWritten: 5, bytesTotal: 2048, bytesDone: 2048, fileName: "acme-2026-11-12.memql-account",
    sizeBytes: 4096, sha256: "c0ffee", report: null, failureCode: "", failureDetail: "", removedAt: "", removedBy: "", removedByName: "", ...over,
  };
}

function connection(handlers: Partial<Record<Name, Handler>>) {
  const conn = fakeConnection({ clientAccountsAll: ACCOUNTS, accountInventory: INVENTORY, accountAssignmentStatus: [{ kind: "switch", enabled: true, held: false }] });
  const original = conn.query.executeNamed.getMockImplementation()!;
  const all: Partial<Record<Name, Handler>> = { accountExportReadiness: () => READY, accountExports: () => [{ kind: "page", nextCursor: "" }], ...handlers };
  conn.query.executeNamed.mockImplementation(async (name: string, call: string) => {
    const handler = all[name as Name];
    if (handler === undefined) {
      if (name === "accountUnplaced") return rowsResult([]);
      return original(name, call);
    }
    const answer = await handler(call);
    if (answer instanceof Error) throw answer;
    return rowsResult(answer as never);
  });
  return conn;
}

function callsTo(conn: Conn, name: string): string[] {
  return conn.query.executeNamed.mock.calls.filter((c) => c[0] === name).map((c) => c[1] as string);
}

const BEARER = { bearer: async () => "test-bearer", refresh: async () => "test-bearer" };

function mount(conn: Conn) {
  h.connection = conn;
  const bag = new Map<string, string>();
  const store = new LocalAccountsSettingsStore({ getItem: (k: string) => bag.get(k) ?? null, setItem: (k: string, v: string) => void bag.set(k, v) });
  return render(withSession(
    <AuthSourceProvider source={BEARER}>
      <AccountsApp sectionId="accounts" navigate={vi.fn()} askContext={() => {}} store={store} />
    </AuthSourceProvider>,
  ));
}

async function openExport(conn: Conn, account = "Acme Consulting") {
  mount(conn);
  fireEvent.click(await screen.findByText(account));
  return openTab("Export");
}

async function openTab(name: string) {
  const tabs = await screen.findByRole("navigation", { name: "Account views" });
  await act(async () => {
    fireEvent.click(within(tabs).getByRole("button", { name }));
  });
  return screen.findByRole("region", { name });
}

const bar = () => screen.queryByRole("group", { name: "What you can do with this" });
const pressInBar = (name: string) => {
  readTheBarNow();
  fireEvent.click(within(bar()!).getByRole("button", { name }));
};
const crumbs = () => within(screen.getByRole("navigation", { name: "Breadcrumbs" })).getAllByRole("listitem").map((li) => li.textContent);

afterEach(() => installSeededAccess("owner"));

describe("the Export tab", () => {
  it("is absent without the export part, and nothing is asked", async () => {
    setEffectiveCapabilities(seededAccessWithout("owner", "app:accounts/export"));
    const conn = connection({});
    mount(conn);
    fireEvent.click(await screen.findByText("Acme Consulting"));
    await screen.findByRole("navigation", { name: "Account views" });
    expect(accountTabNames()).toEqual(["Details", "People", "Data", "Settings"]);
    expect(callsTo(conn, "accountExportReadiness")).toHaveLength(0);
    expect(callsTo(conn, "accountExports")).toHaveLength(0);
  });

  it("stands between Data and Settings for a holder", async () => {
    const conn = connection({});
    mount(conn);
    fireEvent.click(await screen.findByText("Acme Consulting"));
    await screen.findByRole("navigation", { name: "Account views" });
    expect(accountTabNames()).toEqual(["Details", "People", "Data", "Export", "Settings"]);
    expect(callsTo(conn, "accountExportReadiness")).toHaveLength(0);
  });

  it("names the five conditions with their state and counts when not ready, and offers no Add", async () => {
    const conn = connection({ accountExportReadiness: () => NOT_READY });
    const tab = await openExport(conn);
    const readiness = await within(tab).findByRole("region", { name: "Readiness" });
    expect(within(readiness).getByText("Accounts recorded on rows")).toBeTruthy();
    expect(within(readiness).getByText("Recorded")).toBeTruthy();
    expect(within(readiness).getByText("4 of 9 kinds finished, 12 rows waiting")).toBeTruthy();
    expect(within(readiness).getByText("1 kind does not: stripe")).toBeTruthy();
    expect(within(readiness).getByText("2 rows awaiting")).toBeTruthy();
    expect(within(tab).queryByRole("button", { name: "Export this account" })).toBeNull();
    expect(callsTo(conn, "accountExportReadiness")).toEqual(['builtin accountExportReadiness(accountId: "a1")']);
    expect(callsTo(conn, "accountExports")).toHaveLength(0);
  });

  it("says why existing rows are not yet assigned when every kind has finished but a pass that was asked for has not", async () => {
    const conn = connection({ accountExportReadiness: () => [
      condition("recording", true),
      condition("assigned", false, { kindsTotal: 12, kindsFinished: 12, waiting: 0, pendingPasses: 2 }),
      condition("declared", true), condition("decided", true), condition("followed", true),
      { kind: "unplaced", rows: 0 },
      { kind: "ready", ready: false },
    ] });
    const tab = await openExport(conn);
    const readiness = await within(tab).findByRole("region", { name: "Readiness" });
    expect(within(readiness).getByText("12 of 12 kinds finished. 2 kinds are finishing a pass that was asked for.")).toBeTruthy();
  });

  it("says a failed read of the kinds' declarations as a failed read, not as a fact", async () => {
    const conn = connection({ accountExportReadiness: () => [
      condition("recording", true), condition("assigned", true),
      condition("declared", false, { concepts: [] }),
      condition("decided", true), condition("followed", true),
      { kind: "unplaced", rows: 0 },
      { kind: "ready", ready: false },
    ] });
    const tab = await openExport(conn);
    const readiness = await within(tab).findByRole("region", { name: "Readiness" });
    expect(within(readiness).queryByText("Not every kind does")).toBeNull();
    expect(within(readiness).getByText("Could not be read")).toBeTruthy();
  });

  it("says a queued export is waiting to start, never that it is working", async () => {
    const conn = connection({ accountExports: () => [exportRow("x7", { state: "queued", phase: "", sizeBytes: 0 }), { kind: "page", nextCursor: "" }] });
    const tab = await openExport(conn);
    const list = await within(tab).findByRole("list", { name: "Exports" });
    expect(within(list).getByText("Waiting to start")).toBeTruthy();
    expect(within(list).queryByText("Working")).toBeNull();
  });

  it("does not offer Export this account while an export of it is in progress", async () => {
    const conn = connection({ accountExports: () => [exportRow("x2", { state: "running", phase: "rows", sizeBytes: 0 }), exportRow("x1"), { kind: "page", nextCursor: "" }] });
    const tab = await openExport(conn);
    await within(tab).findByRole("list", { name: "Exports" });
    expect(within(tab).queryByRole("button", { name: "Export this account" })).toBeNull();
  });

  describe("a prepared export the cluster removed", () => {
    useFakeDate();

    it("drops a prepared export the cluster removed, at the next refresh, and stops reading every three seconds", async () => {
      let swept = false;
      const conn = connection({
        accountExports: () => swept ? [{ kind: "page", nextCursor: "" }] : [exportRow("x5", { state: "draft", protected: false, sizeBytes: 0 }), { kind: "page", nextCursor: "" }],
        accountExportStatus: () => [exportRow("x5", { state: "draft", protected: false, sizeBytes: 0 })],
      });
      const tab = await openExport(conn);
      fireEvent.click(await within(tab).findByRole("button", { name: /Prepared/ }));
      await screen.findByRole("heading", { name: "Export this account" });
      swept = true;
      pressInBar("Leave");
      const back = await screen.findByRole("region", { name: "Export" });
      await waitFor(() => expect(within(back).queryByRole("button", { name: /Prepared/ })).toBeNull());
      expect(within(back).getByText("No exports yet.")).toBeTruthy();
      // Nothing is active, so the list is read on the idle cadence, not every three seconds.
      const asked = callsTo(conn, "accountExports").length;
      await act(async () => new Promise((resolve) => setTimeout(resolve, 3_500)));
      expect(callsTo(conn, "accountExports")).toHaveLength(asked);
    }, 10_000);

    it("drops a removed prepared export the first page covers, and keeps the rows of later pages", async () => {
      let swept = false;
      const conn = connection({
        accountExports: (call) => call.includes('after: "c2"')
          ? [exportRow("x0", { requestedAt: "2026-11-01T09:00:00Z" }), { kind: "page", nextCursor: "" }]
          : [
            ...(swept ? [] : [exportRow("x5", { requestedAt: "2026-11-12T10:00:00Z", state: "draft", protected: false, sizeBytes: 0 })]),
            exportRow("x3", { requestedAt: "2026-11-12T09:00:00Z" }),
            { kind: "page", nextCursor: "c2" },
          ],
        accountExportStatus: () => [exportRow("x5", { requestedAt: "2026-11-12T10:00:00Z", state: "draft", protected: false, sizeBytes: 0 })],
      });
      const tab = await openExport(conn);
      await within(tab).findByRole("list", { name: "Exports" });
      await act(async () => intersect(document.querySelector(".os-next-page-sentinel")!));
      await waitFor(() => expect(within(within(tab).getByRole("list", { name: "Exports" })).getAllByRole("listitem")).toHaveLength(3));
      fireEvent.click(within(tab).getByRole("button", { name: /Prepared/ }));
      await screen.findByRole("heading", { name: "Export this account" });
      swept = true;
      pressInBar("Leave");
      const back = await screen.findByRole("region", { name: "Export" });
      await waitFor(() => expect(within(back).queryByRole("button", { name: /Prepared/ })).toBeNull());
      expect(within(within(back).getByRole("list", { name: "Exports" })).getAllByRole("listitem")).toHaveLength(2);
    });
  });

  it("lists the exports when ready, each with when, state, size, protection and who started it", async () => {
    const conn = connection({
      accountExports: () => [
        exportRow("x2", { requestedAt: "2026-11-12T10:00:00Z", state: "running", phase: "rows", rowsWritten: 40, kindsDone: 1, sizeBytes: 0 }),
        exportRow("x1", { protected: false }),
        { kind: "page", nextCursor: "" },
      ],
    });
    const tab = await openExport(conn);
    const list = await within(tab).findByRole("list", { name: "Exports" });
    const items = within(list).getAllByRole("listitem");
    expect(items).toHaveLength(2);
    expect(within(items[0]!).getByText("Running")).toBeTruthy();
    expect(within(items[0]!).getByText("Writing rows: 40 rows, 1 of 2 kinds")).toBeTruthy();
    expect(within(items[0]!).getByText("Protected")).toBeTruthy();
    expect(within(items[1]!).getByText("Complete")).toBeTruthy();
    expect(within(items[1]!).getByText("4.0 KiB")).toBeTruthy();
    expect(within(items[1]!).queryByText("Protected")).toBeNull();
    expect(within(items[1]!).getByText("Started by Dana Reyes")).toBeTruthy();
    // The person's id is never printed.
    expect(within(list).queryByText(/u-dana/)).toBeNull();
  });

  it("loads the next page of exports on scroll, passing the cursor back", async () => {
    const conn = connection({
      accountExports: (call) => call.includes('after: "c2"')
        ? [exportRow("x0", { requestedAt: "2026-11-01T09:00:00Z", fileName: "older.memql-account" }), { kind: "page", nextCursor: "" }]
        : [exportRow("x1"), { kind: "page", nextCursor: "c2" }],
    });
    const tab = await openExport(conn);
    await within(tab).findByRole("list", { name: "Exports" });
    // The sentinel has no role or text of its own; it is found by its class.
    await act(async () => intersect(document.querySelector(".os-next-page-sentinel")!));
    await waitFor(() => expect(within(within(tab).getByRole("list", { name: "Exports" })).getAllByRole("listitem")).toHaveLength(2));
    expect(callsTo(conn, "accountExports")[1]).toBe('builtin accountExports(accountId: "a1", limit: 20, after: "c2")');
  });

  it("leaves out who started or removed an export when the answer carries no name, and never prints the id", async () => {
    const conn = connection({ accountExports: () => [
      exportRow("x1", { requestedByName: "", removedAt: "2026-11-12T11:00:00Z", removedBy: "u-lee", removedByName: "" }),
      { kind: "page", nextCursor: "" },
    ] });
    const tab = await openExport(conn);
    expect(await within(tab).findByText(/^Removed, /)).toBeTruthy();
    expect(within(tab).queryByText(/Started by/)).toBeNull();
    expect(within(tab).queryByText(/u-lee|u-dana|someone/)).toBeNull();
  });

  it("shows a removed export with who removed it and when, and offers no Download", async () => {
    const conn = connection({ accountExports: () => [exportRow("x1", { removedAt: "2026-11-12T11:00:00Z", removedBy: "u-lee", removedByName: "Lee Park" }), { kind: "page", nextCursor: "" }] });
    const tab = await openExport(conn);
    expect(await within(tab).findByText(/^Removed by Lee Park, /)).toBeTruthy();
    expect(within(tab).getByText("Removed")).toBeTruthy();
    expect(within(tab).queryByRole("button", { name: /^Download/ })).toBeNull();
  });

  it("says why a run failed in words, with the engine's detail, and never the code", async () => {
    const conn = connection({
      accountExports: () => [
        exportRow("x2", { state: "failed", failureCode: "export_verify_failed", failureDetail: "member rows/notes/note/part-000001 checksum differs", sizeBytes: 0 }),
        exportRow("x1", { state: "failed", failureCode: "export_brand_new", failureDetail: "", sizeBytes: 0, requestedAt: "2026-11-11T09:00:00Z" }),
        { kind: "page", nextCursor: "" },
      ],
    });
    const tab = await openExport(conn);
    expect(await within(tab).findByText("The file did not read back as it was written, so it was not kept.")).toBeTruthy();
    expect(within(tab).getByText("member rows/notes/note/part-000001 checksum differs")).toBeTruthy();
    expect(within(tab).getByText("The export did not finish.")).toBeTruthy();
    expect(within(tab).queryByText(/export_verify_failed|export_brand_new/)).toBeNull();
  });

  it("links the count of rows in no account to the primary account's Data tab", async () => {
    const conn = connection({});
    const tab = await openExport(conn);
    expect(await within(tab).findByText("7 rows in no account")).toBeTruthy();
    fireEvent.click(within(tab).getByRole("button", { name: "Rows in no account" }));
    await waitFor(() => expect(crumbs()).toEqual(["Accounts", "Our Studio", "Data"]));
    expect(screen.getByRole("region", { name: "Data" })).toBeTruthy();
  });

  describe("a complete export", () => {
    useFakeDate();

    it("downloads through the content path with the bearer", async () => {
      const fetchSpy = vi.fn(async (_url: RequestInfo | URL, _init?: RequestInit) => new Response(new Blob(["bytes"]), { status: 200 }));
      vi.stubGlobal("fetch", fetchSpy);
      const created = vi.fn(() => "blob:x");
      Object.assign(URL, { createObjectURL: created, revokeObjectURL: vi.fn() });
      const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
      try {
        const conn = connection({ accountExports: () => [exportRow("x1"), { kind: "page", nextCursor: "" }] });
        const tab = await openExport(conn);
        fireEvent.click(await within(tab).findByRole("button", { name: /^Download the export of / }));
        await waitFor(() => expect(fetchSpy).toHaveBeenCalledOnce());
        expect(String(fetchSpy.mock.calls[0]![0])).toBe("/_memql/accounts/exports/x1/content");
        expect((fetchSpy.mock.calls[0]![1]!.headers as Record<string, string>).Authorization).toBe("Bearer test-bearer");
        await waitFor(() => expect(click).toHaveBeenCalled());
      } finally {
        click.mockRestore();
        vi.unstubAllGlobals();
      }
    });

    it("is removed after asking in place", async () => {
      const conn = connection({
        accountExports: () => [exportRow("x1"), { kind: "page", nextCursor: "" }],
        accountExportRemove: () => [exportRow("x1", { removedAt: "2026-11-12T11:00:00Z", removedBy: "u-me", removedByName: "Sam Owner" })],
      });
      const tab = await openExport(conn);
      fireEvent.click(await within(tab).findByRole("button", { name: "Remove this export" }));
      const ask = within(tab).getByRole("group", { name: "Remove this export?" });
      // The second click of the double-click that asked answers nothing.
      fireEvent.click(within(ask).getByRole("button", { name: "Remove" }));
      expect(callsTo(conn, "accountExportRemove")).toHaveLength(0);
      readTheBarNow();
      fireEvent.click(within(ask).getByRole("button", { name: "Remove" }));
      await waitFor(() => expect(callsTo(conn, "accountExportRemove")).toEqual(['builtin accountExportRemove(exportId: "x1")']));
      expect(await within(tab).findByText(/^Removed by Sam Owner, /)).toBeTruthy();
    });

    it("says a refused remove in words, never its code", async () => {
      const conn = connection({
        accountExports: () => [exportRow("x1"), { kind: "page", nextCursor: "" }],
        accountExportRemove: () => new Error("export_in_progress: export x1 is being downloaded"),
      });
      const tab = await openExport(conn);
      fireEvent.click(await within(tab).findByRole("button", { name: "Remove this export" }));
      readTheBarNow();
      fireEvent.click(within(tab).getByRole("button", { name: "Remove" }));
      expect(await within(tab).findByText("export x1 is being downloaded")).toBeTruthy();
      expect(within(tab).getByText("The export was not removed.")).toBeTruthy();
      expect(within(tab).queryByText(/export_in_progress/)).toBeNull();
    });

    it("says a refused download in words, and downloads when tried again", async () => {
      let refuse = true;
      const fetchSpy = vi.fn(async () => refuse ? new Response("not found", { status: 404 }) : new Response(new Blob(["bytes"]), { status: 200 }));
      vi.stubGlobal("fetch", fetchSpy);
      Object.assign(URL, { createObjectURL: vi.fn(() => "blob:x"), revokeObjectURL: vi.fn() });
      const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
      try {
        const conn = connection({ accountExports: () => [exportRow("x1"), { kind: "page", nextCursor: "" }] });
        const tab = await openExport(conn);
        const download = await within(tab).findByRole("button", { name: /^Download the export of / });
        fireEvent.click(download);
        expect(await within(tab).findByText("The file was not downloaded.")).toBeTruthy();
        expect(within(tab).getByText("not found")).toBeTruthy();
        expect(click).not.toHaveBeenCalled();
        refuse = false;
        fireEvent.click(within(tab).getByRole("button", { name: /^Download the export of / }));
        await waitFor(() => expect(click).toHaveBeenCalled());
        expect(fetchSpy).toHaveBeenCalledTimes(2);
        await waitFor(() => expect(within(tab).queryByText("The file was not downloaded.")).toBeNull());
      } finally {
        click.mockRestore();
        vi.unstubAllGlobals();
      }
    });
  });
});

describe("Export this account", () => {
  useFakeDate();

  let stored: string[] = [];
  let logged: string[] = [];
  beforeEach(() => {
    stored = [];
    logged = [];
    localStorage.clear();
    sessionStorage.clear();
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(function (this: Storage, key: string, value: string) {
      stored.push(`${key}=${value}`);
    });
    for (const level of ["log", "info", "warn", "error", "debug"] as const) {
      vi.spyOn(console, level).mockImplementation((...args: unknown[]) => void logged.push(args.map(String).join(" ")));
    }
  });
  afterEach(() => vi.restoreAllMocks());

  async function openWizard(conn: Conn) {
    const tab = await openExport(conn);
    fireEvent.click(await within(tab).findByRole("button", { name: "Export this account" }));
    return screen.findByRole("heading", { name: "Export this account" });
  }

  const STATUS_COMPLETE = () => [exportRow("x9", {
    report: { bytes: { missing: 1, notCarriedObjects: 2, notCarriedBytes: 1024 }, scrubbedFields: [{ concept: "v1:shopify:store", field: "adminToken" }] },
  })];

  it("keeps the retained account page out of layout behind the export wizard and restores it on cancel", async () => {
    const removeStyles = installAccountPageDisplayRules();
    try {
      const conn = connection({});
      await openWizard(conn);
      const page = document.querySelector<HTMLElement>(".os-account-page")!;
      expect(page.hidden).toBe(true);
      expect(getComputedStyle(page).display).toBe("none");
      pressInBar("Cancel");
      await screen.findByRole("region", { name: "Export" });
      expect(document.querySelector(".os-account-page")).toBe(page);
      expect(getComputedStyle(page).display).toBe("flex");
      expect(callsTo(conn, "accountExportPrepare")).toEqual([]);
    } finally {
      removeStyles();
    }
  });

  it("reads the inventory the Data tab read, and says what a file leaves out", async () => {
    const conn = connection({});
    mount(conn);
    fireEvent.click(await screen.findByText("Acme Consulting"));
    const data = await openTab("Data");
    await within(data).findByRole("button", { name: /^Files, / });
    const tab = await openTab("Export");
    fireEvent.click(await within(tab).findByRole("button", { name: "Export this account" }));
    await screen.findByRole("heading", { name: "Export this account" });
    expect(screen.getByText("2 kinds")).toBeTruthy();
    expect(screen.getByText("5 rows")).toBeTruthy();
    expect(screen.getByText("2.0 KiB")).toBeTruthy();
    for (const what of ["Sign-in material and visits", "Work in transit", "What is bound to a machine", "The installation's own rows", "Secret values", "Attachments' and older bundles' bytes"]) {
      expect(screen.getByText(what)).toBeTruthy();
    }
    // One read for both.
    expect(callsTo(conn, "accountInventory")).toHaveLength(1);
  });

  it("shows the documented too-large limit on Contents as a plain note, not as an error", async () => {
    const sentence = "This account holds too many rows to count here. An export does not depend on these counts.";
    const conn = connection({});
    const original = conn.query.executeNamed.getMockImplementation()!;
    conn.query.executeNamed.mockImplementation(async (n: string, call: string) => {
      if (n === "accountInventory") throw new Error(`account_inventory_too_large: ${sentence}`);
      return original(n, call);
    });
    await openWizard(conn);
    expect(await screen.findByText(sentence)).toBeTruthy();
    expect(screen.queryByText("What this account holds was not read.")).toBeNull();
    expect(screen.queryByText(/account_inventory_too_large/)).toBeNull();
    expect(within(bar()!).getByRole("button", { name: "Continue" })).toBeTruthy();
  });

  it("walks a protected export: the passphrase shown once, never stored, gone once started", async () => {
    const conn = connection({
      accountExportPrepare: () => [{ ...exportRow("x9", { state: "draft", sizeBytes: 0, fileName: "" }), passphrase: PASSPHRASE }],
      accountExportStart: () => [exportRow("x9", { state: "queued", sizeBytes: 0 })],
      accountExportStatus: STATUS_COMPLETE,
    });
    await openWizard(conn);
    pressInBar("Continue");
    // Protection: one switch, on.
    expect((screen.getByRole("switch", { name: /Protect the file/ }) as HTMLInputElement).checked).toBe(true);
    expect(callsTo(conn, "accountExportPrepare")).toHaveLength(0);
    pressInBar("Continue");
    await waitFor(() => expect(screen.getByText(PASSPHRASE)).toBeTruthy());
    expect(callsTo(conn, "accountExportPrepare")).toEqual(['builtin accountExportPrepare(accountId: "a1", protect: true)']);
    // Copy is offered; Back is held; the forward act waits for the tick.
    expect(screen.getByRole("button", { name: "Copy passphrase" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Back to / })).toBeNull();
    expect(within(bar()!).queryByRole("button", { name: "Start export" })).toBeNull();
    fireEvent.click(screen.getByRole("checkbox", { name: "I have saved it" }));
    pressInBar("Start export");
    await waitFor(() => expect(callsTo(conn, "accountExportStart")).toEqual(['builtin accountExportStart(exportId: "x9")']));
    // Gone from the page once the step is left forward.
    expect(screen.queryByText(PASSPHRASE)).toBeNull();

    // The run lands; Download says the file and what the report flags.
    await waitFor(() => expect(screen.getByText("acme-2026-11-12.memql-account")).toBeTruthy());
    expect(screen.getByText("c0ffee")).toBeTruthy();
    expect(screen.getByText("1 file was missing at the source and is not in the file.")).toBeTruthy();
    expect(screen.getByText("Secret values removed: store.adminToken.")).toBeTruthy();
    pressInBar("Done");
    expect(await screen.findByRole("region", { name: "Export" })).toBeTruthy();

    // Never written to storage of any kind, never logged.
    expect(stored.some((s) => s.includes(PASSPHRASE))).toBe(false);
    expect(Object.values({ ...localStorage }).some((v) => String(v).includes(PASSPHRASE))).toBe(false);
    expect(Object.values({ ...sessionStorage }).some((v) => String(v).includes(PASSPHRASE))).toBe(false);
    expect(logged.some((l) => l.includes(PASSPHRASE))).toBe(false);
    expect(document.body.innerHTML.includes(PASSPHRASE)).toBe(false);
  });

  it("asks in the bar before switching protection off, and then starts on Start export", async () => {
    const conn = connection({
      accountExportPrepare: () => [exportRow("x9", { state: "draft", protected: false, sizeBytes: 0 })],
      accountExportStart: () => [exportRow("x9", { state: "queued", protected: false, sizeBytes: 0 })],
      accountExportStatus: () => [exportRow("x9", { state: "running", phase: "checking", protected: false, sizeBytes: 0 })],
    });
    await openWizard(conn);
    pressInBar("Continue");
    const protect = screen.getByRole("switch", { name: /Protect the file/ }) as HTMLInputElement;
    fireEvent.click(protect);
    expect(within(bar()!).getByText(/Export without a passphrase\?/)).toBeTruthy();
    pressInBar("Keep protection");
    expect((screen.getByRole("switch", { name: /Protect the file/ }) as HTMLInputElement).checked).toBe(true);
    fireEvent.click(screen.getByRole("switch", { name: /Protect the file/ }));
    pressInBar("Export without one");
    expect((screen.getByRole("switch", { name: /Protect the file/ }) as HTMLInputElement).checked).toBe(false);
    pressInBar("Start export");
    await waitFor(() => expect(callsTo(conn, "accountExportStart")).toHaveLength(1));
    expect(callsTo(conn, "accountExportPrepare")).toEqual(['builtin accountExportPrepare(accountId: "a1", protect: false)']);
    // No passphrase step on the way.
    expect(screen.queryByRole("checkbox", { name: "I have saved it" })).toBeNull();
    await waitFor(() => expect(within(bar()!).getByText("Running")).toBeTruthy());
  });

  it("shows a refusal from prepare in the engine's words, never its code", async () => {
    const conn = connection({ accountExportPrepare: () => new Error("export_in_progress: export x7 is running") });
    await openWizard(conn);
    pressInBar("Continue");
    pressInBar("Continue");
    expect(await screen.findByText("Another export of this account is already prepared or running.")).toBeTruthy();
    expect(screen.getByText("export x7 is running")).toBeTruthy();
    expect(screen.queryByText(/export_in_progress/)).toBeNull();
    expect(within(bar()!).getByRole("button", { name: "Try again" })).toBeTruthy();
  });

  it("reopens a running export at its stage, and cancels it only after asking by name", async () => {
    const running = exportRow("x2", { state: "running", phase: "bytes", bytesDone: 1024, bytesTotal: 2048, sizeBytes: 0 });
    // The engine's real answer: a running export stays running with the
    // cancel asked; the runner writes cancelled at its next heartbeat.
    const asked = { ...running, cancelRequested: true };
    let cancelled = false;
    let after = 0;
    const conn = connection({
      accountExports: () => [running, { kind: "page", nextCursor: "" }],
      accountExportStatus: () => {
        if (!cancelled) return [running];
        after += 1;
        return after === 1 ? [asked] : [exportRow("x2", { state: "cancelled", sizeBytes: 0 })];
      },
      accountExportCancel: () => {
        cancelled = true;
        return [asked];
      },
    });
    const tab = await openExport(conn);
    fireEvent.click(await within(tab).findByRole("button", { name: /Running/ }));
    expect(await screen.findByRole("heading", { name: "Export this account" })).toBeTruthy();
    await waitFor(() => expect(within(bar()!).getByText("Running")).toBeTruthy());
    expect(within(bar()!).getByText(/^Copying files: 1\.0 KiB of 2\.0 KiB/)).toBeTruthy();
    pressInBar("Cancel");
    expect(within(bar()!).getByText("Cancel the export of Acme Consulting? What has been written so far is removed.")).toBeTruthy();
    expect(callsTo(conn, "accountExportCancel")).toHaveLength(0);
    pressInBar("Cancel export");
    await waitFor(() => expect(callsTo(conn, "accountExportCancel")).toEqual(['builtin accountExportCancel(exportId: "x2")']));
    // Asked, not yet done: the bar says so, and Cancel is not offered again.
    await waitFor(() => expect(within(bar()!).getByText("Cancelling")).toBeTruthy());
    expect(within(bar()!).queryByRole("button", { name: "Cancel" })).toBeNull();
    expect(within(bar()!).getByRole("button", { name: "Leave" })).toBeTruthy();
    // The runner's next heartbeat writes cancelled; a later status says it.
    // The Date is fake, so the clock is moved on for the status cadence.
    await waitFor(() => {
      vi.setSystemTime(Date.now() + 1_100);
      expect(within(bar()!).getByText("Cancelled")).toBeTruthy();
    }, { timeout: 6_000 });
  }, 12_000);

  it("says Cancelling in the list for a running export whose cancel was asked", async () => {
    const conn = connection({ accountExports: () => [exportRow("x2", { state: "running", phase: "rows", sizeBytes: 0, cancelRequested: true }), { kind: "page", nextCursor: "" }] });
    const tab = await openExport(conn);
    const list = await within(tab).findByRole("list", { name: "Exports" });
    expect(within(list).getByText("Cancelling")).toBeTruthy();
    expect(within(list).queryByText("Running")).toBeNull();
  });

  it("opens a complete export at Download, reading what its report flags, with Download still on its line", async () => {
    const conn = connection({
      accountExports: () => [exportRow("x1"), { kind: "page", nextCursor: "" }],
      accountExportStatus: () => [exportRow("x1", {
        report: { bytes: { missing: 0, refused: 2, notCarriedObjects: 3, notCarriedBytes: 1024, notCarriedUnknownSize: 1 }, scrubbedFields: [] },
      })],
    });
    const tab = await openExport(conn);
    expect(within(tab).getByRole("button", { name: /^Download the export of / })).toBeTruthy();
    fireEvent.click(within(tab).getByRole("button", { name: /Complete/ }));
    await screen.findByRole("heading", { name: "Export this account" });
    await waitFor(() => expect(callsTo(conn, "accountExportStatus")).toEqual(['builtin accountExportStatus(exportId: "x1")']));
    expect(await screen.findByText("2 references to files outside where their kind keeps them were left out and not read.")).toBeTruthy();
    expect(screen.getByText("3 older or attached files were counted, not carried (1.0 KiB).")).toBeTruthy();
    expect(screen.getByText("1 of them has no known size.")).toBeTruthy();
    expect(within(bar()!).getByRole("button", { name: "Download" })).toBeTruthy();
  });

  it("reads readiness again when the wizard opens, and offers nothing to prepare once the account is not ready", async () => {
    let read = 0;
    const conn = connection({ accountExportReadiness: () => (++read === 1 ? READY : NOT_READY) });
    await openWizard(conn);
    await waitFor(() => expect(callsTo(conn, "accountExportReadiness")).toHaveLength(2));
    expect(await screen.findByText("This account is no longer ready to export.")).toBeTruthy();
    expect(within(bar()!).queryByRole("button", { name: "Continue" })).toBeNull();
    pressInBar("Leave");
    expect(await screen.findByRole("region", { name: "Export" })).toBeTruthy();
    expect(callsTo(conn, "accountExportPrepare")).toHaveLength(0);
  });

  it("leaves a running export running", async () => {
    const running = exportRow("x2", { state: "running", phase: "checking", sizeBytes: 0 });
    const conn = connection({ accountExports: () => [running, { kind: "page", nextCursor: "" }], accountExportStatus: () => [running] });
    const tab = await openExport(conn);
    fireEvent.click(await within(tab).findByRole("button", { name: /Running/ }));
    await waitFor(() => expect(within(bar()!).getByText("Running")).toBeTruthy());
    pressInBar("Leave");
    expect(await screen.findByRole("region", { name: "Export" })).toBeTruthy();
    expect(callsTo(conn, "accountExportCancel")).toHaveLength(0);
  });

  const DRAFT = () => [{ ...exportRow("x9", { state: "draft", sizeBytes: 0, fileName: "" }), passphrase: PASSPHRASE }];

  async function toPassphrase(conn: Conn) {
    await openWizard(conn);
    pressInBar("Continue");
    pressInBar("Continue");
    await waitFor(() => expect(screen.getByText(PASSPHRASE)).toBeTruthy());
  }

  it("says the export must be started within fifteen minutes", async () => {
    await toPassphrase(connection({ accountExportPrepare: DRAFT }));
    expect(screen.getByText(/within fifteen minutes/)).toBeTruthy();
  });

  it("says a draft removed for not being started in time, voids its passphrase, and prepares again", async () => {
    let prepared = 0;
    const conn = connection({
      accountExportPrepare: () => {
        prepared += 1;
        return [{ ...exportRow(`x${prepared}`, { state: "draft", sizeBytes: 0, fileName: "" }), passphrase: prepared === 1 ? PASSPHRASE : "NEWPA-SSPHR-ASEAB-CDEFG-HJK23" }];
      },
      accountExportStart: () => new Error("export_draft_expired: export x1 was removed after fifteen minutes"),
    });
    await toPassphrase(conn);
    fireEvent.click(screen.getByRole("checkbox", { name: "I have saved it" }));
    pressInBar("Start export");
    expect(await screen.findByText("The prepared export was removed because it was not started in time.")).toBeTruthy();
    expect(screen.getByText(/Its passphrase is void/)).toBeTruthy();
    // The void passphrase is gone, and no Try again is offered for it.
    expect(screen.queryByText(PASSPHRASE)).toBeNull();
    expect(within(bar()!).queryByRole("button", { name: "Try again" })).toBeNull();
    expect(within(bar()!).getByRole("button", { name: "Leave" })).toBeTruthy();
    expect(screen.queryByText(/export_draft_expired/)).toBeNull();
    pressInBar("Prepare again");
    expect((screen.getByRole("switch", { name: /Protect the file/ }) as HTMLInputElement).checked).toBe(true);
    pressInBar("Continue");
    await waitFor(() => expect(screen.getByText("NEWPA-SSPHR-ASEAB-CDEFG-HJK23")).toBeTruthy());
    expect(callsTo(conn, "accountExportPrepare")).toHaveLength(2);
  });

  it("can always be left once the draft has expired", async () => {
    const conn = connection({
      accountExportPrepare: DRAFT,
      accountExportStart: () => new Error("export_draft_expired: gone"),
    });
    await toPassphrase(conn);
    fireEvent.click(screen.getByRole("checkbox", { name: "I have saved it" }));
    pressInBar("Start export");
    await screen.findByText("The prepared export was removed because it was not started in time.");
    pressInBar("Leave");
    expect(await screen.findByRole("region", { name: "Export" })).toBeTruthy();
  });

  it("keeps any other refusal of Start in the engine's words, with Try again", async () => {
    let refuse = true;
    const conn = connection({
      accountExportPrepare: DRAFT,
      accountExportStart: () => refuse ? new Error("the export runner is not available on this cluster") : [exportRow("x9", { state: "queued", sizeBytes: 0 })],
      accountExportStatus: () => [exportRow("x9", { state: "running", phase: "checking", sizeBytes: 0 })],
    });
    await toPassphrase(conn);
    fireEvent.click(screen.getByRole("checkbox", { name: "I have saved it" }));
    pressInBar("Start export");
    expect(await screen.findByText("the export runner is not available on this cluster")).toBeTruthy();
    expect(screen.getByText(PASSPHRASE)).toBeTruthy();
    expect(screen.queryByText("The prepared export was removed because it was not started in time.")).toBeNull();
    refuse = false;
    pressInBar("Try again");
    await waitFor(() => expect(within(bar()!).getByText("Running")).toBeTruthy());
  });

  async function reopen(row: Record<string, unknown>, handlers: Partial<Record<Name, Handler>> = {}) {
    const conn = connection({ accountExports: () => [row, { kind: "page", nextCursor: "" }], accountExportStatus: () => [row], ...handlers });
    const tab = await openExport(conn);
    fireEvent.click(await within(tab).findByRole("button", { name: new RegExp(exportStateWordFor(row)) }));
    await screen.findByRole("heading", { name: "Export this account" });
    return conn;
  }
  const exportStateWordFor = (row: Record<string, unknown>) => ({ draft: "Prepared", queued: "Waiting to start", running: "Running" } as Record<string, string>)[String(row["state"])]!;
  const SHOWN_ONCE = "Its passphrase was shown once, when the export was prepared, and is not shown again.";

  it("reopens a protected draft: says its passphrase is not shown again, and starts only once it is said to be saved", async () => {
    const conn = await reopen(exportRow("x5", { state: "draft", sizeBytes: 0 }), {
      accountExportStart: () => [exportRow("x5", { state: "queued", sizeBytes: 0 })],
    });
    expect(screen.getByText(SHOWN_ONCE)).toBeTruthy();
    expect(within(bar()!).queryByRole("button", { name: "Start export" })).toBeNull();
    expect(within(bar()!).getByRole("button", { name: "Cancel" })).toBeTruthy();
    fireEvent.click(screen.getByRole("checkbox", { name: "I have saved the passphrase" }));
    pressInBar("Start export");
    await waitFor(() => expect(callsTo(conn, "accountExportStart")).toEqual(['builtin accountExportStart(exportId: "x5")']));
  });

  it("reopens an unprotected draft with Start offered directly", async () => {
    await reopen(exportRow("x5", { state: "draft", protected: false, sizeBytes: 0 }));
    expect(screen.queryByText(SHOWN_ONCE)).toBeNull();
    expect(screen.queryByRole("checkbox", { name: "I have saved the passphrase" })).toBeNull();
    expect(within(bar()!).getByRole("button", { name: "Start export" })).toBeTruthy();
  });

  it("reopens a protected running export and says its passphrase is not shown again", async () => {
    await reopen(exportRow("x6", { state: "running", phase: "rows", sizeBytes: 0 }));
    expect(screen.getByText(SHOWN_ONCE)).toBeTruthy();
    await waitFor(() => expect(within(bar()!).getByText("Running")).toBeTruthy());
  });

  it("reopens a queued export at its stage, and says its passphrase is not shown again", async () => {
    await reopen(exportRow("x7", { state: "queued", phase: "", sizeBytes: 0 }));
    expect(screen.getByText(SHOWN_ONCE)).toBeTruthy();
    await waitFor(() => expect(within(bar()!).getByText("Waiting to start")).toBeTruthy());
    expect(within(bar()!).getByRole("button", { name: "Leave" })).toBeTruthy();
  });

  it("keeps a refused cancel in words, leaves the run as it was, and can still be left", async () => {
    const running = exportRow("x2", { state: "running", phase: "checking", sizeBytes: 0 });
    const conn = await reopen(running, { accountExportCancel: () => new Error("export_failed: the run has already finished") });
    await waitFor(() => expect(within(bar()!).getByText("Running")).toBeTruthy());
    pressInBar("Cancel");
    pressInBar("Cancel export");
    expect(await screen.findByText("the run has already finished")).toBeTruthy();
    expect(screen.queryByText(/export_failed:/)).toBeNull();
    expect(callsTo(conn, "accountExportCancel")).toHaveLength(1);
    // The question is withdrawn; the way out stands.
    expect(within(bar()!).queryByRole("button", { name: "Cancel export" })).toBeNull();
    pressInBar("Leave");
    expect(await screen.findByRole("region", { name: "Export" })).toBeTruthy();
  });
});
