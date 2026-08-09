// The sign-in flow as an operator experiences it (memql#3315).
//
// Driven through the real AuthProvider + route table with a fake identity
// service, fake storage and a fake navigation, so the assertions are about
// behaviour ("the deep link comes back") rather than about which functions
// were called.

import { webcrypto } from "node:crypto";
import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { Connection, QueryClient } from "@znasllc-io/memql-sdk-core/client";

import { AuthenticatedCluster } from "../src/app/App";
import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import type { CryptoLike } from "../src/auth/pkce";
import type { StorageLike } from "../src/auth/pending";
import type { PortalRuntimeConfig } from "../src/cluster/config";

const nodeCrypto = webcrypto as unknown as CryptoLike;

const CONFIG: PortalRuntimeConfig = {
  identityUrl: "https://identity.example.com",
  identityApiBaseUrl: "https://identity.example.com",
  oauthClientId: "portal",
  authEnabled: true,
};

const REDIRECT_URI = "https://cockpit.example.com/portal/auth/callback";
const DEEP_LINK = "/concepts/v1:cluster:node";

function memoryStorage(): StorageLike & { map: Map<string, string> } {
  const map = new Map<string, string>();
  return {
    map,
    getItem: (k) => map.get(k) ?? null,
    setItem: (k, v) => void map.set(k, v),
    removeItem: (k) => void map.delete(k),
  };
}

function jsonResponse(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  } as unknown as Response;
}

// fakeDial stands in for the SDK's dial. The cluster wire is sdk/ts's business
// and is tested there; here it only needs to answer "connected".
function fakeDial(): typeof Connection.dial {
  const query = {
    listConcepts: vi.fn(async () => []),
    getMyAccess: vi.fn(async () => ({
      requestId: "r1",
      userId: "u-42",
      primaryEmail: "ops@example.com",
      clusterRole: "owner" as const,
    })),
  } as unknown as QueryClient;
  return vi.fn(
    async () =>
      ({
        nodeId: "bff-1",
        serverVersion: "0.0.0-test",
        query,
        close: vi.fn(),
        done: vi.fn(() => new Promise<void>(() => {})),
      }) as unknown as Connection,
  ) as unknown as typeof Connection.dial;
}

interface HarnessOptions {
  path?: string;
  config?: PortalRuntimeConfig;
  fetchImpl: (url: string, init?: RequestInit) => Promise<Response>;
  storage?: StorageLike;
  navigate?: (url: string) => void;
}

function renderPortal(opts: HarnessOptions) {
  const storage = opts.storage ?? memoryStorage();
  return {
    storage,
    ...render(
      <MemoryRouter initialEntries={[opts.path ?? "/"]}>
        <AuthProvider
          config={opts.config ?? CONFIG}
          fetchImpl={opts.fetchImpl}
          storage={storage}
          cryptoImpl={nodeCrypto}
          navigate={opts.navigate ?? (() => {})}
          redirectUri={REDIRECT_URI}
        >
          <AuthenticatedCluster dial={fakeDial()}>
            <AppRoutes />
          </AuthenticatedCluster>
        </AuthProvider>
      </MemoryRouter>,
    ),
  };
}

// unauthenticated is the cold-load-while-signed-out response: identity has no
// refresh cookie to rotate. A 401 here is a NORMAL outcome, not an error.
const unauthenticated = () =>
  jsonResponse({ error: "invalid_grant", message: "no refresh token presented" }, 401);

