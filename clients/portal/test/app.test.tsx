// The app shell + routing + connection state, driven against a fake dial.
//
// The fake is the SDK's own Connection.dial signature, so this exercises the
// real ClusterProvider state machine (connecting -> connected, the
// error branch, the query handoff) without a server. What it deliberately
// does NOT do is fake the QueryClient's protocol -- listConcepts is stubbed
// at the method boundary, because the wire is sdk/ts's business and is tested
// there.

import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { Concept, Connection, QueryClient } from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";

const CONCEPTS: Concept[] = [
  {
    id: "v1:cluster:node",
    version: "v1",
    domain: "cluster",
    entity: "node",
    description: "A registered node in the cluster",
    type: "concept",
    displayCard: { primary: "name" },
  },
];

// fakeConnection builds the narrow slice of Connection the provider touches.
// Cast at the boundary rather than implementing the whole class: widening the
// fake every time an unrelated method is added to Connection would make this
// test a maintenance tax with no extra coverage.
function fakeConnection(overrides: Partial<Connection> = {}): Connection {
  const query = {
    listConcepts: vi.fn(async () => CONCEPTS),
  } as unknown as QueryClient;
  return {
    nodeId: "bff-test",
    serverVersion: "0.0.0-test",
    query,
    close: vi.fn(),
    done: vi.fn(() => new Promise<void>(() => {})), // never settles
    ...overrides,
  } as unknown as Connection;
}

// The routes now sit behind RequireAuth (memql#3315), so a shell test needs a
// session to exist. It is supplied by declaring the cluster auth-disabled --
// the one configuration in which there is genuinely nothing to sign in to, so
// the shell renders with no credential machinery involved at all. That keeps
// these tests about the shell; the sign-in flow itself is authFlow.test.tsx's
// subject.
const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
};

function renderApp(dial: typeof Connection.dial, path = "/concepts") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("the shell tests must make no identity calls");
        }}
        storage={null}
        navigate={() => {}}
        redirectUri="https://cockpit.example.com/portal/auth/callback"
      >
        <ClusterProvider dial={dial}>
          <AppRoutes />
        </ClusterProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

describe("the portal shell", () => {
  it("reports connected and lists the cluster's concepts", async () => {
    const dial = vi.fn(async () => fakeConnection()) as unknown as typeof Connection.dial;
    renderApp(dial);

    await waitFor(() => expect(screen.getByText("memQL Portal")).toBeTruthy());
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("Connected"));
    await waitFor(() => expect(screen.getByText("v1:cluster:node")).toBeTruthy());
  });

  it("surfaces a dial failure instead of showing an empty page", async () => {
    const dial = vi.fn(async () => {
      throw new Error("websocket closed before open: code=1006");
    }) as unknown as typeof Connection.dial;
    renderApp(dial);

    await waitFor(() =>
      expect(screen.getByRole("status").textContent).toBe("Connection failed"),
    );
    expect(screen.getByText(/websocket closed before open/)).toBeTruthy();
    // A page that cannot reach the cluster must say so rather than rendering
    // an empty concept list that reads as "this cluster has no concepts".
    expect(screen.getByText(/Not connected to a cluster/)).toBeTruthy();
  });

  it("routes an unknown path to the client-side 404 (the SPA fallback's other half)", async () => {
    const dial = vi.fn(async () => fakeConnection()) as unknown as typeof Connection.dial;
    renderApp(dial, "/nope");
    await waitFor(() => expect(screen.getByText("Not found")).toBeTruthy());
  });

  it("redirects the index to /concepts", async () => {
    const dial = vi.fn(async () => fakeConnection()) as unknown as typeof Connection.dial;
    renderApp(dial, "/");
    await waitFor(() => expect(screen.getByText("Concepts")).toBeTruthy());
  });
});
