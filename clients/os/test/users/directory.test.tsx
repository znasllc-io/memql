import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
const h = vi.hoisted(() => ({ connection: null as any }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection, osBridgePath: "", bridgePathFor: () => "" }));
const { fakeConnection, userRow, invitationRow, withSession } = await import("./harness");
const { Result } = await import("@znasllc-io/memql-sdk-core/client");
const { LiveReadVisibility } = await import("../../src/live/useGraphRevision");
const { useRosterDirectory } = await import("../../src/apps/users/useRosterDirectory");
const { DEFAULT_ROSTER_FILTER } = await import("../../src/apps/users/roster");
const filter = { ...DEFAULT_ROSTER_FILTER };
const people = Array.from({ length: 25 }, (_, n) => userRow({ id: `person-${n}`, displayName: `Person ${n}`, role: n === 0 ? "viewer" : "user", createdAt: `2026-10-${String(n + 1).padStart(2, "0")}T10:00:00Z` }));
const invites = Array.from({ length: 25 }, (_, n) => invitationRow({ id: `invite-${n}`, inviteeName: `Invitee ${n}`, expiresAt: "2099-01-01T00:00:00Z", createdAt: `2026-10-${String(n + 1).padStart(2, "0")}T11:00:00Z` }));
const wrapper = ({ children }: { children: React.ReactNode }) => withSession(children);
const shown = (directory: ReturnType<typeof useRosterDirectory>) => directory.source?.snapshot.rows ?? [];
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


it("keeps the merged window behind an interrupted frontier and retries only that side", async () => {
  const view = renderHook(() => useRosterDirectory(filter), { wrapper });
  await waitFor(() => expect(view.result.current.invites.snapshot.state).toBe("live"));
  h.connection.query.pendingUserInvitationsBrowse.mockRejectedValueOnce(Object.assign(new Error("stream closed"), { reason: "transport" }));
  await act(() => view.result.current.loadMore());
  expect(view.result.current.invites.snapshot.state).toBe("disconnected");
  expect(view.result.current.loadMoreError).toBe("");
  expect(view.result.current.visible).toBe(10);
  expect(shown(view.result.current)).toHaveLength(10);
  act(() => view.result.current.invites.reseed());
  await waitFor(() => expect(view.result.current.invites.snapshot.state).toBe("live"));
  await act(() => view.result.current.loadMore());
  expect(view.result.current.visible).toBe(20);
  expect(h.connection.query.peopleBrowse).toHaveBeenCalledTimes(2);
  expect(h.connection.query.pendingUserInvitationsBrowse).toHaveBeenCalledTimes(4);
});

it("does not reveal a merged page when a second Load more overlaps its pending frontier", async () => {
  const view = renderHook(() => useRosterDirectory(filter), { wrapper });
  await waitFor(() => expect(view.result.current.invites.snapshot.state).toBe("live"));
  const read = h.connection.query.pendingUserInvitationsBrowse.getMockImplementation();
  let finish!: () => void;
  h.connection.query.pendingUserInvitationsBrowse.mockImplementationOnce((...args: unknown[]) => new Promise(resolve => {
    finish = () => { void read(...args).then(resolve); };
  }));
  let first!: Promise<void>;
  let overlapping!: Promise<void>;
  await act(async () => {
    first = view.result.current.loadMore();
    overlapping = view.result.current.loadMore();
    await Promise.resolve();
  });
  expect(view.result.current.visible).toBe(10);
  expect(shown(view.result.current)).toHaveLength(10);
  await act(async () => { finish(); await Promise.all([first, overlapping]); });
  expect(view.result.current.visible).toBe(20);
  expect(h.connection.query.peopleBrowse).toHaveBeenCalledTimes(2);
  expect(h.connection.query.pendingUserInvitationsBrowse).toHaveBeenCalledTimes(2);
});


it("shares a completed merged window across consumers while a new authenticated connection starts at ten", async () => {
 const first=renderHook(()=>useRosterDirectory(filter),{wrapper});
 const second=renderHook(()=>useRosterDirectory(filter),{wrapper});
 await waitFor(()=>expect(first.result.current.invites.snapshot.state).toBe("live"));
 await act(()=>first.result.current.loadMore());
 expect(first.result.current.visible).toBe(20);
 expect(second.result.current.visible).toBe(20);
 await act(()=>second.result.current.loadMore());
 expect(first.result.current.visible).toBe(30);
 expect(second.result.current.visible).toBe(30);
 first.unmount(); second.unmount();
 h.connection=fakeConnection({searchUsers:people,pendingUserInvitations:invites});
 const fresh=renderHook(()=>useRosterDirectory(filter),{wrapper});
 await waitFor(()=>expect(fresh.result.current.invites.snapshot.state).toBe("live"));
 expect(fresh.result.current.visible).toBe(10);
 expect(shown(fresh.result.current)).toHaveLength(10);
});


