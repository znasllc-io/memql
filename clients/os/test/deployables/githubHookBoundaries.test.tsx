import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({
  connection: { query: {} },
  repositories: vi.fn(), revoke: vi.fn(), begin: vi.fn(),
}));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));
vi.mock("../../src/apps/deployables/sources/calls", () => ({
  readSourceRepositories: h.repositories, revokeSourceCredential: h.revoke, githubConnectBegin: h.begin,
}));
import { useCredentialRevoke, useGithubConnect, useSourceRepositories } from "../../src/apps/deployables/sources/useGithubConnect";
import { withSession } from "./harness";
import { EMPTY_PAGE } from "../../src/apps/deployables/sources/repositories";

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: Error) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
const page = (login: string) => ({ ...EMPTY_PAGE, installations: [{ id: login, login, accountType: "Organization" }], reason: "ok" });
beforeEach(() => vi.clearAllMocks());

describe("GitHub hook request boundaries", () => {
  it.each(["success", "failure"])("clear discards a late repository %s and all prior page facts", async outcome => {
    h.repositories.mockResolvedValueOnce(page("acme"));
    const { result } = renderHook(useSourceRepositories);
    await act(async () => { await result.current.read("a", 1); });
    expect(result.current.readAt).not.toBe("");
    const pending = deferred<ReturnType<typeof page>>();
    h.repositories.mockReturnValueOnce(pending.promise);
    let read!: Promise<boolean>;
    act(() => { read = result.current.read("a", 2); });
    act(() => result.current.clear());
    expect(result.current.page).toEqual(EMPTY_PAGE);
    expect(result.current.readAt).toBe("");
    expect(result.current.busy).toBe(false);
    await act(async () => {
      if (outcome === "success") pending.resolve(page("old"));
      else pending.reject(new Error("credential_revoked: old request refused"));
      expect(await read).toBe(false);
    });
    expect(result.current.page).toEqual(EMPTY_PAGE);
    expect(result.current.refusal).toBeNull();
    expect(result.current.readAt).toBe("");
  });

  it("changing credentials drops the prior page and keeps the newer request busy when an older one settles", async () => {
    h.repositories.mockResolvedValueOnce(page("old"));
    const { result } = renderHook(useSourceRepositories);
    await act(async () => { await result.current.read("a", 1); });
    const old = deferred<ReturnType<typeof page>>();
    const next = deferred<ReturnType<typeof page>>();
    h.repositories.mockReturnValueOnce(old.promise).mockReturnValueOnce(next.promise);
    let oldRead!: Promise<boolean>, nextRead!: Promise<boolean>;
    act(() => { oldRead = result.current.read("a", 2); });
    act(() => { nextRead = result.current.read("b", 2); });
    expect(result.current.readAt).toBe("");
    expect(result.current.page).toEqual(EMPTY_PAGE);
    await act(async () => { old.reject(new Error("forbidden: previous owner")); await oldRead; });
    expect(result.current.busy).toBe(true);
    expect(result.current.refusal).toBeNull();
    await act(async () => { next.resolve(page("new")); expect(await nextRead).toBe(true); });
    expect(result.current.busy).toBe(false);
    expect(result.current.page.installations.map(row => row.login)).toEqual(["new"]);
  });

  it("switching organizations under one grant drops old pages and ignores a late answer", async () => {
    h.repositories.mockResolvedValueOnce(page("acme"));
    const { result } = renderHook(useSourceRepositories);
    await act(async () => { await result.current.read("grant", 1, "source-acme"); });
    const old = deferred<ReturnType<typeof page>>();
    h.repositories.mockReturnValueOnce(old.promise).mockResolvedValueOnce(page("beta"));
    let oldRead!: Promise<boolean>;
    act(() => { oldRead = result.current.read("grant", 2, "source-acme"); });
    await act(async () => { await result.current.read("grant", 1, "source-beta"); });
    await act(async () => { old.resolve(page("acme-late")); expect(await oldRead).toBe(false); });
    expect(result.current.page.installations.map(row => row.login)).toEqual(["beta"]);
    expect(h.repositories).toHaveBeenLastCalledWith(h.connection.query, "grant", 1, "source-beta");
  });

  it.each([true, false])("returns successful local revoke when GitHub acknowledgement is %s", async remote => {
    h.revoke.mockResolvedValueOnce(remote);
    const { result } = renderHook(useCredentialRevoke);
    await act(async () => { expect(await result.current.revoke("grant")).toBe(true); });
    expect(result.current.revokedCredentialId).toBe("grant");
    expect(result.current.remoteRevoked).toBe(remote);
    act(() => result.current.clear());
    expect(result.current.revokedCredentialId).toBe("");
    expect(result.current.remoteRevoked).toBeNull();
  });

  it("does not claim revocation succeeded when the cluster refuses it", async () => {
    h.revoke.mockRejectedValueOnce(new Error("forbidden: not your credential"));
    const { result } = renderHook(useCredentialRevoke);
    await act(async () => { expect(await result.current.revoke("grant")).toBe(false); });
    expect(result.current.revokedCredentialId).toBe("");
    expect(result.current.remoteRevoked).toBeNull();
    expect(result.current.refusal?.code).toBe("forbidden");
  });

  it.each(["success", "failure"])("clear prevents a late install-link %s from reviving the old connection", async outcome => {
    const pending = deferred<{ installUrl: string; authorizeUrl: string; reason: string }>();
    h.begin.mockReturnValueOnce(pending.promise);
    const { result } = renderHook(useGithubConnect, { wrapper: ({ children }) => withSession(children) });
    let learn!: Promise<boolean>;
    act(() => { learn = result.current.learn("/return"); });
    act(() => result.current.clear());
    await act(async () => {
      if (outcome === "success") pending.resolve({ installUrl: "https://github.com/apps/old", authorizeUrl: "", reason: "ok" });
      else pending.reject(new Error("forbidden: old lookup"));
      expect(await learn).toBe(false);
    });
    expect(result.current.installUrl).toBe("");
    expect(result.current.refusal).toBeNull();
    expect(result.current.busy).toBe(false);
  });
  it("does not complete a disconnected card action after its owner unmounts", async () => {
    const pending = deferred<boolean>();
    h.revoke.mockReturnValueOnce(pending.promise);
    const { result, unmount } = renderHook(useCredentialRevoke);
    let revoke!: Promise<boolean>;
    act(() => { revoke = result.current.revoke("grant"); });
    unmount();
    await act(async () => { pending.resolve(true); expect(await revoke).toBe(false); });
  });

  it("a begin response arriving after unmount cannot finish its connect flow", async () => {
    const pending = deferred<{ installUrl: string; authorizeUrl: string; reason: string }>();
    h.begin.mockReturnValueOnce(pending.promise);
    const { result, unmount } = renderHook(useGithubConnect, { wrapper: ({ children }) => withSession(children) });
    let learn!: Promise<boolean>;
    act(() => { learn = result.current.learn("/return"); });
    unmount();
    await act(async () => {
      pending.resolve({ installUrl: "", authorizeUrl: "https://github.com/login/oauth/authorize", reason: "ok" });
      expect(await learn).toBe(false);
    });
  });

});
