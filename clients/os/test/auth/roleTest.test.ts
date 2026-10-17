import { afterEach, expect, it, vi } from "vitest";
import { deliverRoleTest, receiveRoleTest, type RoleTestCredential } from "@znasllc-io/memql-sdk-core/identityadmin";

const credential: RoleTestCredential = { token: `mql_test_${"a".repeat(64)}`, sessionId: "session", role: "reader", domain: "example.test", expiresAt: Math.floor(Date.now()/1000)+1800 };
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

it("sends a credential only to the reserved window, exact origin and nonce, once", () => {
  const listeners = new Set<(e: MessageEvent) => void>();
  vi.stubGlobal("window", { addEventListener: (_: string, fn: (e: MessageEvent) => void) => listeners.add(fn), removeEventListener: (_: string, fn: (e: MessageEvent) => void) => listeners.delete(fn) });
  const tab = { location: { replace: vi.fn() }, postMessage: vi.fn() };
  const stop = deliverRoleTest(tab as unknown as Window, "https://vscode.example.test/editor/?resource=file", credential);
  const url = new URL(tab.location.replace.mock.calls[0]![0]);
  expect(url.href).not.toContain(credential.token);
  const message = { source: tab, origin: url.origin, data: { type: "memql.role-test.ready", nonce: url.searchParams.get("roleTest") } };
  const send = (event: unknown) => [...listeners].forEach(fn => fn(event as MessageEvent));
  send({ ...message, origin: "https://evil.test" });
  send({ ...message, source: {} });
  send({ ...message, data: { ...message.data, nonce: "wrong" } });
  expect(tab.postMessage).not.toHaveBeenCalled();
  send(message); send(message);
  expect(tab.postMessage).toHaveBeenCalledTimes(1);
  expect(tab.postMessage.mock.calls[0]![1]).toBe(url.origin);
  expect(listeners.size).toBe(0);
  stop();
});

it("a reloaded test tab refuses instead of borrowing the owner's session", async () => {
  vi.stubGlobal("location", { href: "https://os.example.test/?roleTest=nonce" });
  vi.stubGlobal("window", { opener: null });
  const fetch = vi.fn(); vi.stubGlobal("fetch",fetch);
  await expect(receiveRoleTest("https://os.example.test")).rejects.toThrow("ended");
  expect(fetch).not.toHaveBeenCalled();
});

it("receives from only its opener, then clears the opener without storing credentials", async () => {
  const listeners = new Set<(e: MessageEvent) => void>();
  const parent = { postMessage: vi.fn() };
  const child = { opener: parent as typeof parent | null, addEventListener: (_:string,fn:(e:MessageEvent)=>void)=>listeners.add(fn), removeEventListener: (_:string,fn:(e:MessageEvent)=>void)=>listeners.delete(fn) };
  vi.stubGlobal("window", child);
  vi.stubGlobal("location", { href: "https://os.example.test/?roleTest=nonce" });
  const storage = { setItem: vi.fn() }; vi.stubGlobal("localStorage",storage); vi.stubGlobal("sessionStorage",storage);
  const result = receiveRoleTest("https://os.example.test");
  const send=(event:unknown)=>[...listeners].forEach(fn=>fn(event as MessageEvent));
  const message={source:parent,origin:"https://os.example.test",data:{type:"memql.role-test.credential",nonce:"nonce",credential}};
  send({...message,source:{}}); expect(listeners.size).toBe(1);
  send(message); expect(await result).toEqual(credential);
  expect(child.opener).toBeNull(); expect(storage.setItem).not.toHaveBeenCalled();
});


it("refuses cookie-backed account actions before they can use the original account", async () => {
  vi.stubGlobal("location", { href: "https://os.example.test/?roleTest=nonce" });
  const fetch = vi.fn(); vi.stubGlobal("fetch", fetch);
  vi.stubGlobal("PublicKeyCredential", { parseCreationOptionsFromJSON: vi.fn(), parseRequestOptionsFromJSON: vi.fn() });
  const config = { identityApiBaseUrl: "https://identity.example.test" } as import("../../src/cluster/config").OsRuntimeConfig;
  const { nativeIdentity } = await import("../../src/auth/nativeIdentity");
  const { probeSession, refreshAccessCredential, exchangeCode, logout } = await import("../../src/auth/identityClient");
  const { registerPasskey, loginWithPasskey } = await import("../../src/auth/passkeys");
  for (const attempt of [
    () => nativeIdentity(config, "/account"),
    () => probeSession(config, fetch),
    () => refreshAccessCredential(config, fetch),
    () => exchangeCode(config, { code: "code", codeVerifier: "verifier", redirectUri: "uri" }, fetch),
    () => logout(config, fetch),
    () => registerPasskey(config, "Bearer test", "test"),
    () => loginWithPasskey(config, {}),
  ]) await expect(attempt()).rejects.toThrow();
  expect(fetch).not.toHaveBeenCalled();
});


it("keeps the test boundary when same-document navigation changes the URL", async () => {
  vi.stubGlobal("window", {});
  vi.stubGlobal("location", { href: "https://os.example.test/?roleTest=nonce" });
  const { isRoleTestWindow } = await import("@znasllc-io/memql-sdk-core/identityadmin");
  expect(isRoleTestWindow()).toBe(true);
  vi.stubGlobal("location", { href: "https://os.example.test/identity/me" });
  expect(isRoleTestWindow()).toBe(true);
  const fetch = vi.fn(); vi.stubGlobal("fetch", fetch);
  const { nativeIdentity } = await import("../../src/auth/nativeIdentity");
  await expect(nativeIdentity({} as import("../../src/cluster/config").OsRuntimeConfig, "/me")).rejects.toThrow("Account security");
  expect(fetch).not.toHaveBeenCalled();
});
