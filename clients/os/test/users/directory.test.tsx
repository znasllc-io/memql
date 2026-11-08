import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as any }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection, osBridgePath: "", bridgePathFor: () => "" }));
const { fakeConnection, userRow, invitationRow, withSession } = await import("./harness");
const { useRosterDirectory } = await import("../../src/apps/users/useRosterDirectory");
const { DEFAULT_ROSTER_FILTER, foldRoster } = await import("../../src/apps/users/roster");
const { personFromRow, invitationFromRow } = await import("../../src/apps/users/rows");
const filter = { ...DEFAULT_ROSTER_FILTER };
const people = Array.from({ length: 25 }, (_, n) => userRow({ id: `person-${n}`, displayName: `Person ${n}`, role: n === 0 ? "viewer" : "user", createdAt: `2026-10-${String(n + 1).padStart(2, "0")}T10:00:00Z` }));
const invites = Array.from({ length: 25 }, (_, n) => invitationRow({ id: `invite-${n}`, inviteeName: `Invitee ${n}`, expiresAt: "2099-01-01T00:00:00Z", createdAt: `2026-10-${String(n + 1).padStart(2, "0")}T11:00:00Z` }));
const wrapper = ({ children }: { children: React.ReactNode }) => withSession(children);
const shown = (directory: ReturnType<typeof useRosterDirectory>) => foldRoster(directory.users.snapshot.rows.map(personFromRow), directory.invites.snapshot.rows.map(invitationFromRow), filter, new Date()).slice(0, directory.visible);
beforeEach(() => { h.connection = fakeConnection({ searchUsers: people, pendingUserInvitations: invites }); });
afterEach(cleanup);

it("uses two ten-row queries to show the newest ten combined records and retains the loaded window on reopen", async () => {
  const first = renderHook(() => useRosterDirectory(filter), { wrapper });
  await waitFor(() => expect(first.result.current.invites.snapshot.state).toBe("live"));
  expect(shown(first.result.current).map(row => row.id)).toEqual(["invite-24", "person-24", "invite-23", "person-23", "invite-22", "person-22", "invite-21", "person-21", "invite-20", "person-20"]);
  expect(h.connection.query.peopleBrowse).toHaveBeenCalledTimes(1);
  expect(h.connection.query.pendingUserInvitationsBrowse).toHaveBeenCalledTimes(1);
  expect(h.connection.query.searchUsers).not.toHaveBeenCalled();
  expect(h.connection.query.pendingUserInvitations).not.toHaveBeenCalled();
  await act(() => first.result.current.loadMore());
  expect(shown(first.result.current)).toHaveLength(20);
  first.unmount();
  const reopened = renderHook(() => useRosterDirectory(filter), { wrapper });
  expect(shown(reopened.result.current)).toHaveLength(20);
  expect(h.connection.query.peopleBrowse).toHaveBeenCalledTimes(2);
  expect(h.connection.query.pendingUserInvitationsBrowse).toHaveBeenCalledTimes(2);
});

it("searches and filters the server population before paging, including records beyond the initial page", async () => {
  const query = { ...filter, search: "PERSON 0", role: "viewer", group: "selected-group" };
  const view = renderHook(() => useRosterDirectory(query, new Set(["person-0"])), { wrapper });
  await waitFor(() => expect(view.result.current.users.snapshot.state).toBe("live"));
  expect(view.result.current.users.snapshot.rows.map(row => row["id"])).toEqual(["person-0"]);
  expect(h.connection.query.peopleBrowse).toHaveBeenCalledWith(expect.objectContaining({ search: "PERSON 0", role: "viewer", memberIds: ["person-0"] }), expect.anything());
  expect(h.connection.query.pendingUserInvitationsBrowse).toHaveBeenCalledWith(expect.objectContaining({ groupId: "selected-group" }), expect.anything());
});

it("holds the merged window until both page frontiers succeed and retries only the failed side", async () => {
  const view = renderHook(() => useRosterDirectory(filter), { wrapper });
  await waitFor(() => expect(view.result.current.invites.snapshot.state).toBe("live"));
  h.connection.query.pendingUserInvitationsBrowse.mockRejectedValueOnce(new Error("Invitation page unavailable"));
  await act(() => view.result.current.loadMore());
  expect(shown(view.result.current)).toHaveLength(10);
  expect(view.result.current.loadMoreError).toBe("Invitation page unavailable");
  await act(() => view.result.current.loadMore());
  expect(shown(view.result.current)).toHaveLength(20);
  expect(h.connection.query.peopleBrowse).toHaveBeenCalledTimes(2);
  expect(h.connection.query.pendingUserInvitationsBrowse).toHaveBeenCalledTimes(3);
});

it("waits for group membership before asking the directory and suppresses unrelated catalogs", async () => {
  const view = renderHook(() => useRosterDirectory({ ...filter, group: "group" }), { wrapper });
  await act(async () => {});
  expect(view.result.current.users.snapshot.rows).toHaveLength(0);
  expect(h.connection.query.peopleBrowse).not.toHaveBeenCalled();
  expect(h.connection.query.pendingUserInvitationsBrowse).not.toHaveBeenCalled();
  expect(h.connection.query.searchUsers).not.toHaveBeenCalled();
});
