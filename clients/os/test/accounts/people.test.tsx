import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown, opened: [] as unknown[] }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

// THE SHELL, replaced by a spy on the one act this band performs. The band's
// whole handoff is `openApp("users", "groups", { groupId })`, and the assertion
// worth making is that it dispatches THAT -- not that a window appeared.
vi.mock("../../src/chrome/state", async () => {
  const real = await vi.importActual<typeof import("../../src/chrome/state")>(
    "../../src/chrome/state",
  );
  return {
    ...real,
    useOsIfPresent: () => ({
      actions: {
        openApp: (appId: string, sectionId: string, payload?: Record<string, unknown>) =>
          h.opened.push({ appId, sectionId, payload }),
      },
    }),
  };
});

const { AccountsApp } = await import("../../src/apps/accounts/AccountsApp");
const { LocalAccountsSettingsStore } = await import("../../src/apps/accounts/settings");
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
  h.opened = [];
  render(
    withSession(
      <AccountsApp sectionId="accounts" navigate={vi.fn()} askContext={() => {}} store={memoryStore()} />,
    ),
  );
  const row = await screen.findByText("Acme Consulting");
  await act(async () => {
    fireEvent.click(row.closest("button") as HTMLElement);
  });
  return await screen.findByRole("region", { name: /What belongs to/ });
}

function peopleBand(ledger: HTMLElement): HTMLElement {
  const title = within(ledger).getByText("People");
  const band = title.closest("article");
  if (band === null) throw new Error("no People band");
  return band as HTMLElement;
}

// ===========================================================================
// THE PEOPLE BAND
// ===========================================================================
// It is FIRST among the bands because it is the one the other five are about:
// the people are who the deployables, the files, the knowledge and the
// campaigns are for.

describe("the People band", () => {
  it("counts DISTINCT people across the client's groups", async () => {
    // Somebody in two of a client's groups is one person. Summing memberships
    // would report "3 in 2 groups" for two people, which is a number this
    // window invented.
    const ledger = await openDetail(
      fakeConnection({
        clientAccountsAll: [accountRow({ id: "a1" })],
        groupsForAccount: [
          { id: "g1", name: "Acme" },
          { id: "g2", name: "Acme leads" },
        ],
        membersOfGroup: {
          g1: [
            { id: "m1", groupId: "g1", userId: "u1", status: "active" },
            { id: "m2", groupId: "g1", userId: "u2", status: "active" },
          ],
          g2: [{ id: "m3", groupId: "g2", userId: "u1", status: "active" }],
        },
      }),
    );
    const band = peopleBand(ledger);
    await waitFor(() => expect(within(band).getByText("2")).toBeTruthy());
    expect(within(band).getByText("in 2 groups")).toBeTruthy();
    expect(within(band).getByText("Acme leads")).toBeTruthy();
  });

  it("does not count a membership somebody has left", async () => {
    const ledger = await openDetail(
      fakeConnection({
        clientAccountsAll: [accountRow({ id: "a1" })],
        groupsForAccount: [{ id: "g1", name: "Acme" }],
        membersOfGroup: {
          g1: [
            { id: "m1", groupId: "g1", userId: "u1", status: "active" },
            { id: "m2", groupId: "g1", userId: "u2", status: "removed" },
          ],
        },
      }),
    );
    const band = peopleBand(ledger);
    await waitFor(() => expect(within(band).getByText("1")).toBeTruthy());
  });

  it("renders a refusal in the server's own words rather than a zero", async () => {
    // The reads carry `@requiresRank("admin")`. Rendering a refusal as "0
    // people" would be this window inventing a fact about a client -- the
    // ledger's standing rule.
    const ledger = await openDetail(
      fakeConnection({
        clientAccountsAll: [accountRow({ id: "a1" })],
        groupsForAccount: new Error("reading groups is admin and above"),
      }),
    );
    const band = peopleBand(ledger);
    await waitFor(() => expect(within(band).getByText("Not yours to read")).toBeTruthy());
    expect(within(band).getByText("reading groups is admin and above")).toBeTruthy();
    expect(within(band).queryByText("0")).toBeNull();
  });

  it("hands off to Users on the group, by intent", async () => {
    const ledger = await openDetail(
      fakeConnection({
        clientAccountsAll: [accountRow({ id: "a1" })],
        groupsForAccount: [{ id: "g1", name: "Acme" }],
        membersOfGroup: { g1: [{ id: "m1", groupId: "g1", userId: "u1", status: "active" }] },
      }),
    );
    const band = peopleBand(ledger);
    await waitFor(() => expect(within(band).getByRole("button", { name: "Acme" })).toBeTruthy());
    await act(async () => {
      fireEvent.click(within(band).getByRole("button", { name: "Acme" }));
    });
    expect(h.opened).toEqual([
      { appId: "users", sectionId: "groups", payload: { groupId: "g1" } },
    ]);
  });

  it("points at Users when the client has no group yet", async () => {
    const ledger = await openDetail(
      fakeConnection({ clientAccountsAll: [accountRow({ id: "a1" })], groupsForAccount: [] }),
    );
    const band = peopleBand(ledger);
    await waitFor(() => expect(within(band).getByText("in no groups yet")).toBeTruthy());
    expect(within(band).getByText("Users is where these are added.")).toBeTruthy();
  });
});
