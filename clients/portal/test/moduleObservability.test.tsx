// The module observability drill-in (memql#4192 -> memql#4208): the metric
// windows come from ONE prefix-scoped engine query, codeMetricsInWindow,
// walked to exhaustion through its keyset cursor -- no client-side row-walk
// cap, no coverage footer -- and a module with no join keys issues no query
// at all and says so.

import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Result, type Connection, type QueryCallOptions } from "@znasllc-io/memql-sdk-core/client";

import { AppRoutes } from "../src/app/routes";
import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { joinKeysOf, isJoinable, windowFor } from "../src/modules/useModuleObservability";
import { asQueryClient } from "./support/queryFake";

const AUTH_DISABLED_CLUSTER = {
  identityUrl: "",
  identityApiBaseUrl: "",
  oauthClientId: "",
  authEnabled: false,
};

function moduleWire(fqnPrefixes: string[], codeReference: string) {
  return {
    kind: "integration",
    name: "email",
    description: "Outbound mail.",
    state: "active",
    stateDetail: "",
    scope: "node",
    fqnPrefixes,
    codeReference,
  };
}

function detailReply(fqnPrefixes: string[], codeReference: string) {
  return {
    moduleDetailResult: {
      module: moduleWire(fqnPrefixes, codeReference),
      envVars: [],
      reportingNodeId: "bff-test",
      reportingNodeType: "bff",
    },
  };
}

function metricNode(codeReference: string, windowStart: string, callCount: number, errorCount: number) {
  return {
    id: `v1:observability:codeMetric:${codeReference}:${windowStart}`,
    concept: "v1:observability:codeMetric",
    createdAt: windowStart,
    payload: {
      codeReference,
      windowStart,
      windowEnd: windowStart,
      bucket: "1h",
      callCount,
      errorCount,
      p95DurationNs: 2_500_000,
    },
  };
}

// One page of the named query's reply, with the keyset cursor the engine
// mints on a full page (empty when the set is exhausted).
function page(nodes: ReturnType<typeof metricNode>[], cursor: string): Result {
  return new Result({ bundle: { nodes }, meta: { cursor } });
}

interface NamedCall {
  name: string;
  call: string;
  opts: QueryCallOptions;
}

function fakeConnection(
  detail: ReturnType<typeof detailReply>,
  executeNamed: (name: string, call: string, opts: QueryCallOptions) => Promise<Result>,
): Connection {
  const query = asQueryClient({
    listConcepts: vi.fn(async () => []),
    getMyAccess: vi.fn(async () => ({
      userId: "user-1",
      primaryEmail: "op@example.test",
      clusterRole: "admin",
    })),
    // Every named read -- the typed codeMetricsInWindow method AND the
    // generic browse the recent-invocations table still goes through
    // (browseConceptPage dispatches as "conceptBrowse") -- lands here.
    executeNamed: vi.fn(executeNamed),
  });
  return {
    nodeId: "bff-test",
    serverVersion: "0.0.0-test",
    query,
    dispatcher: {
      send: vi.fn(),
      addEventListener: vi.fn(() => () => {}),
      registerStream: vi.fn(() => () => {}),
      sendAndWait: vi.fn(async (msg: Record<string, unknown>) => {
        if ("moduleDetail" in msg) return { correlateTo: "x", ...detail };
        return { correlateTo: "x" };
      }),
    },
    close: vi.fn(),
    done: vi.fn(() => new Promise<void>(() => {})),
  } as unknown as Connection;
}

