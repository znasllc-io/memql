import { act, renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { useAppSetupMark } from "../../src/chrome/useAppSetupMark";
import { accountSetupState, githubAccountsFor } from "../../src/apps/deployables/sources/accountSetup";
import { credentialFromRow } from "../../src/apps/deployables/sources/rows";
import { credentialRow, githubGrantRow, withSession } from "./harness";

describe("personal GitHub setup", () => {
  const active = credentialFromRow(githubGrantRow({ id: "mine" }));
  const live = { state: "live" as const, error: "" };
  it("requires one active GitHub account, not an installation or source", () => {
    expect(accountSetupState([], live)).toBe("partial");
    expect(accountSetupState([{ ...active, installationIds: [] }], live)).toBe("ready");
    expect(accountSetupState([{ ...active, status: "revoked" }], live)).toBe("partial");
    expect(accountSetupState([active, { ...active, id: "old", status: "revoked" }], live)).toBe("ready");
  });
  it("does not count another user's account, pasted tokens, or locally revoked grants", () => {
    const accounts = githubAccountsFor([active,
      credentialFromRow(githubGrantRow({ id: "other", ownerUserId: "other-user" })),
      credentialFromRow(credentialRow({ id: "token" })),
    ], "u-me", ["mine"]);
    expect(accounts).toHaveLength(1);
    expect(accountSetupState(accounts, live)).toBe("partial");
    expect(githubAccountsFor([active], "", [])).toEqual([]);
  });
  it("never calls an unread or failed account list empty", () => {
    expect(accountSetupState([], { state: "seeding", error: "" })).toBe("unknown");
    expect(accountSetupState([], { state: "live", error: "read failed" })).toBe("unknown");
    expect(accountSetupState([active], { state: "disconnected", error: "" })).toBe("unknown");
  });
});

describe("desktop and phone Settings mark", () => {
  it("reacts to personal setup and clears it when switching apps", () => {
    const hook = renderHook(({ appId }) => useAppSetupMark(appId, null), {
      initialProps: { appId: "deployables" },
      wrapper: ({ children }) => withSession(children, { userId: "u-me" }),
    });
    expect(hook.result.current.settingsTone).toBeNull();
    act(() => hook.result.current.reportSetupState("partial"));
    expect(hook.result.current.settingsTone).toBe("partlySetUp");
    act(() => hook.result.current.reportSetupState("ready"));
    expect(hook.result.current.settingsTone).toBeNull();
    act(() => hook.result.current.reportSetupState("partial"));
    hook.rerender({ appId: "fleet" });
    expect(hook.result.current.settingsTone).toBeNull();
  });
  it("never carries a personal setup mark into another signed-in identity", () => {
    let userId = "u-me";
    const hook = renderHook(() => useAppSetupMark("deployables", null), { wrapper: ({ children }) => withSession(children, { userId }) });
    act(() => hook.result.current.reportSetupState("partial"));
    expect(hook.result.current.settingsTone).toBe("partlySetUp");
    userId = "u-other";
    hook.rerender();
    expect(hook.result.current.settingsTone).toBeNull();
  });
  it("does not override another app's infrastructure requirements", () => {
    const hook = renderHook(() => useAppSetupMark("other", "needsSetup"), { wrapper: ({ children }) => withSession(children) });
    act(() => hook.result.current.reportSetupState("ready"));
    expect(hook.result.current.settingsTone).toBe("needsSetup");
  });
});