describe("route protection", () => {
  it("shows the sign-in view instead of the requested route when signed out", async () => {
    renderPortal({ path: DEEP_LINK, fetchImpl: async () => unauthenticated() });
    await waitFor(() => expect(screen.getByRole("button", { name: /Continue with/ })).toBeTruthy());
    // The protected content is not merely hidden -- it never rendered.
    expect(screen.queryByText("Concepts")).toBeNull();
  });

  it("does not flash the sign-in view while the session is still being probed", () => {
    // "not known yet" and "signed out" must not look the same: flashing a
    // sign-in page at someone who IS signed in teaches them to distrust it.
    renderPortal({ path: DEEP_LINK, fetchImpl: () => new Promise<Response>(() => {}) });
    expect(screen.queryByRole("button", { name: /Continue with/ })).toBeNull();
    expect(screen.getByText(/Loading your session/)).toBeTruthy();
  });

  it("renders the shell once a session is restored from the refresh cookie", async () => {
    const fetchImpl = vi.fn(async () => jsonResponse({ access_token: "AT-1", expires_in: 900 }));
    renderPortal({ path: "/concepts", fetchImpl });
    await waitFor(() => expect(screen.getByText("Concepts")).toBeTruthy());
    expect(screen.queryByRole("button", { name: /Continue with/ })).toBeNull();
  });

  it("renders the shell, and says so, on a cluster with auth disabled", async () => {
    const fetchImpl = vi.fn(async () => {
      throw new Error("no identity call should be made when auth is disabled");
    });
    renderPortal({
      path: "/concepts",
      config: { ...CONFIG, authEnabled: false },
      fetchImpl,
    });
    await waitFor(() => expect(screen.getByText("Authentication disabled")).toBeTruthy());
    // An operator must not be able to mistake "every stream is admitted as the
    // local-dev cluster owner" for "I authenticated".
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it("explains itself rather than looping when the cluster published no identity service", async () => {
    renderPortal({
      path: "/concepts",
      config: { ...CONFIG, identityUrl: "", oauthClientId: "" },
      fetchImpl: async () => unauthenticated(),
    });
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toMatch(/not configured|published no identity/),
    );
    expect(screen.queryByRole("button", { name: /Continue with/ })).toBeNull();
  });
});

describe("signing in", () => {
  it("sends the browser to identity with a PKCE-bound authorization request", async () => {
    const navigated: string[] = [];
    const { storage } = renderPortal({
      path: DEEP_LINK,
      fetchImpl: async () => unauthenticated(),
      navigate: (url) => navigated.push(url),
    });

    await waitFor(() => screen.getByRole("button", { name: /Continue with/ }));
    fireEvent.click(screen.getByRole("button", { name: /Continue with/ }));
    await waitFor(() => expect(navigated).toHaveLength(1));

    const url = new URL(navigated[0]!);
    expect(url.origin + url.pathname).toBe("https://identity.example.com/authorize");
    expect(url.searchParams.get("code_challenge_method")).toBe("S256");
    expect(url.searchParams.get("redirect_uri")).toBe(REDIRECT_URI);

    // The verifier stays here; only its hash travelled.
    const pending = JSON.parse(storage.getItem("memql-portal-pending-auth")!) as {
      verifier: string;
      state: string;
      returnTo: string;
    };
    expect(url.toString()).not.toContain(pending.verifier);
    expect(url.searchParams.get("state")).toBe(pending.state);
    // The deep link is remembered across a navigation that destroys the page.
    expect(pending.returnTo).toBe(DEEP_LINK);
  });

  it("refuses to redirect when storage is blocked, instead of looping forever", async () => {
    const navigated: string[] = [];
    const blocked: StorageLike = {
      getItem: () => null,
      setItem: () => {
        throw new DOMException("QuotaExceededError");
      },
      removeItem: () => {},
    };
    renderPortal({
      path: DEEP_LINK,
      fetchImpl: async () => unauthenticated(),
      storage: blocked,
      navigate: (url) => navigated.push(url),
    });

    await waitFor(() => screen.getByRole("button", { name: /Continue with/ }));
    fireEvent.click(screen.getByRole("button", { name: /Continue with/ }));

    await waitFor(() => expect(screen.getByRole("alert").textContent).toMatch(/session storage/));
    expect(navigated).toHaveLength(0);
  });
});

describe("the callback", () => {
  it("exchanges the code and lands on the originally requested deep link", async () => {
    const storage = memoryStorage();
    storage.setItem(
      "memql-portal-pending-auth",
      JSON.stringify({
        state: "STATE-1",
        verifier: "VERIFIER-1",
        returnTo: DEEP_LINK,
        createdAt: Date.now(),
      }),
    );

    const bodies: string[] = [];
    const fetchImpl = vi.fn(async (_url: string, init?: RequestInit) => {
      bodies.push(String(init?.body ?? ""));
      return jsonResponse({ access_token: "AT-1", expires_in: 900 });
    });

    renderPortal({
      path: "/auth/callback?code=AUTH-CODE&state=STATE-1",
      fetchImpl,
      storage,
    });

    // Landed on the deep link, signed in -- the round trip preserved the
    // destination (#3316's deep-linkable URLs must survive a login).
    await waitFor(() => expect(screen.getByText("v1:cluster:node")).toBeTruthy());

    const exchange = JSON.parse(bodies[0]!) as Record<string, string>;
    expect(exchange.grant_type).toBe("authorization_code");
    expect(exchange.code).toBe("AUTH-CODE");
    expect(exchange.code_verifier).toBe("VERIFIER-1");

    // Consumed: the code is single-use and identity audits a replay.
    expect(storage.map.size).toBe(0);
  });

  it("discards a response whose state does not match this tab's request", async () => {
    // Without this check an attacker can hand the portal an authorization code
    // of their own and sign the operator into the ATTACKER's account -- a much
    // quieter compromise than the reverse.
    const storage = memoryStorage();
    storage.setItem(
      "memql-portal-pending-auth",
      JSON.stringify({
        state: "STATE-1",
        verifier: "VERIFIER-1",
        returnTo: DEEP_LINK,
        createdAt: Date.now(),
      }),
    );
    const fetchImpl = vi.fn(async () => jsonResponse({ access_token: "AT-1", expires_in: 900 }));

    renderPortal({
      path: "/auth/callback?code=ATTACKER-CODE&state=WRONG-STATE",
      fetchImpl,
      storage,
    });

    await waitFor(() => expect(screen.getByRole("alert").textContent).toMatch(/state did not match/));
    // Crucially: no token exchange was attempted. (The cold-load session probe
    // against /auth/refresh is expected and unrelated.)
    const exchanges = fetchImpl.mock.calls.filter((call) =>
      String((call as unknown[])[0]).includes("/oauth/token"),
    );
    expect(exchanges).toHaveLength(0);
  });

  it("surfaces an error the identity service redirected back with", async () => {
    renderPortal({
      path: "/auth/callback?error=access_denied&error_description=You+are+not+on+the+allow+list",
      fetchImpl: async () => unauthenticated(),
    });
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toMatch(/not on the allow list/),
    );
  });

  it("reports an exchange rejected by identity instead of silently staying signed out", async () => {
    const storage = memoryStorage();
    storage.setItem(
      "memql-portal-pending-auth",
      JSON.stringify({
        state: "STATE-1",
        verifier: "VERIFIER-1",
        returnTo: DEEP_LINK,
        createdAt: Date.now(),
      }),
    );
    const fetchImpl = vi.fn(async () =>
      jsonResponse({ error: "invalid_grant", message: "auth code has already been used" }, 400),
    );

    renderPortal({ path: "/auth/callback?code=C&state=STATE-1", fetchImpl, storage });
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toMatch(/already been used/),
    );
  });
});

