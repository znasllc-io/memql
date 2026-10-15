import type { ReactNode } from "react";
import { act, renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { installSeededAccess } from "../seededAccess";
import { fakeConnection, rowsResult, withSession } from "./harness";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
const { useSessions, useSessionsCount } = await import("../../src/apps/users/useSessions");

describe("session reads follow changing effective access", () => {
  it("starts reading after permission arrives and clears both views when it is revoked", async () => {
    const connection = fakeConnection({ sessionsForSubjectAdmin: [{ id: "session", clientLabel: "Browser" }] });
    h.connection = connection;
    let role = "developer";
    installSeededAccess(role);
    const view = renderHook(() => ({ sessions: useSessions("person"), count: useSessionsCount("person") }), {
      wrapper: ({ children }: { children: ReactNode }) => withSession(children, { role }),
    });
    expect(connection.query.sessionsForSubjectAdmin).not.toHaveBeenCalled();
    role = "admin";
    installSeededAccess(role);
    view.rerender();
    await waitFor(() => expect(view.result.current.count.count).toBe(1));
    expect(view.result.current.sessions.live).toHaveLength(1);

    role = "developer";
    installSeededAccess(role);
    view.rerender();
    expect(view.result.current.sessions.live).toEqual([]);
    expect(view.result.current.sessions.unknown).toBe(true);
    expect(view.result.current.count.count).toBeNull();
    expect(connection.query.sessionsForSubjectAdmin).toHaveBeenCalledTimes(2);
  });

  it("ignores an in-flight answer after permission is revoked", async () => {
    const connection = fakeConnection();
    h.connection = connection;
    let finish!: (result: ReturnType<typeof rowsResult>) => void;
    connection.query.sessionsForSubjectAdmin.mockReturnValue(new Promise(resolve => { finish = resolve; }));
    let role = "admin";
    installSeededAccess(role);
    const view = renderHook(() => useSessions("person"), {
      wrapper: ({ children }: { children: ReactNode }) => withSession(children, { role }),
    });
    expect(connection.query.sessionsForSubjectAdmin).toHaveBeenCalledOnce();
    role = "developer";
    installSeededAccess(role);
    view.rerender();
    await act(async () => finish(rowsResult([{ id: "session", clientLabel: "Browser" }])));
    expect(view.result.current.live).toEqual([]);
    expect(view.result.current.unknown).toBe(true);
  });
});
