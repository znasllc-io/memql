import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({
  connection: null as unknown,
  mint: vi.fn(),
  revoke: vi.fn(),
}));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

vi.mock("@znasllc-io/memql-sdk-core/identity", () => ({
  mintAccountToken: (...args: unknown[]) => h.mint(...args),
  revokeAccountToken: (...args: unknown[]) => h.revoke(...args),
}));

const { AccountsApp } = await import("../../src/apps/accounts/AccountsApp");
const { ACCOUNTS_SECTIONS, LocalAccountsSettingsStore } = await import(
  "../../src/apps/accounts/settings"
);
const { accountRow, accountTokenRow, billingAccountRow, fakeConnection, withSession } =
  await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

// The Credentials section (memql#5013).
//
// ===========================================================================
// THE DEFECT THESE TESTS PIN
// ===========================================================================
// `v1:identity:account` and `v1:accounts:account` are different concepts that
// share the word "account", with no field and no reference between them. A
// credential is minted against the FIRST -- `mintAccountToken` gates on
// `query accountById`, which binds it -- and the OS Accounts list is the
// SECOND. A credentials panel hung on a client row was therefore refused on
// every mint, and the refusal reads as a permission problem rather than as the
// wrong concept.
//
// A test that seeds only one of the two reads cannot see that mistake at all:
// whichever read a surface makes, it gets the fixture it was going to get. So
// EVERY test below seeds BOTH, with different names and different ids, and the
// assertions name which one had to come back. That is what makes them a
// regression test rather than a description of current behaviour.

/** A client-registry row -- the concept the Credentials section must NOT read. */
const CLIENT_ID = "v1:accounts:account:a1";
const CLIENT_NAME = "Acme Consulting";

/** A billing account -- the concept it MUST read. */
const BILLING_ID = "v1:identity:account:b1";
const BILLING_NAME = "Northwind Trading";

function memoryStore() {
  const bag = new Map<string, string>();
  return new LocalAccountsSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
}

function mount(connection: Conn, sectionId = "credentials") {
  h.connection = connection;
  return render(
    withSession(
      <AccountsApp
        sectionId={sectionId}
        navigate={vi.fn()}
        askContext={() => {}}
        store={memoryStore()}
      />,
    ),
  );
}

/**
 * A connection answering BOTH account reads, with rows that cannot be confused.
 *
 * The client and the billing account carry different names and different ids
 * on purpose: an assertion that one is on screen and the other is not is the
 * only kind that can tell the two concepts apart.
 */
function bothConcepts(over: Partial<Parameters<typeof fakeConnection>[0]> = {}) {
  return fakeConnection({
    clientAccountsAll: [accountRow({ id: CLIENT_ID, name: CLIENT_NAME })],
    accounts: [billingAccountRow({ id: BILLING_ID, name: BILLING_NAME })],
    ...over,
  });
}

beforeEach(() => {
  h.connection = null;
  h.mint.mockReset();
  h.revoke.mockReset();
  h.mint.mockResolvedValue({
    success: true,
    plainToken: ["mql", "acct", "notARealCredentialOnlyATestFixture"].join("_"),
    identityId: "v1:identity:identity:tok1",
    accountId: BILLING_ID,
    subjectUserId: "v1:identity:user:me",
    auditEventId: "v1:identity:auditEvent:ae1",
    errorCode: "",
    errorMessage: "",
  });
  globalThis.localStorage.clear();
  globalThis.sessionStorage.clear();
});