it("withholds a partial initial merge until both source frontiers are known", async () => {
  const read = h.connection.query.pendingUserInvitationsBrowse.getMockImplementation();
  let finish!: () => void;
  h.connection.query.pendingUserInvitationsBrowse.mockImplementationOnce((...args: unknown[]) => new Promise(resolve => {
    finish = () => { void read(...args).then(resolve); };
  }));
  const view = renderHook(() => useRosterDirectory(filter), { wrapper });
  await waitFor(() => expect(view.result.current.users.snapshot.state).toBe("live"));
  expect(view.result.current.invites.snapshot.state).toBe("seeding");
  expect(shown(view.result.current)).toEqual([]);
  expect(view.result.current.source?.snapshot.state).toBe("seeding");
  expect(view.result.current.visible).toBe(10);
  await act(async () => { finish(); });
  await waitFor(() => expect(shown(view.result.current)).toHaveLength(10));
  expect(shown(view.result.current)[0]?.id).toBe("invite-24");
  expect(h.connection.query.peopleBrowse).toHaveBeenCalledTimes(1);
  expect(h.connection.query.pendingUserInvitationsBrowse).toHaveBeenCalledTimes(1);
});

it.each([false, true])("holds the reliable prefix during a shared live-removal refill (Load more during repair: %s)", async loadDuringRepair => {
  h.connection = fakeConnection({ searchUsers: people.map(row => ({ ...row, createdAt: String(row["createdAt"]).replace("2026-", "2025-") })), pendingUserInvitations: invites });
  const view = renderHook(() => useRosterDirectory(filter), { wrapper });
  const second = renderHook(() => useRosterDirectory(filter), { wrapper });
  await waitFor(() => expect(view.result.current.invites.snapshot.state).toBe("live"));
  expect(shown(view.result.current).every(row => row.kind === "invited")).toBe(true);
  const read = h.connection.query.pendingUserInvitationsBrowse.getMockImplementation();
  let finish!: () => void;
  h.connection.query.pendingUserInvitationsBrowse.mockImplementationOnce((...args: unknown[]) => new Promise(resolve => {
    finish = () => { void read(...args).then(resolve); };
  }));
  await act(async () => {
    for (let n = 20; n < 25; n++) h.connection.subscriptions.emit("v1:identity:invitation", { ...invites[n], status: "accepted", createdAt: new Date().toISOString() });
    expect(shown(view.result.current).map(row => row.id)).toEqual(Array.from({ length: 5 }, (_, n) => `invite-${19 - n}`));
  });
  const reliable = Array.from({ length: 5 }, (_, n) => `invite-${19 - n}`);
  expect(shown(view.result.current).map(row => row.id)).toEqual(reliable);
  expect(shown(second.result.current).map(row => row.id)).toEqual(reliable);
  expect(view.result.current.visible).toBe(10);
  expect(h.connection.query.pendingUserInvitationsBrowse).toHaveBeenCalledTimes(2);
  expect(h.connection.query.peopleBrowse).toHaveBeenCalledTimes(1);
  let requests: Promise<void>[] = [];
  if (loadDuringRepair) {
    await act(async () => {
      requests = [view.result.current.loadMore(), second.result.current.loadMore()];
      await Promise.resolve();
    });
    expect(view.result.current.visible).toBe(10);
  }
  await act(async () => { finish(); await Promise.all(requests); });
  await waitFor(() => expect(shown(view.result.current)).toHaveLength(loadDuringRepair ? 20 : 10));
  expect(view.result.current.visible).toBe(loadDuringRepair ? 20 : 10);
  expect(second.result.current.visible).toBe(view.result.current.visible);
  if (!loadDuringRepair) await act(() => view.result.current.loadMore());
  expect(shown(view.result.current).map(row => row.id)).toEqual(Array.from({ length: 20 }, (_, n) => `invite-${19 - n}`));
  expect(h.connection.query.pendingUserInvitationsBrowse).toHaveBeenCalledTimes(3);
  expect(h.connection.query.peopleBrowse).toHaveBeenCalledTimes(2);
});

