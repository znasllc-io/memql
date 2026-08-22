// The console (memql#4182, memql#4263): / renders the landing surface (the old
// /concepts redirect is gone), tiles settle into EXACT counts from the engine's
// `count` directive rather than the length of a page, and the audit tile is
// LIVE -- a CDC event re-reads it without any poll.
//
// The fake below answers two shapes per tile because the surface makes two
// reads: `count(concept==X)` for the number, and a bounded page only for the
// tiles that list recent rows underneath. That split is the fix -- counting a
// page capped every number at the page size and rendered "100+" forever.

import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, act } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { Connection} from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { asQueryClient } from "./support/queryFake";

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
  domain: "",
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
    const query = asQueryClient({
      listConcepts: vi.fn(async () => []),
      getMyAccess: vi.fn(async () => ({
        userId: "user-1",
        primaryEmail: "op@example.test",
        clusterRole: "reader",
      })),
      executeNamed: vi.fn(async (_name: string, call: string) => {
        // The count directive: answered with the aggregate envelope, never
        // with rows. 137 is deliberately past any page size -- a surface that
        // went back to counting a page would render "100+" and fail here.
        if (call.startsWith("count(")) {
          const total = call.includes("v1:identity:auditEvent")
            ? auditCount
            : call.includes("v1:identity:user")
              ? 137
              : 0;
          return {
            rawNodes: () => [],
            single: () => ({ count: total }),
            meta: () => ({ count: total }),
          };
        }
        let rows: unknown[] = [];
        if (call.includes("v1:identity:auditEvent")) {
          rows = Array.from({ length: auditCount }, (_, i) => auditRow(i));
        } else if (call.includes("v1:identity:user")) {
          rows = [auditRow(0)];
        }
        return { rawNodes: () => rows, meta: () => ({ cursor: "" }) };
      }),
    });
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

    // The console renders in place of the old redirect, with a tile per
    // population the rail offers -- customers included, which was missing.
    await waitFor(() => expect(screen.getByText("audit events")).toBeTruthy());
    for (const label of ["people", "agents", "customers", "sites", "deployments"]) {
      expect(screen.getByText(label)).toBeTruthy();
    }
    await waitFor(() => expect(screen.getByText("audit_action_0")).toBeTruthy());

    // The number is the engine's count, not the length of a page: 137 is past
    // any page size this surface ever fetched.
    await waitFor(() => expect(screen.getByText("137")).toBeTruthy());
    expect(screen.queryByText("137+")).toBeNull();

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