describe("which concept the surface is about", () => {
  it("LISTS THE BILLING ACCOUNTS `accounts` RETURNED, and never the client registry", async () => {
    // THE REGRESSION. Both reads are seeded; only one of them may be on
    // screen. If this section ever reads `clientAccountsAll` again it renders
    // "Acme Consulting" and every mint beneath it is refused with zero rows.
    const conn = bothConcepts();
    mount(conn);

    expect(await screen.findByText(BILLING_NAME)).toBeTruthy();
    expect(screen.queryByText(CLIENT_NAME)).toBeNull();
    await waitFor(() => expect(conn.query.accounts).toHaveBeenCalled());
  });

  it("MINTS AGAINST THE BILLING ACCOUNT'S ID, which is what the engine gates on", async () => {
    // `mintAccountToken` runs `query accountById` as the caller, and that
    // query binds `v1:identity:account`. A client-registry id resolves ZERO
    // ROWS, and zero rows IS the refusal -- so the id that reaches the mint is
    // the whole of this fix.
    const conn = bothConcepts();
    mount(conn);
    fireEvent.click(await screen.findByText(BILLING_NAME));

    const panel = await screen.findByLabelText("Credentials");
    // The read under the page asks about the billing account too.
    await waitFor(() =>
      expect(conn.query.accountTokensForAccount).toHaveBeenCalledWith(
        { accountId: BILLING_ID },
        expect.anything(),
      ),
    );

    fireEvent.change(within(panel).getByLabelText("What is this credential for"), {
      target: { value: "Nightly export job" },
    });
    fireEvent.click(within(panel).getByRole("button", { name: "Issue a credential" }));

    await waitFor(() => expect(h.mint).toHaveBeenCalledTimes(1));
    expect(h.mint.mock.calls[0]?.[1]).toEqual({
      accountId: BILLING_ID,
      label: "Nightly export job",
    });
    expect(String(h.mint.mock.calls[0]?.[1]?.accountId)).toContain("v1:identity:account");
  });

  it("SAYS WHICH ACCOUNTS THESE ARE, once, at the head", async () => {
    // The whole defect is two concepts sharing a word, so the surface has to
    // name which one it means -- and ONCE. A caveat repeated per row is a
    // caveat nobody reads, and it would imply the distinction is per-account
    // when it is per-concept.
    const conn = bothConcepts({
      accounts: [
        billingAccountRow({ id: BILLING_ID, name: BILLING_NAME }),
        billingAccountRow({ id: "v1:identity:account:b2", name: "Halberd Freight" }),
      ],
    });
    mount(conn);
    await screen.findByText(BILLING_NAME);

    const said = screen.getAllByText(/not the clients listed under Accounts/);
    expect(said.length).toBe(1);
    expect(said[0]?.textContent).toContain("billing accounts");
    // And it names the concept, which is the one thing a reader can check the
    // claim against.
    expect(screen.getByText("v1:identity:account")).toBeTruthy();
  });

  it("THE CLIENT DETAIL NO LONGER CARRIES THE CREDENTIALS PANEL", async () => {
    // The placement this fixed. A panel here is refused on every mint, so its
    // absence is the fix -- and it is asserted with a POSITIVE CONTROL that
    // the detail actually opened, or "no panel" would pass on a detail that
    // failed to render for some unrelated reason.
    const conn = bothConcepts();
    mount(conn, "accounts");
    fireEvent.click(await screen.findByText(CLIENT_NAME));

    // The control: the detail is open.
    expect(await screen.findByLabelText("Profile")).toBeTruthy();

    expect(screen.queryByLabelText("Credentials")).toBeNull();
    expect(screen.queryByRole("button", { name: "Issue a credential" })).toBeNull();
    // Nothing even ASKED for a client's credentials.
    expect(conn.query.accountTokensForAccount).not.toHaveBeenCalled();
  });
});