describe("the header", () => {
  it("names both the cluster and the signed-in identity", async () => {
    // The two questions an operations console must answer without a click.
    renderPortal({
      path: "/concepts",
      fetchImpl: async () => jsonResponse({ access_token: "AT-1", expires_in: 900 }),
    });
    // Identity, read from the CLUSTER (MyAccessMsg), not from the local token.
    await waitFor(() => expect(screen.getByText("ops@example.com")).toBeTruthy());
    expect(screen.getByText("owner")).toBeTruthy();
    // Cluster, and the replica serving this stream.
    expect(screen.getByText(globalThis.location.host)).toBeTruthy();
    expect(screen.getByTitle(/bff-1/)).toBeTruthy();
  });

  it("signs out locally without waiting for the server round trip", async () => {
    let logoutCalls = 0;
    const fetchImpl = vi.fn(async (url: string) => {
      if (url.endsWith("/auth/logout")) {
        logoutCalls++;
        return jsonResponse({}, 204);
      }
      return jsonResponse({ access_token: "AT-1", expires_in: 900 });
    });
    renderPortal({ path: "/concepts", fetchImpl });

    await waitFor(() => screen.getByRole("button", { name: "Sign out" }));
    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));

    await waitFor(() => expect(screen.getByRole("button", { name: /Continue with/ })).toBeTruthy());
    await waitFor(() => expect(logoutCalls).toBe(1));
  });
});
