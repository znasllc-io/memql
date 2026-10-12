import { renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { AccountOptionsProvider, useAccountOptions } = await import("../../src/apps/accounts/tie");
const { ACCOUNT_CONCEPT } = await import("../../src/apps/accounts/rows");
const { accountRow, fakeConnection, withSession } = await import("./harness");

// ===========================================================================
// ONE ACCOUNT LIST FOR A WHOLE APP (the Email app, memql#6033)
// ===========================================================================
// `useAccountOptions` opens one collection per mounting component, which is
// accepted across apps and not inside one (see tie.tsx). An app that puts a
// picker in many places retains the one feed at its root. These pin both
// halves: under the provider there is ONE subscription and ONE read however
// many pickers mount; outside it nothing changed, so the four apps that mount
// a picker of their own keep theirs.

function opened(connection: ReturnType<typeof fakeConnection>) {
  const spy = vi.spyOn(connection.subscriptions, "subscribeGraph");
  return () => spy.mock.calls.filter(([, options]) => options.concept === ACCOUNT_CONCEPT).length;
}

/** Three pickers, as an app with a picker on three pages would mount them. */
const threePickers = () => [useAccountOptions(), useAccountOptions(), useAccountOptions()];

describe("the account list under a provider", () => {
  it("opens one subscription and one read however many pickers mount", async () => {
    const connection = fakeConnection({ clientAccountsAll: [accountRow({ id: "acme", name: "Acme" })] });
    h.connection = connection;
    const subscriptions = opened(connection);
    const { result } = renderHook(threePickers, {
      wrapper: ({ children }) => withSession(<AccountOptionsProvider>{children}</AccountOptionsProvider>),
    });

    await waitFor(() => expect(result.current[0]!.map((a) => a.id)).toEqual(["acme"]));
    // Every picker reads the same list.
    expect(result.current[1]).toBe(result.current[0]);
    expect(result.current[2]).toBe(result.current[0]);
    expect(subscriptions()).toBe(1);
    expect(connection.query.clientAccountsAll).toHaveBeenCalledTimes(1);
  });

  it("gives every picker the caller's own clients when the read is empty", async () => {
    const connection = fakeConnection({ clientAccountsAll: [] });
    h.connection = connection;
    const { result } = renderHook(threePickers, {
      wrapper: ({ children }) => withSession(
        <AccountOptionsProvider>{children}</AccountOptionsProvider>,
        { groups: [{ id: "g1", name: "Acme", kind: "account", accountId: "acct-acme", accountName: "Acme" }] },
      ),
    });
    await waitFor(() => expect(result.current[2]!.map((a) => a.id)).toEqual(["acct-acme"]));
    expect(result.current[0]!.map((a) => a.id)).toEqual(["acct-acme"]);
  });
});

describe("the account list outside a provider", () => {
  it("keeps one subscription per picker, as every other app has", async () => {
    const connection = fakeConnection({ clientAccountsAll: [accountRow({ id: "acme", name: "Acme" })] });
    h.connection = connection;
    const subscriptions = opened(connection);
    const { result } = renderHook(threePickers, { wrapper: ({ children }) => withSession(children) });

    await waitFor(() => expect(result.current[2]!.map((a) => a.id)).toEqual(["acme"]));
    expect(subscriptions()).toBe(3);
  });
});
