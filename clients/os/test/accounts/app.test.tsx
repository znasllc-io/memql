import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { betweenTheClicks, readTheBar, useFakeDate } from "../readTheBar";
import { installSeededAccess } from "../seededAccess";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { AccountsApp } = await import("../../src/apps/accounts/AccountsApp");
const { LocalAccountsSettingsStore } = await import("../../src/apps/accounts/settings");
const { SELF_ACCOUNT_ID } = await import("../../src/apps/accounts/rows");
const { accountRow, accountTabNames, fakeConnection, openAccountTab, rowsResult, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

const ACCOUNT_CONCEPT = "v1:accounts:account";

function memoryStore(over: Record<string, unknown> = {}) {
  const bag = new Map<string, string>();
  const store = new LocalAccountsSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
  if (Object.keys(over).length > 0) store.save({ ...store.load(), ...over });
  return store;
}

function mount(connection: Conn, sectionId = "accounts", settings: Record<string, unknown> = {}) {
  h.connection = connection;
  const navigate = vi.fn();
  const view = render(
    withSession(
      <AccountsApp
        sectionId={sectionId}
        navigate={navigate}
        askContext={() => {}}
        store={memoryStore(settings)}
      />,
    ),
  );
  return { view, navigate };
}

describe("the registry list", () => {
  it("renders the clients the cluster returned", async () => {
    const conn = fakeConnection({
      clientAccountsAll: [
        accountRow({ id: "v1:accounts:account:a1", name: "Acme Consulting" }),
        accountRow({ id: "v1:accounts:account:b2", name: "Borden Ltd", domain: "borden.test" }),
      ],
    });
    mount(conn);
    expect(await screen.findByText("Acme Consulting")).toBeTruthy();
    expect(screen.getByText("Borden Ltd")).toBeTruthy();
  });

  it("places the authorized filtered count beside Accounts and updates it on arrival", async () => {
    const conn = fakeConnection({ clientAccountsAll: [
      accountRow({ id: "client-a", name: "Visible client" }),
      accountRow({ id: "client-b", name: "Archived client", status: "archived" }),
    ] });
    mount(conn);
    await screen.findByText("Visible client");
    const heading = screen.getByRole("heading", { name: "Accounts" });
    expect(heading.parentElement?.querySelector(".os-head-meta")?.textContent).toBe("1 shown");
    expect(screen.getByRole("button", { name: /Visible client/ }).classList.contains("os-record-row")).toBe(true);
    await act(async () => conn.subscriptions.emit(ACCOUNT_CONCEPT, accountRow({ id: "client-c", name: "New client" }), "NODE_CREATED"));
    await screen.findByText("New client");
    expect(heading.parentElement?.querySelector(".os-head-meta")?.textContent).toBe("2 shown");
  });

  it("does not invent a zero while the account read is pending or refused", async () => {
    const conn = fakeConnection({});
    let refuse!: (reason: Error) => void;
    conn.query.clientAccountsBrowse.mockImplementation(() => new Promise((_resolve, reject) => { refuse = reject; }));
    const { view } = mount(conn);
    expect(view.container.querySelector(".os-head-meta")).toBeNull();
    await act(async () => refuse(new Error("permission denied")));
    await screen.findByText("This cluster did not return its clients.");
    expect(view.container.querySelector(".os-head-meta")).toBeNull();
  });

  it("filters archives before paging", async () => {
    // The server narrows before the ten-row limit; each filter keeps its own cached pages.
    const conn = fakeConnection({
      clientAccountsAll: [accountRow({ id: "v1:accounts:account:a1", name: "Acme" })],
    });
    mount(conn);
    await screen.findByText("Acme");
    expect(conn.query.clientAccountsBrowse).toHaveBeenCalledWith(
      { includeArchived: false, search: undefined },
      expect.anything(),
    );
  });

  it("excludes archived clients by default and shows them, marked, under the settings preference", async () => {
    // The in-surface checkbox is gone (DESIGN.md rules 4/10): archived
    // visibility is the app-settings preference, like its siblings.
    const seed = {
      clientAccountsAll: [
        accountRow({ id: "v1:accounts:account:a1", name: "Acme" }),
        accountRow({ id: "v1:accounts:account:z9", name: "Zephyr", status: "archived" }),
      ],
    };
    const first = mount(fakeConnection(seed));
    await screen.findByText("Acme");
    expect(screen.queryByText("Zephyr")).toBeNull();
    first.view.unmount();

    mount(fakeConnection(seed), "accounts", { showArchived: true });
    expect(await screen.findByText("Zephyr")).toBeTruthy();
    expect(screen.getByText("archived")).toBeTruthy();
  });

  it("marks the owner's own company", async () => {
    const conn = fakeConnection({
      clientAccountsAll: [accountRow({ id: SELF_ACCOUNT_ID, name: "Our Studio" })],
    });
    mount(conn);
    await screen.findByText("Our Studio");
    expect(screen.getByText("you")).toBeTruthy();
  });
});

describe("the arrival cue", () => {
  it("announces a client created while somebody is watching", async () => {
    const conn = fakeConnection({
      clientAccountsAll: [accountRow({ id: "v1:accounts:account:a1", name: "Acme" })],
    });
    mount(conn);
    await screen.findByText("Acme");

    await act(async () => {
      conn.subscriptions.emit(
        ACCOUNT_CONCEPT,
        accountRow({ id: "v1:accounts:account:new", name: "Newcomer" }),
        "NODE_CREATED",
      );
    });

    expect(await screen.findByText("Newcomer")).toBeTruthy();
    await waitFor(() => expect(screen.getByText("new")).toBeTruthy());
  });

  it("does NOT announce a row whose configuredAt is all that moved", async () => {
    // configuredAt is stamped by every save, so a cue keyed on it would fire
    // twice for one edit. The fingerprint deliberately omits it.
    const conn = fakeConnection({
      clientAccountsAll: [
        accountRow({
          id: "v1:accounts:account:a1",
          name: "Acme",
          configuredAt: "2026-08-01T00:00:00Z",
        }),
      ],
    });
    mount(conn);
    await screen.findByText("Acme");

    await act(async () => {
      conn.subscriptions.emit(
        ACCOUNT_CONCEPT,
        accountRow({
          id: "v1:accounts:account:a1",
          name: "Acme",
          configuredAt: "2026-09-01T12:00:00Z",
        }),
      );
    });

    const row = screen.getByText("Acme").closest("li");
    expect(row?.getAttribute("data-arrival")).toBeNull();
  });
});

describe("the first-run card (D7)", () => {
  it("stands in for the app when the self row carries no configuredAt", async () => {
    const conn = fakeConnection({
      clientAccountsAll: [accountRow({ id: SELF_ACCOUNT_ID, name: "My company", configuredAt: "" })],
    });
    mount(conn);
    expect(await screen.findByText("This instance is yours.")).toBeTruthy();
    // The ordinary surface is NOT rendered underneath it.
    expect(screen.queryByRole("button", { name: "Add a client" })).toBeNull();
  });

  it("does not render while the feed is still seeding", async () => {
    // An unconfigured self row and a feed that has not arrived look identical
    // from the gate -- both are "no matching row" -- and guessing wrong shows
    // a setup form to somebody whose company was named months ago.
    let release: (() => void) | null = null;
    const conn = fakeConnection();
    conn.query.clientAccountsAll = vi.fn(
      () =>
        new Promise((resolve) => {
          release = () => resolve({ rows: () => [] } as never);
        }),
    );
    mount(conn);
    expect(screen.queryByText("This instance is yours.")).toBeNull();
    await act(async () => {
      release?.();
    });
  });

  it("yields to the ordinary surface once configuredAt is stamped", async () => {
    const conn = fakeConnection({
      clientAccountsAll: [accountRow({ id: SELF_ACCOUNT_ID, configuredAt: "2026-09-01T00:00:00Z" })],
    });
    mount(conn);
    await screen.findByText("Acme Consulting");
    expect(screen.queryByText("This instance is yours.")).toBeNull();
  });

  it("saves through updateClientAccount, which is what stamps the field", async () => {
    const conn = fakeConnection({
      clientAccountsAll: [accountRow({ id: SELF_ACCOUNT_ID, name: "My company", configuredAt: "" })],
    });
    mount(conn);
    await screen.findByText("This instance is yours.");

    fireEvent.change(screen.getByLabelText("Your company's name"), {
      target: { value: "Our Studio" },
    });
    fireEvent.click(screen.getByText("Save and continue"));

    await waitFor(() =>
      expect(conn.query.updateClientAccount).toHaveBeenCalledWith(
        expect.objectContaining({ accountId: SELF_ACCOUNT_ID, name: "Our Studio" }),
      ),
    );
  });

  it("refuses to save with no name, and says so", async () => {
    const conn = fakeConnection({
      clientAccountsAll: [accountRow({ id: SELF_ACCOUNT_ID, name: "My company", configuredAt: "" })],
    });
    mount(conn);
    await screen.findByText("This instance is yours.");
    expect(screen.getByText("Save and continue").closest("button")?.disabled).toBe(true);
    expect(screen.getByText("A name is the one thing this needs.")).toBeTruthy();
  });
});

describe("the People tab", () => {
  async function openDetail(conn: Conn) {
    mount(conn);
    fireEvent.click(await screen.findByText("Acme Consulting"));
    return openAccountTab("People");
  }

  it("names the guests, which the inventory cannot", async () => {
    const conn = fakeConnection({
      clientAccountsAll: [accountRow({ id: "a1" })],
      invitationsForAccount: [{ id: "i1", inviteeEmail: "guest@acme.com" }],
    });
    const ledger = await openDetail(conn);
    await waitFor(() => expect(within(ledger).getByText("guest@acme.com")).toBeTruthy());
  });

  it("A REFUSAL IS NOT A ZERO -- the gated band says so verbatim", async () => {
    const conn = fakeConnection({
      clientAccountsAll: [accountRow({ id: "a1" })],
      invitationsForAccount: new Error("reading invitations is owner and admin only"),
    });
    const ledger = await openDetail(conn);
    await waitFor(() =>
      expect(within(ledger).getByText("reading invitations is owner and admin only")).toBeTruthy(),
    );
    expect(within(ledger).getAllByText("Not yours to read").length).toBeGreaterThan(0);
  });

  it("re-reads on demand and says when it read", async () => {
    const conn = fakeConnection({ clientAccountsAll: [accountRow({ id: "a1" })] });
    const people = await openDetail(conn);
    await waitFor(() => expect(within(people).getByText(/^Read at /)).toBeTruthy());
    expect(conn.query.groupsForAccount).toHaveBeenCalledTimes(1);
    fireEvent.click(within(people).getByRole("button", { name: "Re-read" }));
    await waitFor(() => expect(conn.query.groupsForAccount).toHaveBeenCalledTimes(2));
    expect(conn.query.invitationsForAccount).toHaveBeenCalledTimes(2);
  });

  it("no longer reads the four retired bands", async () => {
    const conn = fakeConnection({ clientAccountsAll: [accountRow({ id: "a1" })] });
    await openDetail(conn);
    const query = conn.query as Record<string, unknown>;
    for (const name of ["sitesForAccount", "libraryItemsForAccount", "domainsForAccount", "campaignsForAccount"]) {
      expect(query[name]).toBeUndefined();
    }
  });
});

// THE ACCOUNT PAGE (design record 2026-11-12-account-export, section K): the
// Fleet machine page's shape, LocalTabs under the heading and one subject per
// tab. Each case below asks a tab for its own content and checks that the
// content of the tab beside it is not on screen with it.
describe("the account page", () => {
  async function openAcme(conn: Conn = fakeConnection({
    clientAccountsAll: [
      accountRow({ id: "a1", domainStatus: "verified", domainVerifiedAt: "2026-09-01T10:00:00Z" }),
      accountRow({ id: "b2", name: "Borden Ltd", domain: "borden.test", createdAt: "2026-07-01T00:00:00Z" }),
    ],
    invitationsForAccount: [{ id: "i1", inviteeEmail: "guest@acme.com" }],
  })) {
    mount(conn);
    fireEvent.click(await screen.findByText("Acme Consulting"));
    await screen.findByRole("navigation", { name: "Account views" });
    return conn;
  }

  it("offers Details, People, Data and Settings, in that order, and opens on Details", async () => {
    await openAcme();
    expect(accountTabNames()).toEqual(["Details", "People", "Data", "Settings"]);
    const tabs = screen.getByRole("navigation", { name: "Account views" });
    expect(within(tabs).getByRole("button", { name: "Details" }).getAttribute("aria-current")).toBe("page");
    expect(screen.getByRole("region", { name: "Details" })).toBeTruthy();
  });

  it("reads the trail as Accounts, the account, then the tab", async () => {
    await openAcme();
    const crumbs = () => within(screen.getByRole("navigation", { name: "Breadcrumbs" })).getAllByRole("listitem").map((li) => li.textContent);
    expect(crumbs()).toEqual(["Accounts", "Acme Consulting", "Details"]);
    await openAccountTab("People");
    expect(crumbs()).toEqual(["Accounts", "Acme Consulting", "People"]);
  });

  it("Details holds the facts, read-only, and nothing another tab holds", async () => {
    await openAcme();
    const details = screen.getByRole("region", { name: "Details" });
    for (const section of ["Profile", "Domain", "Front door", "Status"]) {
      expect(within(details).getByRole("region", { name: section })).toBeTruthy();
    }
    expect(within(details).getByText("dana@acme.com")).toBeTruthy();
    expect(within(details).getByText("Proven")).toBeTruthy();
    // Quiet copy on what is worth pasting, never on a status.
    expect(within(details).getByRole("button", { name: "Copy Email" })).toBeTruthy();
    expect(within(details).getByRole("button", { name: "Copy Account id" })).toBeTruthy();
    expect(within(details).queryByRole("button", { name: "Copy Status" })).toBeNull();
    // No edit, and no other tab's subject.
    expect(within(details).queryByRole("textbox")).toBeNull();
    expect(within(details).queryByText("Archive this client")).toBeNull();
    expect(screen.queryByText("guest@acme.com")).toBeNull();
    expect(screen.queryByRole("region", { name: "Inventory" })).toBeNull();
  });

  it("People holds the People and Guests bands, and nothing another tab holds", async () => {
    await openAcme();
    const people = await openAccountTab("People");
    expect(await within(people).findByText("guest@acme.com")).toBeTruthy();
    expect(within(people).getAllByText("Guests").length).toBeGreaterThan(0);
    expect(screen.queryByRole("region", { name: "Details" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Inventory" })).toBeNull();
    expect(screen.queryByText("Archive this client")).toBeNull();
  });

  it("Data holds the inventory, and nothing another tab holds", async () => {
    await openAcme();
    const data = await openAccountTab("Data");
    expect(within(data).getByRole("region", { name: "Inventory" })).toBeTruthy();
    expect(screen.queryByText("guest@acme.com")).toBeNull();
    expect(screen.queryByRole("region", { name: "Profile" })).toBeNull();
    expect(screen.queryByText("Archive this client")).toBeNull();
  });

  it("Settings holds every edit, and nothing another tab holds", async () => {
    await openAcme();
    const settings = await openAccountTab("Settings");
    expect(within(settings).getByRole("textbox", { name: "Name" })).toBeTruthy();
    expect(within(settings).getByRole("region", { name: /The domain of/ })).toBeTruthy();
    expect(within(settings).getByRole("button", { name: "Archive this client" })).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Details" })).toBeNull();
    expect(screen.queryByText("guest@acme.com")).toBeNull();
    expect(screen.queryByRole("region", { name: "Inventory" })).toBeNull();
  });

  it("saves a profile edit from Settings through updateClientAccount", async () => {
    const conn = await openAcme();
    const settings = await openAccountTab("Settings");
    const save = within(settings).getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    fireEvent.change(within(settings).getByRole("textbox", { name: "Name" }), { target: { value: "Acme Group" } });
    expect(save.disabled).toBe(false);
    fireEvent.click(save);
    await waitFor(() =>
      expect(conn.query.updateClientAccount).toHaveBeenCalledWith(expect.objectContaining({ accountId: "a1", name: "Acme Group" })),
    );
  });

  it("keeps a refused profile edit as typed, with the refusal beside it", async () => {
    const conn = fakeConnection({ clientAccountsAll: [accountRow({ id: "a1" })] });
    conn.query.updateClientAccount.mockRejectedValue(new Error("a client named Acme Group already exists"));
    await openAcme(conn);
    const settings = await openAccountTab("Settings");
    fireEvent.change(within(settings).getByRole("textbox", { name: "Name" }), { target: { value: "Acme Group" } });
    fireEvent.click(within(settings).getByRole("button", { name: "Save" }));
    const profile = within(settings).getByRole("region", { name: "Profile" });
    expect(await within(profile).findByText("a client named Acme Group already exists")).toBeTruthy();
    expect((within(profile).getByRole("textbox", { name: "Name" }) as HTMLInputElement).value).toBe("Acme Group");
    fireEvent.click(within(profile).getByRole("button", { name: "Discard changes" }));
    expect((within(profile).getByRole("textbox", { name: "Name" }) as HTMLInputElement).value).toBe("Acme Consulting");
    expect(within(profile).queryByText("a client named Acme Group already exists")).toBeNull();
  });

  it("keeps the selected tab when the same account re-renders, and opens another account on Details", async () => {
    await openAcme();
    await openAccountTab("Settings");
    await act(async () => conn0().subscriptions.emit(ACCOUNT_CONCEPT, accountRow({ id: "a1", notes: "Renewal in March" })));
    const tabs = screen.getByRole("navigation", { name: "Account views" });
    expect(within(tabs).getByRole("button", { name: "Settings" }).getAttribute("aria-current")).toBe("page");
    expect(screen.getByRole("region", { name: "Settings" })).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Back to Accounts" }));
    fireEvent.click(await screen.findByText("Borden Ltd"));
    const next = await screen.findByRole("navigation", { name: "Account views" });
    expect(within(next).getByRole("button", { name: "Details" }).getAttribute("aria-current")).toBe("page");
    const profile = within(screen.getByRole("region", { name: "Details" })).getByRole("region", { name: "Profile" });
    expect(within(profile).getByText("Borden Ltd")).toBeTruthy();
  });
});

/** The connection the last mount was handed. */
const conn0 = () => h.connection as Conn;

/** The calls the page made to one builtin, by name. */
function builtinCalls(conn: Conn, name: string): string[] {
  return conn.query.executeNamed.mock.calls.filter((c) => c[0] === name).map((c) => String(c[1]));
}

/** Hold one builtin's answer until the test releases it. */
function holdBuiltin(conn: Conn, name: string) {
  const original = conn.query.executeNamed.getMockImplementation()!;
  let release: (rows: Record<string, unknown>[]) => void = () => {};
  conn.query.executeNamed.mockImplementation(async (n: string, call: string) => {
    if (n !== name) return original(n, call);
    return new Promise((resolve) => {
      release = (rows) => resolve(rowsResult(rows as never));
    });
  });
  return (rows: Record<string, unknown>[]) => act(async () => release(rows));
}

const POPULATED = [
  { kind: "concept", concept: "v1:campaigns:campaign", namespace: "campaigns", name: "campaign", rows: 1, versions: 3 },
  { kind: "concept", concept: "v1:commerce:order", namespace: "commerce", name: "order", rows: 1, versions: 1 },
  { kind: "concept", concept: "v1:library:artifact", namespace: "library", name: "artifact", rows: 2, versions: 2 },
  { kind: "concept", concept: "v1:library:file", namespace: "library", name: "file", rows: 2, versions: 5 },
  { kind: "concept", concept: "v1:shopify:product", namespace: "shopify", name: "product", rows: 4, versions: 4 },
  { kind: "concept", concept: "v1:zebra:stripe", namespace: "zebra", name: "stripe", rows: 7, versions: 7 },
  { kind: "files", bytes: 2048 },
];

describe("the inventory", () => {
  afterEach(() => installSeededAccess("owner"));

  async function openInventory(conn: Conn) {
    mount(conn);
    fireEvent.click(await screen.findByText("Acme Consulting"));
    const data = await openAccountTab("Data");
    return within(data).findByRole("region", { name: "Inventory" });
  }

  it("reads the account by its id, through the one builtin", async () => {
    const conn = fakeConnection({ clientAccountsAll: [accountRow({ id: "a1" })], accountInventory: POPULATED });
    await openInventory(conn);
    await waitFor(() => expect(builtinCalls(conn, "accountInventory")).toEqual(['builtin accountInventory(accountId: "a1")']));
  });

  it("draws the shape of a list while it reads", async () => {
    const conn = fakeConnection({ clientAccountsAll: [accountRow({ id: "a1" })] });
    const release = holdBuiltin(conn, "accountInventory");
    const inventory = await openInventory(conn);
    expect(within(inventory).getByText("Loading inventory")).toBeTruthy();
    expect(within(inventory).queryByText("Nothing belongs to this account yet")).toBeNull();
    await release([]);
    expect(await within(inventory).findByText("Nothing belongs to this account yet")).toBeTruthy();
  });

  it("says so when the account holds nothing", async () => {
    const conn = fakeConnection({ clientAccountsAll: [accountRow({ id: "a1" })], accountInventory: [{ kind: "files", bytes: 0 }] });
    const inventory = await openInventory(conn);
    expect(await within(inventory).findByText("Nothing belongs to this account yet")).toBeTruthy();
  });

  it("groups concepts into areas with their totals, and Files with its bytes", async () => {
    const conn = fakeConnection({ clientAccountsAll: [accountRow({ id: "a1" })], accountInventory: POPULATED });
    const inventory = await openInventory(conn);
    const list = await within(inventory).findByRole("list", { name: "Inventory by area" });
    const names = within(list).getAllByRole("button").map((b) => b.getAttribute("aria-label"));
    // Ordered by area; shopify and commerce are one Store; an unknown
    // namespace shows under its own name.
    expect(names).toEqual(["Email, 1 row", "Files, 4 rows, 2.0 KiB", "Store, 5 rows", "Zebra, 7 rows"]);
  });

  it("opens an area to list its concepts", async () => {
    const conn = fakeConnection({ clientAccountsAll: [accountRow({ id: "a1" })], accountInventory: POPULATED });
    const inventory = await openInventory(conn);
    const store = await within(inventory).findByRole("button", { name: "Store, 5 rows" });
    expect(within(inventory).queryByText("product")).toBeNull();
    fireEvent.click(store);
    expect(store.getAttribute("aria-expanded")).toBe("true");
    expect(within(inventory).getByText("product")).toBeTruthy();
    expect(within(inventory).getByText("4 rows")).toBeTruthy();
    expect(within(inventory).getByText("order")).toBeTruthy();
    fireEvent.click(store);
    expect(within(inventory).queryByText("product")).toBeNull();
  });

  it("shows a refusal in the server's words, and retries in place", async () => {
    const sentence = "This account holds too many rows to count in one read. Try again; a narrower read is coming with export.";
    const conn = fakeConnection({ clientAccountsAll: [accountRow({ id: "a1" })], accountInventory: new Error(sentence) });
    const inventory = await openInventory(conn);
    expect(await within(inventory).findByText(sentence)).toBeTruthy();
    expect(within(inventory).queryByText("Nothing belongs to this account yet")).toBeNull();
    const original = conn.query.executeNamed.getMockImplementation()!;
    conn.query.executeNamed.mockImplementation(async (n: string, call: string) =>
      n === "accountInventory" ? rowsResult(POPULATED as never) : original(n, call));
    fireEvent.click(within(inventory).getByRole("button", { name: "Try again" }));
    expect(await within(inventory).findByRole("button", { name: "Store, 5 rows" })).toBeTruthy();
    expect(within(inventory).queryByText(sentence)).toBeNull();
  });

  it("re-reads on demand and says when it read", async () => {
    const conn = fakeConnection({ clientAccountsAll: [accountRow({ id: "a1" })], accountInventory: POPULATED });
    const inventory = await openInventory(conn);
    await waitFor(() => expect(within(inventory).getByText(/^Read at /)).toBeTruthy());
    expect(builtinCalls(conn, "accountInventory")).toHaveLength(1);
    fireEvent.click(within(inventory).getByRole("button", { name: "Re-read" }));
    await waitFor(() => expect(builtinCalls(conn, "accountInventory")).toHaveLength(2));
  });

  it("is absent, with its tab, and never asked for, without the inventory part", async () => {
    installSeededAccess("admin");
    const conn = fakeConnection({ clientAccountsAll: [accountRow({ id: "a1" })], accountInventory: POPULATED });
    mount(conn);
    fireEvent.click(await screen.findByText("Acme Consulting"));
    // The People tab still stands; Data, which would hold nothing, is not drawn.
    const people = await openAccountTab("People");
    expect(within(people).getAllByText("People").length).toBeGreaterThan(0);
    expect(accountTabNames()).toEqual(["Details", "People", "Settings"]);
    expect(screen.queryByRole("region", { name: "Inventory" })).toBeNull();
    expect(builtinCalls(conn, "accountInventory")).toHaveLength(0);
  });
});

describe("existing data", () => {
  afterEach(() => installSeededAccess("owner"));

  const ACCOUNTS = [
    accountRow({ id: SELF_ACCOUNT_ID, name: "Our Studio", createdAt: "2026-08-02T00:00:00Z" }),
    accountRow({ id: "a1" }),
  ];

  async function openPrimary(status: Record<string, unknown>[]) {
    const conn = fakeConnection({ clientAccountsAll: ACCOUNTS, accountAssignmentStatus: status });
    mount(conn);
    fireEvent.click(await screen.findByText("Our Studio"));
    const data = await openAccountTab("Data");
    return { conn, panel: await within(data).findByRole("region", { name: "Existing data" }) };
  }

  const progress = (concept: string, assigned: number, finished: boolean) => ({
    kind: "progress", concept, assigned, kept: 0, shared: 0, conflicts: 0,
    passStartedAt: "2026-11-12T09:00:00Z", passFinishedAt: finished ? "2026-11-12T09:05:00Z" : "", verifiedThrough: "",
  });

  it("says the switch is held, and shows no totals", async () => {
    const { panel } = await openPrimary([{ kind: "switch", enabled: false, held: true }]);
    expect(await within(panel).findByText("Recording accounts on rows is held on this cluster.")).toBeTruthy();
    expect(within(panel).queryByText("Totals are not available right now.")).toBeNull();
  });

  it("says it is waiting for every node", async () => {
    const { panel } = await openPrimary([{ kind: "switch", enabled: false, held: false }]);
    expect(await within(panel).findByText(/^Waiting for every node/)).toBeTruthy();
    expect(within(panel).queryByText("Totals are not available right now.")).toBeNull();
  });

  it("counts finished concepts and shows the totals while it runs", async () => {
    const { panel } = await openPrimary([
      { kind: "switch", enabled: true, held: false },
      progress("v1:notes:note", 10, true),
      progress("v1:todos:todo", 5, false),
      { kind: "account", accountId: "a1", rows: 15 },
    ]);
    expect(await within(panel).findByText("Assigning existing rows to accounts.")).toBeTruthy();
    expect(within(panel).getByText("1 of 2")).toBeTruthy();
    // The per-concept counts start again with every pass, so they are not
    // summed into a figure; the per-account total is the one shown.
    expect(within(panel).queryByText("15")).toBeNull();
    expect(within(panel).getByText("15 rows")).toBeTruthy();
  });

  it("lists rows per account, by name, once finished", async () => {
    const { panel } = await openPrimary([
      { kind: "switch", enabled: true, held: false },
      progress("v1:notes:note", 12, true),
      { kind: "account", accountId: SELF_ACCOUNT_ID, rows: 12 },
      { kind: "account", accountId: "a1", rows: 3 },
    ]);
    expect(await within(panel).findByText("Existing rows have been assigned.")).toBeTruthy();
    expect(within(panel).getByText("Our Studio")).toBeTruthy();
    expect(within(panel).getByText("12 rows")).toBeTruthy();
    expect(within(panel).getByText("Acme Consulting")).toBeTruthy();
    expect(within(panel).getByText("3 rows")).toBeTruthy();
    expect(within(panel).queryByText("Needs a decision")).toBeNull();
  });

  it("finished with no account rows: the state, and that totals are not available", async () => {
    const { panel } = await openPrimary([
      { kind: "switch", enabled: true, held: false },
      progress("v1:notes:note", 12, true),
    ]);
    expect(await within(panel).findByText("Existing rows have been assigned.")).toBeTruthy();
    expect(within(panel).getByText("Totals are not available right now.")).toBeTruthy();
    expect(within(panel).queryByRole("alert")).toBeNull();
  });

  it("lists the rows that need a decision", async () => {
    const { panel } = await openPrimary([
      { kind: "switch", enabled: true, held: false },
      progress("v1:library:artifact", 4, true),
      { kind: "conflict", concept: "v1:library:artifact", rowId: "art-7", conflict: "several_accounts", detail: "Tagged for two accounts", foundAt: "2026-11-12T09:01:00Z" },
    ]);
    expect(await within(panel).findByText("Needs a decision")).toBeTruthy();
    expect(within(panel).getByText("art-7")).toBeTruthy();
    expect(within(panel).getByText("Tagged for two accounts")).toBeTruthy();
  });

  it("is absent on any account but the primary one", async () => {
    const conn = fakeConnection({ clientAccountsAll: ACCOUNTS, accountAssignmentStatus: [{ kind: "switch", enabled: true, held: false }] });
    mount(conn);
    fireEvent.click(await screen.findByText("Acme Consulting"));
    const data = await openAccountTab("Data");
    // The control: the tab is open and holds the inventory.
    expect(await within(data).findByRole("region", { name: "Inventory" })).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Existing data" })).toBeNull();
    expect(builtinCalls(conn, "accountAssignmentStatus")).toHaveLength(0);
  });

  it("is absent on the primary account without the inventory part", async () => {
    installSeededAccess("admin");
    const conn = fakeConnection({ clientAccountsAll: ACCOUNTS, accountAssignmentStatus: [{ kind: "switch", enabled: true, held: false }] });
    mount(conn);
    fireEvent.click(await screen.findByText("Our Studio"));
    await openAccountTab("People");
    expect(accountTabNames()).not.toContain("Data");
    expect(screen.queryByRole("region", { name: "Existing data" })).toBeNull();
    expect(builtinCalls(conn, "accountAssignmentStatus")).toHaveLength(0);
  });
});

describe("archiving (D8)", () => {
  it("asks in surface, never through a browser dialog", async () => {
    const conn = fakeConnection({
      clientAccountsAll: [accountRow({ id: "v1:accounts:account:a1" })],
    });
    const confirmSpy = vi.spyOn(globalThis, "confirm" as never);
    mount(conn);
    fireEvent.click(await screen.findByText("Acme Consulting"));
    await openAccountTab("Settings");
    fireEvent.click(await screen.findByText("Archive this client"));

    expect(await screen.findByText("Archive Acme Consulting?")).toBeTruthy();
    expect(confirmSpy).not.toHaveBeenCalled();

    // The confirm is not armed until somebody says so. Queried by ROLE
    // because "Archive" is also the panel's heading -- a getByText would
    // match both and pass or fail for the wrong reason.
    const confirmButton = screen.getByRole("button", { name: "Archive" });
    expect((confirmButton as HTMLButtonElement).disabled).toBe(true);
    await readTheBar();
    fireEvent.click(screen.getByLabelText("I want to file this client away", { selector: "input" }));
    fireEvent.click(confirmButton);

    await waitFor(() =>
      expect(conn.query.archiveClientAccount).toHaveBeenCalledWith({
        accountId: "v1:accounts:account:a1",
      }),
    );
  });

  it("is not offered on a client already archived", async () => {
    const conn = fakeConnection({
      clientAccountsAll: [accountRow({ id: "v1:accounts:account:a1", status: "archived" })],
    });
    mount(conn, "accounts", { showArchived: true });
    fireEvent.click(await screen.findByText("Acme Consulting"));
    const settings = await openAccountTab("Settings");
    // The control: the Settings tab is open and holds the profile.
    expect(within(settings).getByRole("region", { name: "Profile" })).toBeTruthy();
    expect(screen.queryByText("Archive this client")).toBeNull();
  });
});

// The question opens where "Archive this client" stood, outside any bar
// (memql#6033): the second click of a double-click on that button used to land
// in it -- on the acknowledgement, which is what arms the confirmation.
describe("archiving, one double-click later", () => {
  useFakeDate();

  async function ask() {
    const conn = fakeConnection({
      clientAccountsAll: [accountRow({ id: "v1:accounts:account:a1" })],
    });
    mount(conn);
    fireEvent.click(await screen.findByText("Acme Consulting"));
    await openAccountTab("Settings");
    fireEvent.click(await screen.findByText("Archive this client"));
    await screen.findByText("Archive Acme Consulting?");
    return conn;
  }
  const acknowledgement = () =>
    screen.getByLabelText("I want to file this client away", { selector: "input" }) as HTMLInputElement;

  it("does not arm the confirmation on the second click of the click that asked, and does once it is read", async () => {
    const conn = await ask();
    betweenTheClicks();
    fireEvent.click(acknowledgement());
    expect(acknowledgement().checked).toBe(false);
    expect((screen.getByRole("button", { name: "Archive" }) as HTMLButtonElement).disabled).toBe(true);

    await readTheBar();
    fireEvent.click(acknowledgement());
    expect(acknowledgement().checked).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Archive" }));
    await waitFor(() =>
      expect(conn.query.archiveClientAccount).toHaveBeenCalledWith({ accountId: "v1:accounts:account:a1" }),
    );
  });

  it("does not withdraw the question on that click either", async () => {
    const conn = await ask();
    betweenTheClicks();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByText("Archive Acme Consulting?")).toBeTruthy();
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByText("Archive Acme Consulting?")).toBeNull();
    expect(conn.query.archiveClientAccount).not.toHaveBeenCalled();
  });
});

describe("writes", () => {
  it("adds a client and inserts NOTHING locally", async () => {
    const conn = fakeConnection({ clientAccountsAll: [] });
    mount(conn);
    fireEvent.click(await screen.findByRole("button", { name: "Add a client" }));
    fireEvent.change(screen.getByLabelText("Client name"), { target: { value: "Newcomer" } });
    for (let i = 0; i < 3; i++) { await readTheBar(); fireEvent.click(screen.getByRole("button", { name: "Next" })); }
    await readTheBar();
    fireEvent.click(screen.getByText("Add client"));

    await waitFor(() =>
      expect(conn.query.createClientAccount).toHaveBeenCalledWith(
        expect.objectContaining({ name: "Newcomer" }),
      ),
    );
    // The row arrives on its own broadcast, with the cue, exactly like one
    // somebody else created. A local insert would put a row on screen the
    // cluster had not confirmed.
    expect(screen.queryByText("Newcomer")).toBeNull();
  });

  it("omits blank fields rather than writing empty strings over stored values", async () => {
    const conn = fakeConnection({ clientAccountsAll: [] });
    mount(conn);
    fireEvent.click(await screen.findByRole("button", { name: "Add a client" }));
    fireEvent.change(screen.getByLabelText("Client name"), { target: { value: "Newcomer" } });
    for (let i = 0; i < 3; i++) { await readTheBar(); fireEvent.click(screen.getByRole("button", { name: "Next" })); }
    await readTheBar();
    fireEvent.click(screen.getByText("Add client"));

    await waitFor(() => expect(conn.query.createClientAccount).toHaveBeenCalled());
    const args = conn.query.createClientAccount.mock.calls[0]?.[0] ?? {};
    expect(args.domain).toBeUndefined();
    expect(args.notes).toBeUndefined();
  });

  it("renders a server refusal in surface, verbatim", async () => {
    const conn = fakeConnection({ clientAccountsAll: [] });
    conn.query.createClientAccount = vi.fn(async () => {
      throw new Error("a client named Newcomer already exists");
    });
    mount(conn);
    fireEvent.click(await screen.findByRole("button", { name: "Add a client" }));
    fireEvent.change(screen.getByLabelText("Client name"), { target: { value: "Newcomer" } });
    for (let i = 0; i < 3; i++) { await readTheBar(); fireEvent.click(screen.getByRole("button", { name: "Next" })); }
    await readTheBar();
    fireEvent.click(screen.getByText("Add client"));

    expect(await screen.findByText("a client named Newcomer already exists")).toBeTruthy();
    const first = conn.query.createClientAccount.mock.calls[0]?.[0] as Record<string, unknown>;
    await readTheBar(); fireEvent.click(screen.getByRole("button", { name: "Add client" }));
    await waitFor(() => expect(conn.query.createClientAccount).toHaveBeenCalledTimes(2));
    expect((conn.query.createClientAccount.mock.calls[1]?.[0] as Record<string, unknown>).accountId).toBe(first.accountId);
  });
});