describe("the list", () => {
  it("renders every billing account the read returned", async () => {
    const conn = bothConcepts({
      accounts: [
        billingAccountRow({ id: BILLING_ID, name: BILLING_NAME }),
        billingAccountRow({ id: "v1:identity:account:b2", name: "Halberd Freight" }),
      ],
    });
    mount(conn);

    expect(await screen.findByText(BILLING_NAME)).toBeTruthy();
    expect(screen.getByText("Halberd Freight")).toBeTruthy();
    expect(within(screen.getByLabelText("Billing accounts")).getAllByRole("button").length).toBe(2);
  });

  it("SAYS SO WHEN THERE ARE NONE, rather than rendering nothing", async () => {
    // An empty region is indistinguishable from a region that failed to
    // render, and this one has a second reading a reader needs: the read
    // SUCCEEDED and came back with none.
    const conn = bothConcepts({ accounts: [] });
    mount(conn);

    expect(
      await screen.findByText(/You have no billing accounts/),
    ).toBeTruthy();
    // And it says why the clients next door are not a substitute.
    expect(screen.getByText(/cannot stand in for one/)).toBeTruthy();
  });

  it("A REFUSAL IS NOT AN EMPTY LIST", async () => {
    // "You have none" and "the cluster would not tell you" are different
    // answers, and rendering the second as the first is this window inventing
    // a fact about what somebody owns.
    const conn = bothConcepts({ accounts: new Error("reading accounts is not permitted here") });
    mount(conn);

    expect(
      await screen.findByText("This cluster did not return your billing accounts."),
    ).toBeTruthy();
    expect(screen.getByText("reading accounts is not permitted here")).toBeTruthy();
    expect(screen.queryByText(/You have no billing accounts/)).toBeNull();
  });

  it("ASKS FOR EVERY STATUS, and keeps a closed account reachable", async () => {
    // `accounts` takes an optional `status` and it is deliberately not passed:
    // a closed billing account's credentials still exist and still work until
    // they are revoked, so narrowing the read would hide the one surface that
    // can revoke them.
    const conn = bothConcepts({
      accounts: [
        billingAccountRow({
          id: BILLING_ID,
          name: BILLING_NAME,
          status: "archived",
          archivedAt: "2026-09-02T00:00:00Z",
        }),
      ],
    });
    mount(conn);

    await screen.findByText(BILLING_NAME);
    expect(conn.query.accounts).toHaveBeenCalledWith({}, expect.anything());
    expect(screen.getByText("archived")).toBeTruthy();

    // Still openable, and the page says why it is still here.
    fireEvent.click(screen.getByText(BILLING_NAME));
    expect(await screen.findByLabelText("Credentials")).toBeTruthy();
    expect(screen.getByText(/still work until they are revoked/)).toBeTruthy();
  });

  it("NEVER RENDERS THE TWO `@pii` FIELDS", async () => {
    // `primaryContactName` and `primaryContactEmail` are `@pii` on the concept
    // -- personal data about a third party -- and a credentials surface has no
    // use for either. They are absent from the PROJECTION, so this holds
    // through the page as well as the list.
    const conn = bothConcepts({
      accounts: [
        billingAccountRow({
          id: BILLING_ID,
          name: BILLING_NAME,
          primaryContactName: "Wren Alderman",
          primaryContactEmail: "wren@northwind.test",
        }),
      ],
    });
    mount(conn);
    fireEvent.click(await screen.findByText(BILLING_NAME));
    await screen.findByLabelText("Credentials");

    // THE REACHABLE POSITIVE: a non-pii field from the same row IS on screen,
    // so the negatives below are measuring a rendered row rather than an empty
    // document.
    expect(screen.getByText("CRM-4471")).toBeTruthy();

    expect(document.body.innerHTML).not.toContain("Wren Alderman");
    expect(document.body.innerHTML).not.toContain("wren@northwind.test");
  });
});

describe("DESIGN.md rule 11 -- a list and its detail never share a scroll column", () => {
  it("renders ONE Head in the list view and ONE in the page that replaces it", async () => {
    // Two Heads in one scroller is the tell that neither shape happened. This
    // surface takes the back-Head shape (the page REPLACES the list) because
    // the detail is tall: an issue form, a one-time reveal carrying four facts
    // and two controls, a refusal notice, and a per-row revoke confirm.
    const conn = bothConcepts({
      accountTokensForAccount: [accountTokenRow({ id: "v1:identity:identity:tok1" })],
    });
    const { container } = mount(conn);

    await screen.findByText(BILLING_NAME);
    expect(container.querySelectorAll(".os-head").length).toBe(1);
    expect(screen.getByText("Credentials")).toBeTruthy();

    fireEvent.click(screen.getByText(BILLING_NAME));
    await screen.findByLabelText("Credentials");

    // Still one: the page took the list's place rather than appending to it.
    expect(container.querySelectorAll(".os-head").length).toBe(1);
    // And the list is GONE, not scrolled past.
    expect(screen.queryByLabelText("Billing accounts")).toBeNull();
  });

  it("goes back to the list from the page's own Head", async () => {
    const conn = bothConcepts();
    const { container } = mount(conn);

    fireEvent.click(await screen.findByText(BILLING_NAME));
    await screen.findByLabelText("Credentials");

    fireEvent.click(screen.getByRole("button", { name: /Billing accounts/ }));

    expect(await screen.findByLabelText("Billing accounts")).toBeTruthy();
    expect(screen.queryByLabelText("Credentials")).toBeNull();
    expect(container.querySelectorAll(".os-head").length).toBe(1);
  });
});

describe("the manifest", () => {
  it("declares the section between Accounts and Logs, with no floor of its own", () => {
    // The manifest and the settings picker read this ONE array, so adding to
    // it is the whole wiring. No role floor: `v1:identity:account` is the
    // plain owned tier and the engine already answers "which of these are
    // yours" -- a floor written here would be presentation pretending to be
    // authorization.
    expect(ACCOUNTS_SECTIONS.map((s) => s.id)).toEqual([
      "accounts",
      "credentials",
      "logs",
      "settings",
    ]);
    const section = ACCOUNTS_SECTIONS.find((s) => s.id === "credentials");
    expect(section?.name).toBe("Credentials");
    expect(section?.roles).toBeUndefined();
  });
});
