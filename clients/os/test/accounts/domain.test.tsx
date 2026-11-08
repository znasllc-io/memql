import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { AccountsApp } = await import("../../src/apps/accounts/AccountsApp");
const { LocalAccountsSettingsStore } = await import("../../src/apps/accounts/settings");
const { accountFingerprint, accountFromRow } = await import("../../src/apps/accounts/rows");
const { accountRow, fakeConnection, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

function memoryStore() {
  const bag = new Map<string, string>();
  return new LocalAccountsSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
}

async function openDetail(connection: Conn): Promise<HTMLElement> {
  h.connection = connection;
  render(
    withSession(
      <AccountsApp sectionId="accounts" navigate={vi.fn()} askContext={() => {}} store={memoryStore()} />,
    ),
  );
  const row = await screen.findByText("Acme Consulting");
  await act(async () => {
    fireEvent.click(row.closest("button") as HTMLElement);
  });
  return await screen.findByRole("region", { name: /The domain of/ });
}

/** One stop of the rail, by the name a person reads. */
function stop(rail: HTMLElement, name: string): HTMLElement {
  const label = within(rail).getByText(name);
  const li = label.closest("li");
  if (li === null) throw new Error(`no stop called ${name}`);
  return li as HTMLElement;
}

/**
 * Open a stop and hand back its `<li>`.
 *
 * ONE STOP IS OPEN AT A TIME on this rail -- it is a record, and four open
 * bodies would be four panels with a line down them -- so a test that asks
 * about a closed stop's body is asking about a node the browser has not been
 * asked to render. The same reasoning `test/selectControl.ts` states for the
 * kit's listbox.
 */
async function openStop(rail: HTMLElement, name: string): Promise<HTMLElement> {
  const li = stop(rail, name);
  // The stop's OWN disclosure line, not whatever buttons its body holds: an
  // open stop's body carries three Copy controls, and `queryByRole("button")`
  // would find four.
  const line = li.querySelector(".os-rail-line") as HTMLElement | null;
  if (line !== null && line.getAttribute("aria-expanded") !== "true") {
    await act(async () => {
      fireEvent.click(line);
    });
  }
  return stop(rail, name);
}

// ===========================================================================
// FOUR STOPS, FROM FOUR ROW STATES
// ===========================================================================
// The rail is a rail because the stops depend on each other: there is a
// domain, ownership of it is proven, joining can THEN be turned on, and a
// MemQL name can then be reserved. Four panels would draw the same fields and
// say nothing about that order.

describe("the domain rail", () => {
  it("draws the ownership record as three copyable parts", async () => {
    // A person is pasting these into three different fields of a registrar's
    // form; one blob of text is one they have to split by hand.
    const rail = await openDetail(
      fakeConnection({ clientAccountsAll: [accountRow({ id: "a1", domain: "acme.com" })] }),
    );
    const ownership = await openStop(rail, "Ownership");
    expect(within(ownership).getByText("TXT")).toBeTruthy();
    expect(within(ownership).getByText("_memql-verify.acme.com")).toBeTruthy();
    expect(within(ownership).getByText("memql-verify-example-not-a-real-token")).toBeTruthy();
    expect(within(ownership).getByRole("button", { name: /Copy Value/ })).toBeTruthy();
  });

  it("says there is nothing to press, because the walk is on its own schedule", async () => {
    const rail = await openDetail(
      fakeConnection({ clientAccountsAll: [accountRow({ id: "a1", domain: "acme.com" })] }),
    );
    expect(
      within(rail).getByText(/Checked every two minutes on its own; there is nothing to press/),
    ).toBeTruthy();
    expect(within(rail).queryByRole("button", { name: /Check/ })).toBeNull();
  });

  it("reads the typed failure reason and what the lookup actually saw", async () => {
    const rail = await openDetail(
      fakeConnection({
        clientAccountsAll: [
          accountRow({
            id: "a1",
            domain: "acme.com",
            domainStatus: "verifying",
            domainFailureReason: "dns_token_missing",
            domainFailureDetail: "the zone answered with no TXT records",
            domainLastCheckedAt: "2026-09-07T10:00:00Z",
          }),
        ],
      }),
    );
    const ownership = await openStop(rail, "Ownership");
    expect(within(ownership).getByText("The record is not there yet.")).toBeTruthy();
    expect(within(ownership).getByText("the zone answered with no TXT records")).toBeTruthy();
  });

  it("offers NO joining control before ownership is proven", async () => {
    // The engine refuses `domain_not_verified`, and a checkbox that can only
    // fail is one somebody has to read past to learn it is not for them.
    const rail = await openDetail(
      fakeConnection({ clientAccountsAll: [accountRow({ id: "a1", domain: "acme.com" })] }),
    );
    const joining = await openStop(rail, "Joining");
    expect(within(joining).getByText("Prove ownership first.")).toBeTruthy();
    expect(within(joining).queryByRole("checkbox")).toBeNull();
  });

  it("offers it once ownership is proven, and writes through updateClientAccount", async () => {
    const conn = fakeConnection({
      clientAccountsAll: [
        accountRow({
          id: "a1",
          domain: "acme.com",
          domainStatus: "verified",
          domainVerifiedAt: "2026-09-05T00:00:00Z",
        }),
      ],
    });
    const rail = await openDetail(conn);
    const joining = await openStop(rail, "Joining");
    const check = within(joining).getByRole("checkbox");
    await act(async () => {
      fireEvent.click(check);
    });
    await waitFor(() =>
      expect(conn.query.updateClientAccount).toHaveBeenCalledWith({
        accountId: "a1",
        joinOnDomain: true,
      }),
    );
  });

  it("names the domain_not_verified refusal at the Joining stop", async () => {
    // The engine's validation reads the MERGED payload, so flipping the flag
    // on a row whose STORED status is not verified is refused by code -- which
    // is what happens when somebody changes the domain and turns joining on in
    // the same sitting. A generic failure would leave them with a checkbox
    // that will not stay on and nothing to act on.
    const conn = fakeConnection({
      clientAccountsAll: [
        accountRow({ id: "a1", domain: "acme.com", domainStatus: "verified" }),
      ],
    });
    conn.query.updateClientAccount.mockRejectedValue(
      new Error("domain_not_verified: joinOnDomain may only be set once domainStatus is verified"),
    );
    const rail = await openDetail(conn);
    const joining = await openStop(rail, "Joining");
    await act(async () => {
      fireEvent.click(within(joining).getByRole("checkbox"));
    });
    expect(await screen.findByText("This domain is not proven yet.")).toBeTruthy();
    expect(
      screen.getByText(/joinOnDomain may only be set once domainStatus is verified/),
    ).toBeTruthy();
  });

  it("reads the self account's stops as the cluster's own", async () => {
    const rail = await openDetail(
      fakeConnection({
        clientAccountsAll: [
          accountRow({ id: "self", name: "Acme Consulting", domain: "acme.com", memqlDomain: "memql.acme.com" }),
        ],
      }),
    );
    expect(within(await openStop(rail, "Ownership")).getByText(/nothing has to be proven/)).toBeTruthy();
    expect(
      within(await openStop(rail, "MemQL address")).getByText("This cluster's own."),
    ).toBeTruthy();
  });

  // The stop used to say "Recorded now; served when the per-account front door
  // lands". Epic memql#5168 landed it, so the stop now says what the door is
  // actually doing -- and with no door row yet, that is the records to create.
  // HELD WITH NO DOOR IS ITS OWN STATE. The sweep opens the door on its own
  // schedule, so for up to one pass a held name has no row -- and the first
  // version of this stop rendered that as "Not held", which is the exact
  // conflation the reservation reason exists to end, reintroduced one layer up.
  it("says a held name is reserved and waiting, not unheld, before its door exists", async () => {
    const rail = await openDetail(
      fakeConnection({
        clientAccountsAll: [
          accountRow({
            id: "a1",
            domain: "acme.com",
            domainStatus: "verified",
            memqlDomain: "memql.acme.com",
            memqlReservedAt: "2026-09-06T00:00:00Z",
          }),
        ],
      }),
    );
    const memql = await openStop(rail, "MemQL address");
    expect(within(memql).getByText("Reserved")).toBeTruthy();
    expect(within(memql).getByText(/Held for this client/)).toBeTruthy();
    // And NOT the unheld sentence, which would be false about this account.
    expect(within(memql).queryByText("Not held")).toBeNull();
  });

  // The other half of the same split: a name the cluster has NOT agreed to
  // serve says why, from the typed reason rather than by inference.
  it("says why an unheld name is unheld", async () => {
    const rail = await openDetail(
      fakeConnection({
        clientAccountsAll: [
          accountRow({
            id: "a1",
            domain: "acme.com",
            domainStatus: "unverified",
            memqlDomain: "memql.acme.com",
            memqlReservedAt: "",
            memqlReservationReason: "ownership_unproven",
          }),
        ],
      }),
    );
    const memql = await openStop(rail, "MemQL address");
    expect(within(memql).getByText("Not held")).toBeTruthy();
    expect(within(memql).getByText(/not verified yet/)).toBeTruthy();
  });

  // A LIVE DOOR SHOWS ADDRESSES, NOT RECORDS. Once all three point here the
  // "create this CNAME" instruction is finished work, and the hosts stop being
  // a list of records to make and become a list of things to open.
  it("shows the three hosts as addresses once the door is serving", async () => {
    const rail = await openDetail(
      fakeConnection({
        clientAccountsAll: [
          accountRow({
            id: "a1",
            domain: "acme.com",
            domainStatus: "verified",
            memqlDomain: "memql.acme.com",
            memqlReservedAt: "2026-09-06T00:00:00Z",
          }),
        ],
        accountFrontDoorsOpen: [
          {
            id: "door-1",
            accountId: "a1",
            reservedName: "memql.acme.com",
            status: "live",
            issuedAt: "2026-09-08T09:00:00Z",
            hostChecks: {
              app: { ok: true },
              api: { ok: true },
              id: { ok: true },
            },
          } as unknown as Row,
        ],
      }),
    );
    const memql = await openStop(rail, "MemQL address");
    expect(within(memql).getByText("Serving")).toBeTruthy();
    for (const host of ["app.memql.acme.com", "api.memql.acme.com", "id.memql.acme.com"]) {
      expect(within(memql).getByText(host)).toBeTruthy();
    }
    // The guidance is GONE: a served door has no records left to create.
    expect(within(memql).queryByText("CNAME")).toBeNull();
  });
});

// ===========================================================================
// THE FIELD THAT MOVES ON A TIMER
// ===========================================================================

describe("the arrival fingerprint", () => {
  it("excludes domainLastCheckedAt", () => {
    // The walk restamps it on a timer (every thirty minutes while a check
    // keeps failing the same way), so a digest built over it would change for
    // nothing a person did -- and the account list would ring on a timer,
    // which is the standing badge the cue exists not to be.
    // Projected through the real `accountFromRow`, so the fingerprint is taken
    // over the same shape the list holds rather than over a literal a test
    // wrote.
    const before = accountFingerprint(
      accountFromRow(accountRow({ id: "a1", domainLastCheckedAt: "2026-09-07T10:00:00Z" })),
    );
    const after = accountFingerprint(
      accountFromRow(accountRow({ id: "a1", domainLastCheckedAt: "2026-09-07T10:02:00Z" })),
    );
    expect(before).toBe(after);
  });

  it("does fire on the walk's real answers", () => {
    // `domainStatus` and `joinOnDomain` are what a person would call a change:
    // a domain that became proven, and joining that was switched on.
    const unproven = accountFingerprint(
      accountFromRow(accountRow({ id: "a1", domainStatus: "unverified" })),
    );
    const proven = accountFingerprint(
      accountFromRow(accountRow({ id: "a1", domainStatus: "verified" })),
    );
    expect(unproven).not.toBe(proven);
  });
});
