import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { AccountsApp } = await import("../../src/apps/accounts/AccountsApp");
const { LocalAccountsSettingsStore } = await import("../../src/apps/accounts/settings");
const { SELF_ACCOUNT_ID } = await import("../../src/apps/accounts/rows");
const { accountRow, fakeConnection, withSession } = await import("./harness");

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

  it("SEEDS UNFILTERED and folds the archive filter client-side", async () => {
    // Seeding filtered would make the toggle re-run the read and re-baseline
    // every arrival cue, so revealing rows the browser already had would
    // announce them as new. The seed therefore asks for everything.
    const conn = fakeConnection({
      clientAccountsAll: [accountRow({ id: "v1:accounts:account:a1", name: "Acme" })],
    });
    mount(conn);
    await screen.findByText("Acme");
    expect(conn.query.clientAccountsAll).toHaveBeenCalledWith(
      { includeArchived: true },
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
    expect(screen.queryByText("Add a client")).toBeNull();
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

describe("the ledger", () => {
  async function openDetail(conn: Conn) {
    mount(conn);
    fireEvent.click(await screen.findByText("Acme Consulting"));
    return screen.findByLabelText("What belongs to Acme Consulting");
  }

  it("counts each of the four populations", async () => {
    const conn = fakeConnection({
      clientAccountsAll: [accountRow({ id: "v1:accounts:account:a1" })],
      sitesForAccount: [{ id: "s1", hostname: "acme.example.com" }, { id: "s2", hostname: "b.example.com" }],
      libraryItemsForAccount: [{ id: "f1", title: "Contract.pdf" }],
      domainsForAccount: [],
      invitationsForAccount: [{ id: "i1", inviteeEmail: "guest@acme.com" }],
    });
    const ledger = await openDetail(conn);
    await waitFor(() => expect(within(ledger).getByText("2")).toBeTruthy());
    expect(within(ledger).getByText("acme.example.com")).toBeTruthy();
    expect(within(ledger).getByText("Contract.pdf")).toBeTruthy();
    expect(within(ledger).getByText("No domains yet")).toBeTruthy();
  });

  it("A REFUSAL IS NOT A ZERO -- the gated band says so verbatim", async () => {
    // The guest-invitation rollup carries `requiresOwnerOrAdmin`, so below
    // that floor the engine refuses. Rendering it as "0 invitations" would be
    // this window inventing a fact about a client.
    const conn = fakeConnection({
      clientAccountsAll: [accountRow({ id: "v1:accounts:account:a1" })],
      invitationsForAccount: new Error("reading invitations is owner and admin only"),
    });
    const ledger = await openDetail(conn);
    await waitFor(() => expect(within(ledger).getByText("Not yours to read")).toBeTruthy());
    expect(within(ledger).getByText("reading invitations is owner and admin only")).toBeTruthy();
  });

  it("one band refusing does not take the other three down with it", async () => {
    const conn = fakeConnection({
      clientAccountsAll: [accountRow({ id: "v1:accounts:account:a1" })],
      sitesForAccount: [{ id: "s1", hostname: "acme.example.com" }],
      invitationsForAccount: new Error("refused"),
    });
    const ledger = await openDetail(conn);
    await waitFor(() => expect(within(ledger).getByText("acme.example.com")).toBeTruthy());
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
    fireEvent.click(await screen.findByText("Archive this client"));

    expect(await screen.findByText("Archive Acme Consulting?")).toBeTruthy();
    expect(confirmSpy).not.toHaveBeenCalled();

    // The confirm is not armed until somebody says so. Queried by ROLE
    // because "Archive" is also the panel's heading -- a getByText would
    // match both and pass or fail for the wrong reason.
    const confirmButton = screen.getByRole("button", { name: "Archive" });
    expect((confirmButton as HTMLButtonElement).disabled).toBe(true);
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
    await screen.findByLabelText("What belongs to Acme Consulting");
    expect(screen.queryByText("Archive this client")).toBeNull();
  });
});

describe("writes", () => {
  it("adds a client and inserts NOTHING locally", async () => {
    const conn = fakeConnection({ clientAccountsAll: [] });
    mount(conn);
    fireEvent.click(await screen.findByText("Add a client"));
    fireEvent.change(screen.getByLabelText("Client name"), { target: { value: "Newcomer" } });
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
    fireEvent.click(await screen.findByText("Add a client"));
    fireEvent.change(screen.getByLabelText("Client name"), { target: { value: "Newcomer" } });
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
    fireEvent.click(await screen.findByText("Add a client"));
    fireEvent.change(screen.getByLabelText("Client name"), { target: { value: "Newcomer" } });
    fireEvent.click(screen.getByText("Add client"));

    expect(await screen.findByText("a client named Newcomer already exists")).toBeTruthy();
  });
});
