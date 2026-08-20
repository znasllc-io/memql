// Cluster operations (memql#4193): courtesy gating, the timeline rendered
// from the graph's own deployment records, rollback staying owner-only, and
// the honest repair statement -- the surface must say the verb is not
// exposed rather than fake a control.

import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { Connection, QueryClient } from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
};

const DEPLOYMENT_ROWS = [
  {
    id: "v1:cluster:deployment:dep-2",
    concept: "v1:cluster:deployment",
    createdAt: "2026-08-20T21:00:00Z",
    payload: { deploymentId: "dep-2", status: "succeeded", engineVersion: "v0.19.1" },
  },
  {
    id: "v1:cluster:deployment:dep-1",
    concept: "v1:cluster:deployment",
    createdAt: "2026-08-19T21:00:00Z",
    payload: { deploymentId: "dep-1", status: "succeeded", engineVersion: "v0.19.0" },
  },
];

function fakeConnection(role: string, sent: Array<Record<string, unknown>>): Connection {
  const query = {
    listConcepts: vi.fn(async () => []),
    getMyAccess: vi.fn(async () => ({
      userId: "user-1",
      primaryEmail: "op@example.test",
      clusterRole: role,
    })),
    executeNamed: vi.fn(async (name: string, call: string) => {
      sent.push({ executeNamed: call });
      const rows = call.includes("v1:cluster:deployment,") || call.includes("v1:cluster:deployment ")
        ? DEPLOYMENT_ROWS
        : call.includes("deploymentNodeSpec")
          ? []
          : [];
      return {
        rawNodes: () => rows,
        meta: () => ({ cursor: "" }),
      };
    }),
  } as unknown as QueryClient;
  const dispatcher = {
    send: vi.fn(),
    addEventListener: vi.fn(() => () => {}),
    registerStream: vi.fn(() => () => {}),
    sendAndWait: vi.fn(async (msg: Record<string, unknown>) => {
      sent.push(msg);
      if ("deployControl" in msg) {
        return {
          correlateTo: "x",
          deployControlResult: {
            getDeploymentStatus: {
              version: "v0.19.1",
              engineVersion: "v0.19.1",
              syncStatus: "Synced",
              healthStatus: "Healthy",
              components: [],
            },
          },
        };
      }
      return { correlateTo: "x" };
    }),
  };
  return {
    nodeId: "bff-test",
    serverVersion: "0.0.0-test",
    query,
    dispatcher,
    subscriptions: { subscribeGraph: vi.fn(() => () => {}) },
    close: vi.fn(),
    done: vi.fn(() => new Promise<void>(() => {})),
  } as unknown as Connection;
}

function renderOps(role: string, sent: Array<Record<string, unknown>> = []) {
  const dial = vi.fn(async () => fakeConnection(role, sent)) as unknown as typeof Connection.dial;
  render(
    <MemoryRouter initialEntries={["/cluster-ops"]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("cluster-ops tests must make no identity calls");
        }}
        storage={null}
        navigate={() => {}}
        redirectUri="https://api.example.com/portal/auth/callback"
      >
        <ClusterProvider dial={dial}>
          <AppRoutes />
        </ClusterProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
  return sent;
}

describe("cluster operations", () => {
  it("offers nothing below the operator roles", async () => {
    renderOps("reader");
    await waitFor(() => expect(screen.getByText("This is an operator surface")).toBeTruthy());
  });

  it("renders the timeline from the deployment records and says repair is not exposed", async () => {
    renderOps("admin");
    await waitFor(() => expect(screen.getByText("v0.19.0")).toBeTruthy());
    expect(screen.getByText(/no repair verb/)).toBeTruthy();
    // Admin can ship but not roll back: the rollback control never renders.
    expect(screen.queryByRole("button", { name: /Roll back to this/ })).toBeNull();
  });

  it("owner rolls back through a dialog that states the gate", async () => {
    const sent = renderOps("owner");
    await waitFor(() => expect(screen.getByText("v0.19.0")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Roll back to this" }));
    await waitFor(() => expect(screen.getByRole("dialog")).toBeTruthy());
    expect(screen.getByRole("dialog").textContent).toContain("Owner-only");
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Roll back" }));
    await waitFor(() =>
      expect(
        sent.some(
          (m) =>
            "deployControl" in m &&
            "rollbackDeployment" in (m["deployControl"] as Record<string, unknown>),
        ),
      ).toBe(true),
    );
  });
});
