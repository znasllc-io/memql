import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as any }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection, osBridgePath: "", bridgePathFor: () => "" }));
const { fakeConnection, accountRow, withSession } = await import("./harness");
const { useAccounts, useAccountRecord } = await import("../../src/apps/accounts/useAccounts");
const rows = Array.from({ length: 25 }, (_, n) => accountRow({ id: `account-${n}`, name: `Client ${n}`, notes: n === 0 ? "Long standing contract" : "", createdAt: `2026-10-${String(n + 1).padStart(2, "0")}T00:00:00Z` }));
const wrapper = ({ children }: { children: React.ReactNode }) => withSession(children);
beforeEach(() => { h.connection = fakeConnection({ clientAccountsAll: rows }); });
afterEach(cleanup);
it("pages ten clients, retains them on reopen, and keeps record lookup independent of loaded pages", async () => {
  const first = renderHook(() => ({ list: useAccounts(), record: useAccountRecord("account-0") }), { wrapper });
  await waitFor(() => expect(first.result.current.list.snapshot.rows).toHaveLength(10));
  await waitFor(() => expect(first.result.current.record.snapshot.rows[0]?.["id"]).toBe("account-0"));
  expect(first.result.current.list.snapshot.rows.some(row => row["id"] === "account-0")).toBe(false);
  expect(h.connection.query.clientAccountsAll).not.toHaveBeenCalled();
  await act(() => first.result.current.list.loadMore());
  expect(first.result.current.list.snapshot.rows).toHaveLength(20);
  first.unmount();
  const reopened = renderHook(() => useAccounts(), { wrapper });
  expect(reopened.result.current.snapshot.rows).toHaveLength(20);
  expect(h.connection.query.clientAccountsBrowse).toHaveBeenCalledTimes(2);
});
it("finds contact notes beyond the initial page with a searched server request", async () => {
  const view = renderHook(() => useAccounts("STANDING"), { wrapper });
  await waitFor(() => expect(view.result.current.snapshot.rows).toHaveLength(1));
  expect(view.result.current.snapshot.rows[0]?.["id"]).toBe("account-0");
  expect(h.connection.query.clientAccountsBrowse).toHaveBeenCalledWith({ search: "STANDING", includeArchived: false }, expect.anything());
});
