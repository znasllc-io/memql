import { StrictMode } from "react";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../../src/app/App";
import { loginWithPasskey } from "../../src/auth/passkeys";
import type { OsAuthSource } from "../../src/auth/source";

const h = vi.hoisted(() => ({ sources: [] as OsAuthSource[] }));
// The full entry flow is real; cluster readiness gets its own deferred-read
// coverage in coreGate.test. Do not open a WebSocket in this transport test.
vi.mock("../../src/chrome/Shell", () => ({ Shell: ({ onSignOut, authSource }: { onSignOut: () => void; authSource: OsAuthSource }) => {
  h.sources.push(authSource);
  return <main><h1>Cluster destination</h1><button onClick={onSignOut}>Sign out</button></main>;
} }));
vi.mock("../../src/auth/passkeys", () => ({ loginWithPasskey: vi.fn(), registerPasskey: vi.fn() }));
vi.mock("../../src/auth/pkce", () => ({ generateCodeVerifier: async () => "test-verifier", challengeFor: async () => "test-challenge", generateState: () => "test-state" }));

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}
const json = (data: unknown, status = 200) => new Response(JSON.stringify(data), { status });
const login = () => json({ page: "login", csrf: "csrf", data: {
  Local: true, Stage: "email", AuthorizeMode: true, ClientID: "os",
  RedirectURI: `${location.origin}/auth/callback`, OAuthState: "test-state",
  CodeChallenge: "test-challenge", CodeChallengeMethod: "S256",
} });

function transport(signedIn = true) {
  const authorization = deferred<Response>();
  const token = deferred<Response>();
  const logout = deferred<Response>();
  const fetcher = vi.fn(async (input: string | URL, init?: RequestInit) => {
    const url = new URL(String(input), location.origin);
    if (url.pathname.endsWith("runtime-config.json")) return json({ identityUrl: "https://identity.example.test", oauthClientId: "os", authEnabled: true, domain: "example.test" });
    if (url.pathname === "/auth/setup/state") return json({ state: "claimed" });
    if (url.pathname === "/auth/refresh") return json({ access_token: "test-bearer", expires_in: 300 }, signedIn ? 200 : 401);
    if (url.pathname === "/auth/logout") { signedIn = false; return logout.promise; }
    if (url.pathname === "/authorize") return (await authorization.promise).clone();
    if (url.pathname === "/oauth/token") { signedIn = true; return token.promise; }
    throw new Error(`Unexpected request: ${url.pathname} ${init?.method}`);
  });
  vi.stubGlobal("fetch", fetcher);
  return { fetcher, authorization, token, logout };
}

beforeEach(() => {
  history.replaceState({}, "", "/");
  localStorage.clear();
  h.sources = [];
  vi.mocked(loginWithPasskey).mockReset();
  vi.mocked(loginWithPasskey).mockImplementation(async (_config, fields) => `${location.origin}/auth/callback?code=test-code&state=${fields.state}`);
  Object.defineProperty(navigator, "locks", { configurable: true, value: { request: async (_name: string, _options: unknown, action: () => Promise<unknown>) => action() } });
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); history.replaceState({}, "", "/"); });

describe("sign-in and sign-out handoffs", () => {
  it("consumes a native authorization redirect once under StrictMode", async () => {
    const t = transport(false);
    render(<StrictMode><App /></StrictMode>);
    await act(async () => t.authorization.resolve(json({ redirect: `${location.origin}/auth/callback?code=sso-code&state=test-state` })));
    await waitFor(() => expect(t.fetcher.mock.calls.some(([url]) => String(url).endsWith("/oauth/token"))).toBe(true));
    expect(screen.queryByRole("heading")).toBeNull();
    await act(async () => t.token.resolve(json({})));
    expect(await screen.findByRole("heading", { name: "Cluster destination" })).toBeTruthy();
    expect(t.fetcher.mock.calls.filter(([url]) => String(url).endsWith("/oauth/token"))).toHaveLength(1);
  });

  it("keeps one quiet entry state through delayed logout, authorization and code exchange", async () => {
    const t = transport();
    render(<StrictMode><App /></StrictMode>);
    fireEvent.click(await screen.findByRole("button", { name: "Sign out" }));
    const previousSource = h.sources.at(-1);
    expect(screen.queryByRole("heading")).toBeNull();
    expect(screen.getByRole("status")).toBeTruthy();
    expect(screen.getAllByRole("group", { name: "Color theme" })).toHaveLength(1);
    await act(async () => t.logout.resolve(json({})));
    await waitFor(() => expect(location.pathname).toBe("/identity/authorize"));
    expect(screen.queryByRole("heading")).toBeNull();
    expect(screen.queryByText("Identity could not be opened.")).toBeNull();
    await act(async () => t.authorization.resolve(login()));
    fireEvent.click(await screen.findByRole("button", { name: "Sign in with a passkey" }));
    await waitFor(() => expect(t.fetcher.mock.calls.some(([url]) => String(url).endsWith("/oauth/token"))).toBe(true));
    expect(screen.queryByRole("heading")).toBeNull();
    expect(screen.getByRole("status").textContent).toBe("Completing sign-in");
    expect(location.pathname).toBe("/identity/authorize");
    const exchange = t.fetcher.mock.calls.find(([url]) => String(url).endsWith("/oauth/token"))!;
    expect(JSON.parse(String(exchange[1]?.body))).toMatchObject({ code: "test-code", code_verifier: "test-verifier", redirect_uri: `${location.origin}/auth/callback` });
    await act(async () => t.token.resolve(json({})));
    expect(await screen.findByRole("heading", { name: "Cluster destination" })).toBeTruthy();
    expect(location.pathname).toBe("/");
    expect(h.sources.at(-1)).not.toBe(previousSource);
    expect(screen.queryByRole("status")).toBeNull();
    expect(t.fetcher.mock.calls.filter(([url]) => String(url).endsWith("/oauth/token"))).toHaveLength(1);
  });

  it.each(["wrong-state", "refused-code", "unavailable", "network"])("shows retry instead of entering the cluster after %s", async failure => {
    const t = transport(false);
    if (failure === "wrong-state") vi.mocked(loginWithPasskey).mockResolvedValue(`${location.origin}/auth/callback?code=test-code&state=wrong`);
    render(<App />);
    await act(async () => t.authorization.resolve(login()));
    fireEvent.click(await screen.findByRole("button", { name: "Sign in with a passkey" }));
    if (failure !== "wrong-state") {
      await waitFor(() => expect(t.fetcher.mock.calls.some(([url]) => String(url).endsWith("/oauth/token"))).toBe(true));
      await act(async () => {
        if (failure === "network") t.token.reject(new TypeError("Failed to fetch"));
        else t.token.resolve(json({}, failure === "unavailable" ? 503 : 400));
      });
    }
    expect(await screen.findByRole("button", { name: "Retry" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Cluster destination" })).toBeNull();
    expect(screen.queryByRole("status")).toBeNull();
    expect(location.pathname).toBe("/");
    if (failure === "wrong-state") expect(t.fetcher.mock.calls.some(([url]) => String(url).endsWith("/oauth/token"))).toBe(false);
  });
});