function renderDetail(
  detail: ReturnType<typeof detailReply>,
  pages: Result[],
): NamedCall[] {
  const calls: NamedCall[] = [];
  const executeNamed = async (name: string, call: string, opts: QueryCallOptions) => {
    calls.push({ name, call, opts });
    if (name !== "codeMetricsInWindow") {
      // The recent-invocations browse: an empty newest page is enough here.
      return new Result({ bundle: { nodes: [] }, meta: { cursor: "" } });
    }
    const next = pages.shift();
    if (!next) throw new Error(`unexpected extra page request: ${call}`);
    return next;
  };
  const dial = vi.fn(async () => fakeConnection(detail, executeNamed)) as unknown as typeof Connection.dial;
  render(
    <MemoryRouter initialEntries={["/modules/integration/email"]}>
      <AuthProvider
        config={AUTH_DISABLED_CLUSTER}
        fetchImpl={async () => {
          throw new Error("observability tests must make no identity calls");
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
  return calls;
}

describe("the window the drill-in asks for", () => {
  it("is aligned to the bucket, half-open, and spelled at second precision", () => {
    // 2030-01-01T10:17:42.250Z
    const now = Date.UTC(2030, 0, 1, 10, 17, 42, 250);
    const hourly = windowFor("1h", now);
    expect(hourly.end).toBe("2030-01-01T11:00:00Z");
    expect(hourly.start).toBe("2029-12-25T11:00:00Z");
    expect(hourly.buckets).toBe(168);
    const minutely = windowFor("1m", now);
    expect(minutely.end).toBe("2030-01-01T10:18:00Z");
    expect(minutely.start).toBe("2030-01-01T09:18:00Z");
    expect(minutely.buckets).toBe(60);
  });

  it("drops blank prefixes and calls a module with none unmapped", () => {
    expect(joinKeysOf(["", " ", "integration.email."], " ").prefixes).toEqual(["integration.email."]);
    expect(isJoinable(joinKeysOf(["", " "], ""))).toBe(false);
    expect(isJoinable(joinKeysOf([], "method:x"))).toBe(true);
  });
});

describe("the module observability section", () => {
  it("issues one prefix-scoped query and renders the whole window without a coverage footer", async () => {
    const calls = renderDetail(detailReply(["integration.email."], ""), [
      page(
        [
          metricNode("integration.email.send", "2030-01-01T08:00:00Z", 40, 2),
          metricNode("integration.email.send", "2030-01-01T09:00:00Z", 60, 0),
        ],
        "",
      ),
    ]);

    await waitFor(() => expect(screen.getByText("100")).toBeTruthy());
    expect(screen.getByText("2.0%")).toBeTruthy();

    const metricCalls = calls.filter((c) => c.name === "codeMetricsInWindow");
    expect(metricCalls).toHaveLength(1);
    const call = metricCalls[0]!.call;
    expect(call.startsWith("query codeMetricsInWindow(")).toBe(true);
    expect(call).toContain('prefixes: ["integration.email."]');
    expect(call).toContain('bucket: "1h"');
    expect(call).toMatch(/windowStart: "\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z"/);
    expect(call).toMatch(/windowEnd: "\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z"/);
    expect(call).not.toContain("codeReference:");

    // The cap is gone and so is the sentence that described it.
    expect(screen.queryByText(/aggregate rows/)).toBeNull();
    expect(screen.queryByText(/beyond the cap/)).toBeNull();
    expect(screen.getByText(/the last 7 days/)).toBeTruthy();
  });

  it("walks the keyset cursor to exhaustion and carries the exact codeReference as a second key", async () => {
    const calls = renderDetail(detailReply(["integration.email."], "method:x.Send"), [
      page([metricNode("integration.email.send", "2030-01-01T08:00:00Z", 10, 0)], "cursor-2"),
      page([metricNode("method:x.Send", "2030-01-01T09:00:00Z", 5, 1)], ""),
    ]);

    await waitFor(() => expect(screen.getByText("15")).toBeTruthy());

    const metricCalls = calls.filter((c) => c.name === "codeMetricsInWindow");
    expect(metricCalls).toHaveLength(2);
    expect(metricCalls[0]!.opts.cursor).toBeUndefined();
    expect(metricCalls[1]!.opts.cursor).toBe("cursor-2");
    expect(metricCalls[1]!.call).toContain('codeReference: "method:x.Send"');
    expect(metricCalls[1]!.call).toContain('prefixes: ["integration.email."]');
  });

  it("states the unmapped case and issues no metric query for a module with no join keys", async () => {
    const calls = renderDetail(detailReply([], ""), []);

    await waitFor(() =>
      expect(screen.getByText(/No code reference mapped for this module/)).toBeTruthy(),
    );
    expect(calls.filter((c) => c.name === "codeMetricsInWindow")).toHaveLength(0);
  });
});
