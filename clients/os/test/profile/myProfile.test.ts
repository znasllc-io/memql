import { act, renderHook, waitFor } from "@testing-library/react";
import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import {
  patchMyProfile,
  profileFromRow,
  profileInitial,
  profileName,
  resetMyProfileForTest,
  useMyProfile,
} from "../../src/modules/profile/myProfile";

// The shared copy of the signed-in person's row (memql#6220).

function rows(list: Row[]): Result {
  return new Result({ data: list } as never);
}

const ROW: Row = {
  id: "v1:identity:user:ada",
  displayName: "Ada Lovelace",
  firstName: "Ada",
  lastName: "Lovelace",
  primaryEmail: "ada@example.test",
  role: "owner",
  avatar: "data:image/webp;base64,AAAA",
  signInPolicy: "passkey_only",
  preferences: { timezone: "Europe/London" },
};

describe("reading the person's row", () => {
  it("reads every field the Profile draws", () => {
    expect(profileFromRow(ROW)).toEqual({
      userId: "v1:identity:user:ada",
      displayName: "Ada Lovelace",
      firstName: "Ada",
      lastName: "Lovelace",
      email: "ada@example.test",
      role: "owner",
      phone: "",
      title: "",
      avatar: "data:image/webp;base64,AAAA",
      signInPolicy: "passkey_only",
      computerUseEnabled: true,
      timezone: "Europe/London",
    });
  });

  it("reads an ABSENT computer-use switch as on, and an explicit false as off", () => {
    expect(profileFromRow({ ...ROW, preferences: {} }).computerUseEnabled).toBe(true);
    expect(profileFromRow({ ...ROW, preferences: { computerUseEnabled: false } }).computerUseEnabled).toBe(false);
    expect(profileFromRow({ ...ROW, preferences: undefined }).computerUseEnabled).toBe(true);
  });

  it("names the person by display name, then name parts, then address", () => {
    expect(profileName({ displayName: "Ada Lovelace", firstName: "", lastName: "", email: "a@x" })).toBe("Ada Lovelace");
    expect(profileName({ displayName: " ", firstName: "Ada", lastName: "Byron", email: "a@x" })).toBe("Ada Byron");
    expect(profileName({ displayName: "", firstName: "", lastName: "", email: "ada@example.test" })).toBe("ada@example.test");
    expect(profileInitial("ada lovelace")).toBe("A");
    expect(profileInitial("")).toBe("?");
  });
});

describe("one copy for every place the person is drawn", () => {
  afterEach(() => {
    resetMyProfileForTest();
    h.connection = null;
  });

  it("reads once for two readers, and a write that lands reaches both", async () => {
    const currentUser = vi.fn(async () => rows([ROW]));
    h.connection = { query: { currentUser } };
    const dock = renderHook(() => useMyProfile());
    const page = renderHook(() => useMyProfile());
    await waitFor(() => expect(dock.result.current.state).toBe("ready"));
    expect(currentUser).toHaveBeenCalledTimes(1);
    expect(page.result.current.profile?.avatar).toBe("data:image/webp;base64,AAAA");

    act(() => patchMyProfile({ avatar: "" }));
    expect(dock.result.current.profile?.avatar).toBe("");
    expect(page.result.current.profile?.avatar).toBe("");
  });

  it("says why when the read fails, and a connection that cannot make it lands the same way", async () => {
    h.connection = { query: { currentUser: vi.fn(async () => { throw new Error("stream closed"); }) } };
    const failed = renderHook(() => useMyProfile());
    await waitFor(() => expect(failed.result.current.state).toBe("failed"));
    expect(failed.result.current.error).toBe("stream closed");
    failed.unmount();

    resetMyProfileForTest();
    h.connection = { query: {} };
    const broken = renderHook(() => useMyProfile());
    await waitFor(() => expect(broken.result.current.state).toBe("failed"));
    expect(broken.result.current.profile).toBeNull();
  });
});