it.each([5, 10])("keeps the reliable prefix and stops automatic retries after %i live removals and a failed refill", async removed => {
  h.connection = fakeConnection({ searchUsers: people.map(row => ({ ...row, createdAt: String(row["createdAt"]).replace("2026-", "2025-") })), pendingUserInvitations: invites });
  const view = renderHook(() => useRosterDirectory(filter), { wrapper });
  await waitFor(() => expect(view.result.current.invites.snapshot.state).toBe("live"));
  h.connection.query.pendingUserInvitationsBrowse.mockRejectedValueOnce(new Error("Invitation page unavailable"));
  await act(async () => {
    for (let n = 25 - removed; n < 25; n++) h.connection.subscriptions.emit("v1:identity:invitation", { ...invites[n], status: "accepted", createdAt: new Date().toISOString() });
  });
  expect(shown(view.result.current).map(row => row.id)).toEqual(Array.from({ length: 10 - removed }, (_, n) => `invite-${24 - removed - n}`));
  expect(view.result.current.visible).toBe(10);
  expect(view.result.current.loadMoreError).toBe("Invitation page unavailable");
  expect(view.result.current.source?.snapshot.state).toBe("degraded");
  view.rerender();
  await act(async () => {});
  expect(h.connection.query.pendingUserInvitationsBrowse).toHaveBeenCalledTimes(2);
  expect(h.connection.query.peopleBrowse).toHaveBeenCalledTimes(1);
  await act(() => view.result.current.loadMore());
  expect(view.result.current.visible).toBe(20);
  expect(shown(view.result.current).map(row => row.id)).toEqual([...Array.from({ length: 25 - removed }, (_, n) => `invite-${24 - removed - n}`), ...Array.from({ length: removed - 5 }, (_, n) => `person-${24 - n}`)]);
  expect(h.connection.query.pendingUserInvitationsBrowse).toHaveBeenCalledTimes(4);
  expect(h.connection.query.peopleBrowse).toHaveBeenCalledTimes(2);
});

it.each(["hide", "unmount"])("stops a shared refill when its last visible consumer leaves (%s)", async leave => {
  h.connection = fakeConnection({ searchUsers: people.map(row => ({ ...row, createdAt: String(row["createdAt"]).replace("2026-", "2025-") })), pendingUserInvitations: invites });
  let visible = true;
  const pane = ({ children }: { children: React.ReactNode }) => withSession(<LiveReadVisibility.Provider value={visible}>{children}</LiveReadVisibility.Provider>);
  const view = renderHook(() => useRosterDirectory(filter), { wrapper: pane });
  await waitFor(() => expect(view.result.current.invites.snapshot.state).toBe("live"));
  let finish!: () => void;
  h.connection.query.pendingUserInvitationsBrowse.mockImplementationOnce(() => new Promise(resolve => {
    finish = () => resolve(new Result({ data: [], meta: { cursor: "20" } } as never));
  }));
  await act(async () => {
    for (let n = 15; n < 25; n++) h.connection.subscriptions.emit("v1:identity:invitation", { ...invites[n], status: "accepted", createdAt: new Date().toISOString() });
  });
  expect(shown(view.result.current)).toEqual([]);
  expect(view.result.current.source?.snapshot.state).toBe("seeding");
  expect(h.connection.query.pendingUserInvitationsBrowse).toHaveBeenCalledTimes(2);
  if (leave === "hide") { visible = false; view.rerender(); }
  else view.unmount();
  await act(async () => { finish(); });
  // A page can finish during the subscription grace period, but no follow-up
  // read should start with no visible consumer.
  expect(h.connection.query.pendingUserInvitationsBrowse).toHaveBeenCalledTimes(2);
  visible = true;
  const resumed = leave === "hide" ? (view.rerender(), view) : renderHook(() => useRosterDirectory(filter), { wrapper: pane });
  await waitFor(() => expect(shown(resumed.result.current)).toHaveLength(10));
  expect(resumed.result.current.visible).toBe(10);
  expect(h.connection.query.pendingUserInvitationsBrowse).toHaveBeenCalledTimes(3);
  expect(h.connection.query.peopleBrowse).toHaveBeenCalledTimes(1);
});

it.each(["recent", "name"] as const)("coalesces overlapping cached-window advances for %s order", async sort => {
  h.connection = fakeConnection({ searchUsers: sort === "recent" ? people.slice(0, 6) : people, pendingUserInvitations: sort === "recent" ? invites.slice(0, 6) : invites });
  const view = renderHook(() => useRosterDirectory({ ...filter, sort }), { wrapper });
  await waitFor(() => expect(view.result.current.invites.snapshot.state).toBe("live"));
  await act(async () => {
    await Promise.all([view.result.current.loadMore(), view.result.current.loadMore()]);
  });
  expect(view.result.current.visible).toBe(20);
  expect(shown(view.result.current)).toHaveLength(sort === "recent" ? 12 : 20);
});
