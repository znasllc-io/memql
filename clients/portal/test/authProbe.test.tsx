// How often the portal probes for a session (memql#4327).
//
// The bootstrap probe calls POST /auth/refresh, and every call is a real
// refresh-token ROTATION server-side -- one row in the identity service's
// activity log per call. The effect's comment said "one unconditional request
// per cold load"; its dependency array said `location.pathname`, so it ran on
// every in-app navigation. Clicking a row in the Audit Trail wrote a row in the
// Audit Trail.
//
// These tests drive the real AuthProvider through real route changes and count
// requests, because the defect was invisible to anything that mounted the
// provider once.

import { webcrypto } from "node:crypto";
import type { ReactElement } from "react";
import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, useNavigate } from "react-router-dom";

import { AuthProvider, useAuth } from "../src/auth/AuthProvider";
import type { CryptoLike } from "../src/auth/pkce";
import type { StorageLike } from "../src/auth/pending";
import type { PortalRuntimeConfig } from "../src/cluster/config";

const nodeCrypto = webcrypto as unknown as CryptoLike;

const CONFIG: PortalRuntimeConfig = {
  identityUrl: "https://identity.example.com",
  identityApiBaseUrl: "https://identity.example.com",
  oauthClientId: "portal",
  authEnabled: true,
  domain: "",
};

function memoryStorage(): StorageLike {
  const map = new Map<string, string>();
  return {
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

// Probe renders the auth status plus the two controls the tests drive: an
// in-app navigation, and Sign out. Nothing here is portal chrome -- the point
// is to exercise AuthProvider against route changes without dragging the whole
// route table in.
function Probe(): ReactElement {
  const auth = useAuth();
  const navigate = useNavigate();
  return (
    <div>
      <span data-testid="status">{auth.status}</span>
      <button type="button" onClick={() => navigate("/concepts")}>
        go concepts
      </button>
      <button type="button" onClick={() => navigate("/modules")}>
        go modules
      </button>
      <button type="button" onClick={() => navigate("/sites")}>
        go sites
      </button>
      <button type="button" onClick={() => auth.signOut()}>
        sign out
      </button>
    </div>
  );
}

function renderProbe(fetchImpl: (url: string, init?: RequestInit) => Promise<Response>) {
  return render(
    <MemoryRouter initialEntries={["/"]}>
      <AuthProvider
        config={CONFIG}
        fetchImpl={fetchImpl}
        storage={memoryStorage()}
        cryptoImpl={nodeCrypto}
        navigate={() => {}}
        redirectUri="https://api.example.com/portal/auth/callback"
      >
        <Probe />
      </AuthProvider>
    </MemoryRouter>,
  );
}

describe("session probe cadence", () => {
  it("probes once per cold load, not once per route change", async () => {
    const refreshes: string[] = [];
    const fetchImpl = vi.fn(async (url: string) => {
      if (String(url).includes("/auth/refresh")) {
        refreshes.push(String(url));
        return jsonResponse({ access_token: "AT-1", expires_in: 900 });
      }
      throw new Error(`unexpected fetch ${url}`);
    });

    renderProbe(fetchImpl);
    await waitFor(() => expect(screen.getByTestId("status").textContent).toBe("signedIn"));
    expect(refreshes).toHaveLength(1);

    // Three in-app navigations. Each one re-runs the bootstrap effect under
    // the old dependency array.
    fireEvent.click(screen.getByText("go concepts"));
    fireEvent.click(screen.getByText("go modules"));
    fireEvent.click(screen.getByText("go sites"));

    // Give any (wrongly) re-armed effect a chance to settle before counting;
    // asserting immediately would pass against the defect too.
    await waitFor(() => expect(screen.getByTestId("status").textContent).toBe("signedIn"));
    await new Promise((r) => setTimeout(r, 20));

    expect(refreshes).toHaveLength(1);
  });

  it("probes again after a sign-out, because the session really is gone", async () => {
    const refreshes: string[] = [];
    const fetchImpl = vi.fn(async (url: string) => {
      const path = String(url);
      if (path.includes("/auth/refresh")) {
        refreshes.push(path);
        return jsonResponse({ access_token: `AT-${refreshes.length}`, expires_in: 900 });
      }
      if (path.includes("/auth/logout")) return jsonResponse({});
      throw new Error(`unexpected fetch ${path}`);
    });

    renderProbe(fetchImpl);
    await waitFor(() => expect(screen.getByTestId("status").textContent).toBe("signedIn"));
    expect(refreshes).toHaveLength(1);

    fireEvent.click(screen.getByText("sign out"));
    await waitFor(() => expect(screen.getByTestId("status").textContent).toBe("signedOut"));

    // A navigation after signing out must re-probe exactly once: the ref that
    // suppresses the per-route probe is reset by sign-out, so a session
    // established in another tab is picked up.
    fireEvent.click(screen.getByText("go concepts"));
    await waitFor(() => expect(refreshes).toHaveLength(2));
    await new Promise((r) => setTimeout(r, 20));
    expect(refreshes).toHaveLength(2);
  });

  it("reads the runtime config once, not once per route change", async () => {
    const configReads: string[] = [];
    const fetchImpl = vi.fn(async (url: string) => {
      const path = String(url);
      if (path.includes("runtime-config")) {
        configReads.push(path);
        return jsonResponse({
          identityUrl: "https://identity.example.com",
          identityApiBaseUrl: "https://identity.example.com",
          oauthClientId: "portal",
          authEnabled: true,
        });
      }
      if (path.includes("/auth/refresh")) {
        return jsonResponse({ access_token: "AT-1", expires_in: 900 });
      }
      throw new Error(`unexpected fetch ${path}`);
    });

    render(
      <MemoryRouter initialEntries={["/"]}>
        <AuthProvider
          fetchImpl={fetchImpl}
          storage={memoryStorage()}
          cryptoImpl={nodeCrypto}
          navigate={() => {}}
          redirectUri="https://api.example.com/portal/auth/callback"
        >
          <Probe />
        </AuthProvider>
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByTestId("status").textContent).toBe("signedIn"));
    const afterCold = configReads.length;

    fireEvent.click(screen.getByText("go concepts"));
    fireEvent.click(screen.getByText("go modules"));
    await new Promise((r) => setTimeout(r, 20));

    expect(configReads).toHaveLength(afterCold);
  });
});
