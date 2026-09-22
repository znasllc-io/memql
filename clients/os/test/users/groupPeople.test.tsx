import { act, renderHook, waitFor } from "@testing-library/react";
import { Result } from "@znasllc-io/memql-sdk-core/client";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
import { useGroupPeople } from "../../src/apps/users/useGroupPeople";

describe("the delegated group manager's people", () => {
  it("reads only the opened organization's bounded roster, preserving person IDs through the builtin envelope", async () => {
    const groupPeople = vi.fn(async (_args: { groupId: string }) => new Result({ data: [{ member: { id: "u-acme", concept: "integration:groups:person", payload: { displayName: "Acme member", primaryEmail: "member@acme.test", role: "acme-member", active: true } } }] } as never));
    const searchUsers = vi.fn();
    h.connection = { query: { groupPeople, searchUsers } };
    const { result, rerender } = renderHook(({ revision }) => useGroupPeople("acct-acme", revision), { initialProps: { revision: 1 } });
    await waitFor(() => expect(result.current.people).toHaveLength(1));
    expect(result.current.people[0]).toMatchObject({ id: "u-acme", displayName: "Acme member" });
    expect(groupPeople.mock.calls[0]?.[0]).toEqual({ groupId: "acct-acme" });
    expect(searchUsers).not.toHaveBeenCalled();
    rerender({ revision: 2 });
    await waitFor(() => expect(groupPeople).toHaveBeenCalledTimes(2));
  });
  it("keeps refusals visible and permits retry without retaining a previous roster", async () => {
    let refuse = true;
    const groupPeople = vi.fn(async () => { if (refuse) throw new Error("Organization membership was removed"); return new Result({ data: [] }); });
    h.connection = { query: { groupPeople } };
    const { result } = renderHook(() => useGroupPeople("acct-acme", 1));
    await waitFor(() => expect(result.current.error).toBe("Organization membership was removed"));
    expect(result.current.people).toEqual([]);
    refuse = false;
    act(() => result.current.reload());
    await waitFor(() => expect(result.current.error).toBe(""));
    expect(groupPeople).toHaveBeenCalledTimes(2);
  });
});
