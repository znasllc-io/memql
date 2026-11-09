import { act, cleanup, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { ProfileAccess } from "../../src/modules/profile/access";
import { chooseOption, openSelect, selectedLabel } from "../selectControl";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { clientScopeFor, useClientScope } = await import("../../src/apps/accounts/clientScope");
const { ScopeControl } = await import("../../src/apps/accounts/ScopeControl");
const { accountFromRow } = await import("../../src/apps/accounts/rows");
const { accountRow, fakeConnection, rowsResult, withSession } = await import("./harness");

afterEach(cleanup);

// ===========================================================================
// THE CLIENT A STATISTICS PAGE READS UNDER (spec D13)
// ===========================================================================
// The control appears only when there is a choice. Staff choose among every
// client; somebody in more than one organization chooses among theirs; a
// member of one organization never sees it, because a control with one option
// is furniture and a control listing other clients would disclose them. The
// read narrows on the server either way (D11): this is presentation, and a
// stored choice that is not among the options reads everything in scope.

function account(id: string, name: string, status = "active") {
  return accountFromRow(accountRow({ id, name, status }));
}

function access(over: Partial<ProfileAccess>): ProfileAccess {
  return { userId: "v1:identity:user:me", primaryEmail: "me@example.com", role: "user", roleName: "", rank: 0, ...over };
}

const ACCOUNTS = [account("acct-b", "Borden Ltd"), account("acct-a", "Acme"), account("acct-z", "Archived Client", "archived"), account("self", "Operator")];
const STAFF = access({ role: "developer", everyAccount: true });
const GROUP = { id: "g-c", name: "Client Three members", kind: "account", accountId: "acct-c", accountName: "Client Three" };

function connectionWith(accounts: ReturnType<typeof accountRow>[]) {
  h.connection = fakeConnection({ clientAccountsAll: accounts });
}

/** A page's scope and its control, with the choice it stores. */
function Page({ stored, onStore }: { stored: string; onStore: (value: string) => void }) {
  const scope = useClientScope(stored, onStore);
  return <div data-testid="page"><ScopeControl id="scope" scope={scope} /></div>;
}

function inSession(children: ReactNode, session: Parameters<typeof withSession>[1]) {
  return withSession(children, session);
}

/** A connection whose account list read has not answered yet, and the way to answer it. */
function pendingAccountList() {
  let answer: (rows: ReturnType<typeof accountRow>[]) => void = () => {};
  const connection = fakeConnection({});
  connection.query.clientAccountsAll = vi.fn(() => new Promise<ReturnType<typeof rowsResult>>((resolve) => {
    answer = (rows) => resolve(rowsResult(rows));
  }));
  h.connection = connection;
  return { answer: (rows: ReturnType<typeof accountRow>[]) => answer(rows) };
}

describe("the client scope", () => {
  it("staff choose among every client", async () => {
    const scope = clientScopeFor(STAFF, ACCOUNTS, "");
    expect(scope.showControl).toBe(true);
    // All, then every active organization by name; a filed-away one is not a choice.
    expect(scope.options).toEqual([
      { value: "", label: "All clients" },
      { value: "acct-a", label: "Acme" },
      { value: "acct-b", label: "Borden Ltd" },
      { value: "self", label: "Operator" },
    ]);
    expect(scope.value).toBe("");
    expect(scope.accountIds).toEqual([]);
    expect(clientScopeFor(STAFF, ACCOUNTS, "acct-b")).toMatchObject({ value: "acct-b", accountIds: ["acct-b"] });

    // The same, drawn: the control lists them and stores the one chosen.
    connectionWith([accountRow({ id: "acct-b", name: "Borden Ltd" }), accountRow({ id: "acct-a", name: "Acme" })]);
    const onStore = vi.fn();
    render(inSession(<Page stored="" onStore={onStore} />, { role: "developer", everyAccount: true }));
    const trigger = await screen.findByLabelText("Client");
    expect(selectedLabel(trigger)).toBe("All clients");
    await waitFor(() => expect(within(openSelect(trigger)).getAllByRole("option").map((option) => option.textContent)).toEqual(["All clients", "Acme", "Borden Ltd"]));
    chooseOption(trigger, "Borden Ltd");
    expect(onStore).toHaveBeenCalledWith("acct-b");
  });

  it("a member of two chooses only theirs", () => {
    const member = access({ accountIds: ["acct-a", "acct-c"], groups: [GROUP] });
    const scope = clientScopeFor(member, ACCOUNTS, "");
    expect(scope.showControl).toBe(true);
    // Exactly their two: Borden Ltd is readable here and still not offered.
    // The third is named by the group that grants it, since no row names it.
    expect(scope.options).toEqual([
      { value: "", label: "All my clients" },
      { value: "acct-a", label: "Acme" },
      { value: "acct-c", label: "Client Three" },
    ]);
    expect(clientScopeFor(member, ACCOUNTS, "acct-c")).toMatchObject({ value: "acct-c", accountIds: ["acct-c"] });
    // An organization nothing names is named as the Accounts app names an
    // unnamed account, which keeps its id in sight.
    expect(clientScopeFor(access({ accountIds: ["acct-a", "acct-q"] }), ACCOUNTS, "").options).toEqual([
      { value: "", label: "All my clients" },
      { value: "acct-a", label: "Acme" },
      { value: "acct-q", label: "Untitled account (acct-q)" },
    ]);
  });

  it("names an unnamed organization the same way for staff and for members", () => {
    const unnamed = account("acct-n", "");
    const forStaff = clientScopeFor(STAFF, [unnamed], "").options.find((option) => option.value === "acct-n");
    const forMember = clientScopeFor(access({ accountIds: ["acct-n", "acct-a"] }), [unnamed], "").options.find((option) => option.value === "acct-n");
    expect(forStaff?.label).toBe("Untitled account (acct-n)");
    expect(forMember?.label).toBe(forStaff?.label);
    // A group that names it names it for both.
    const named = { ...GROUP, accountId: "acct-n", accountName: "Named by its group" };
    expect(clientScopeFor({ ...STAFF, groups: [named] }, [unnamed], "").options.find((option) => option.value === "acct-n")?.label).toBe("Named by its group");
    expect(clientScopeFor(access({ accountIds: ["acct-n", "acct-a"], groups: [named] }), [unnamed], "").options.find((option) => option.value === "acct-n")?.label).toBe("Named by its group");
  });

  it("a member of two sees only theirs in the control, and no other client's name", async () => {
    connectionWith([accountRow({ id: "acct-a", name: "Acme" }), accountRow({ id: "acct-b", name: "Borden Ltd" })]);
    render(inSession(<Page stored="" onStore={() => {}} />, { role: "user", everyAccount: false, accountIds: ["acct-a", "acct-c"], groups: [GROUP] }));
    const trigger = await screen.findByLabelText("Client");
    expect(selectedLabel(trigger)).toBe("All my clients");
    await waitFor(() => expect(within(openSelect(trigger)).getAllByRole("option").map((option) => option.textContent)).toEqual(["All my clients", "Acme", "Client Three"]));
    expect(document.body.textContent).not.toContain("Borden Ltd");
  });

  it("a member of one sees no control", async () => {
    expect(clientScopeFor(access({ accountIds: ["acct-a"] }), ACCOUNTS, "acct-a")).toEqual({ showControl: false, value: "", options: [], accountIds: [] });
    // Nobody signed in, and somebody in no organization, read the same way.
    expect(clientScopeFor(null, ACCOUNTS, "")).toEqual({ showControl: false, value: "", options: [], accountIds: [] });
    expect(clientScopeFor(access({}), ACCOUNTS, "")).toEqual({ showControl: false, value: "", options: [], accountIds: [] });

    connectionWith([accountRow({ id: "acct-a", name: "Acme" }), accountRow({ id: "acct-b", name: "Borden Ltd" })]);
    render(inSession(<Page stored="acct-b" onStore={() => {}} />, { role: "user", everyAccount: false, accountIds: ["acct-a"], groups: [{ ...GROUP, accountId: "acct-a", accountName: "Acme" }] }));
    // Give any read the page would make a chance to land, then look.
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
    expect(screen.getByTestId("page").childElementCount).toBe(0);
    expect(screen.queryByLabelText("Client")).toBeNull();
    expect(document.body.textContent).not.toContain("Borden Ltd");
  });

  it("a stored choice outside the options falls back to all", () => {
    const member = access({ accountIds: ["acct-a", "acct-c"], groups: [GROUP] });
    // Another client's id, remembered from a different session or typed in.
    expect(clientScopeFor(member, ACCOUNTS, "acct-b")).toMatchObject({ value: "", accountIds: [] });
    // An organization since filed away, or gone.
    expect(clientScopeFor(STAFF, ACCOUNTS, "acct-z")).toMatchObject({ value: "", accountIds: [] });
    expect(clientScopeFor(STAFF, ACCOUNTS, "acct-gone")).toMatchObject({ value: "", accountIds: [] });
  });

  it("stores only a choice it offers", async () => {
    connectionWith([accountRow({ id: "acct-a", name: "Acme" }), accountRow({ id: "acct-b", name: "Borden Ltd" })]);
    const onStore = vi.fn();
    const { result } = renderHook(() => useClientScope("", onStore), {
      wrapper: ({ children }) => inSession(children, { role: "developer", everyAccount: true }),
    });
    await waitFor(() => expect(result.current.options).toHaveLength(3));
    act(() => result.current.set("acct-a"));
    expect(onStore).toHaveBeenLastCalledWith("acct-a");
    act(() => result.current.set("acct-elsewhere"));
    expect(onStore).toHaveBeenLastCalledWith("");
  });
});

// ===========================================================================
// A STAFF MEMBER'S CHOICE SURVIVES THE COLD OPEN
// ===========================================================================
// Staff choose among every organization, and only the account list names
// them. Until that read answers, the list holds at most the organizations
// their own groups name, so validating the stored choice against it would
// drop the choice and the page's FIRST read would ask for every client, the
// one its person chose not to look at. The stored choice is kept, with no
// control, until the list has answered; the reads narrow on the server
// whatever is sent (D11), so a stale id costs an empty answer, never another
// client's figures.

describe("before the account list has answered", () => {
  it("keeps a staff member's stored client from the very first scope", async () => {
    const list = pendingAccountList();
    const { result } = renderHook(() => useClientScope("acct-b", () => {}), {
      wrapper: ({ children }) => inSession(children, { role: "developer", everyAccount: true }),
    });
    // The first scope a page reads under is already the stored client.
    expect(result.current).toMatchObject({ showControl: false, value: "acct-b", accountIds: ["acct-b"] });

    await act(async () => list.answer([accountRow({ id: "acct-a", name: "Acme" }), accountRow({ id: "acct-b", name: "Borden Ltd" })]));
    await waitFor(() => expect(result.current.showControl).toBe(true));
    expect(result.current).toMatchObject({ value: "acct-b", accountIds: ["acct-b"] });
  });

  it("drops the stored client once the account list answers without it", async () => {
    const list = pendingAccountList();
    const { result } = renderHook(() => useClientScope("acct-gone", () => {}), {
      wrapper: ({ children }) => inSession(children, { role: "developer", everyAccount: true }),
    });
    expect(result.current).toMatchObject({ value: "acct-gone", accountIds: ["acct-gone"] });

    await act(async () => list.answer([accountRow({ id: "acct-a", name: "Acme" }), accountRow({ id: "acct-b", name: "Borden Ltd" })]));
    await waitFor(() => expect(result.current.showControl).toBe(true));
    expect(result.current).toMatchObject({ value: "", accountIds: [] });
  });

  it("keeps the stored client while it is not yet known who this is", () => {
    // The session's access has not landed: the first read still asks for the
    // stored choice, not for everything and then again.
    expect(clientScopeFor(null, ACCOUNTS, "acct-b")).toEqual({ showControl: false, value: "acct-b", options: [], accountIds: ["acct-b"] });
    expect(clientScopeFor(null, ACCOUNTS, "")).toEqual({ showControl: false, value: "", options: [], accountIds: [] });
  });

  it("is the stored choice, or All, with no control, for the pure rule too", () => {
    expect(clientScopeFor(STAFF, [], "acct-b", false)).toEqual({ showControl: false, value: "acct-b", options: [], accountIds: ["acct-b"] });
    expect(clientScopeFor(STAFF, [], "", false)).toEqual({ showControl: false, value: "", options: [], accountIds: [] });
    // A member's choices come from who they are, not from the list, so a
    // member is answered at once.
    expect(clientScopeFor(access({ accountIds: ["acct-a", "acct-c"], groups: [GROUP] }), [], "acct-c", false)).toMatchObject({ showControl: true, value: "acct-c" });
  });
});
