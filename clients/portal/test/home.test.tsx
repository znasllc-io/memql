// The console home (memql#4182): / renders the landing surface (the old
// /concepts redirect is gone), tiles skeleton-load and settle into honest
// counts (a full page renders with a trailing plus), and the audit tile is
// LIVE -- a CDC event re-reads it without any poll.

import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, act } from "@testing-library/react";
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

function auditRow(n: number) {
  return {
    id: `v1:identity:auditEvent:evt-${n}`,
    concept: "v1:identity:auditEvent",
    createdAt: `2026-08-20T2${n}:00:00Z`,
    payload: { action: `audit_action_${n}`, category: "admin", outcome: "ok" },
  };
}

describe("the console home", () => {
  it("renders at /, counts honestly, and the audit tile ticks live", async () => {
    let auditCount = 1;
    const graphHandlers = new Map<string, () => void>();
    const query = {
      listConcepts: vi.fn(async () => []),
      getMyAccess: vi.fn(async () => ({
        userId: "user-1",
        primaryEmail: "op@example.test",
        clusterRole: "reader",
      })),
      executeNamed: vi.fn(async (_name: string, call: string) => {
        let rows: unknown[] = [];
        if (call.includes("v1:identity:auditEvent")) {
          rows = Array.from({ length: auditCount }, (_, i) => auditRow(i));
        } else if (call.includes("v1:identity:user")) {
          rows = [auditRow(0)];
        }
        return { rawNodes: () => rows, meta: () => ({ cursor: "" }) };
      }),
    } as unknown as QueryClient;
    const conn = {
      nodeId: "bff-test",
      serverVersion: "0.0.0-test",
      query,
      subscriptions: {
        subscribeGraph: vi.fn((handler: () => void, opts: { concept: string }) => {
          graphHandlers.set(opts.concept, handler);
          return () => {};
        }),
      },
      close: vi.fn(),
      done: vi.fn(() => new Promise<void>(() => {})),
    } as unknown as Connection;
    const dial = vi.fn(async () => conn) as unknown as typeof Connection.dial;

    render(
      <MemoryRouter initialEntries={["/"]}>
        <AuthProvider
          config={AUTH_DISABLED_CLUSTER}
          fetchImpl={async () => {
            throw new Error("home tests must make no identity calls");
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

    // The home renders in place of the old redirect, with the tile labels.
    await waitFor(() => expect(screen.getByText("audit events")).toBeTruthy());
    await waitFor(() => expect(screen.getByText("audit_action_0")).toBeTruthy());

    // A CDC arrival on the audit concept re-reads the tile: live, no poll.
    auditCount = 2;
    const bump = graphHandlers.get("v1:identity:auditEvent");
    expect(bump).toBeTruthy();
    await act(async () => {
      bump!();
    });
    await waitFor(() => expect(screen.getByText("audit_action_1")).toBeTruthy());
  });
});
