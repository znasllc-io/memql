import type { ReactNode } from "react";
import { vi } from "vitest";
import { QueryClient, Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { OsProvider } from "../../src/chrome/state";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";
import { OS_REGISTRY } from "../../src/apps/registry";
import {
  DEFAULT_STORES_SETTINGS,
  type StoresSettings,
  type StoresSettingsStore,
} from "../../src/apps/stores/settings";

// The Stores app's test harness (epic memql#5009 / memql#5012).
//
// ===========================================================================
// THE FAKE SITS UNDER `executeNamed`, NOT OVER THE GENERATED METHODS
// ===========================================================================
// The Files, Deployables and Logs harnesses' rule, kept for the reason their
// headers state: a double that stubs `query.createStore` records ARGUMENTS
// and never renders the call, so the generated builder -- the thing that
// turns those arguments into the MemQL text the engine has to parse -- would
// run in production and nowhere else. The stub answers at the funnel, so
// every test exercises the real builders and can assert on the STRING that
// reached the wire.
//
// ===========================================================================
// A BUILTIN'S REPLY IS AN id-KEYED NODE MAP, NOT A ROW SET
// ===========================================================================
// `shopifyStoreHealth` is a top-level builtin, and one does not answer rows:
// the engine returns the handler's node map and marshals it as ONE value
// keyed by node id. A harness answering flat rows here would be green against
// a shape the engine never sends -- which is exactly how the Logs app once
// shipped rendering nothing at all.

/** What a top-level `builtin X(...)` answers ON THE WIRE. */
export function builtinReply(name: string, rows: Row[]): Result {
  const wrapper: Record<string, unknown> = {};
  rows.forEach((row, index) => {
    const own = typeof row["id"] === "string" && row["id"] !== "" ? (row["id"] as string) : name;
    const id = own in wrapper ? `${own}-${index}` : own;
    wrapper[id] = {
      id,
      concept: `integration:shopify:${name}`,
      type: "object",
      createdAt: `2026-01-01T00:00:00.${String(999_999_999 - index).padStart(9, "0")}Z`,
      payload: row,
    };
  });
  return new Result({ data: rows.length === 0 ? [] : [wrapper] } as never);
}

export function rowsResult(rows: Row[]): Result {
  return new Result({ data: rows } as never);
}

export interface FakeStoresSeed {
  /** The `stores` array the health builtin's one node carries. */
  stores?: Row[];
  /** Refusal sentences, keyed by construct name (createStore, setStoreStatus,
   *  shopifyStoreHealth, shopifyEnsureSubscriptions). */
  refuse?: Record<string, string>;
}

export interface FakeConnection {
  query: QueryClient;
  /** Every call string that reached the wire, in order. */
  calls: string[];
  callsNamed: (construct: string) => string[];
  /** Replaces what the next health read answers with. */
  setStores: (stores: Row[]) => void;
  subscriptions: { subscribeGraph: () => () => void };
}

export function fakeConnection(seed: FakeStoresSeed = {}): FakeConnection {
  const calls: string[] = [];
  let stores = seed.stores ?? [];
  const stub = {
    executeNamed: vi.fn(async (name: string, call: string) => {
      calls.push(call);
      const refusal = seed.refuse?.[name];
      if (refusal !== undefined) throw new Error(refusal);
      if (call.startsWith("builtin shopifyStoreHealth(")) {
        return builtinReply("shopifyStoreHealth", [{ status: "ok", stores } as unknown as Row]);
      }
      if (call.startsWith("builtin shopifyEnsureSubscriptions(")) {
        return builtinReply("shopifyEnsureSubscriptions", []);
      }
      return rowsResult([]);
    }),
  };
  return {
    query: Object.setPrototypeOf(stub, QueryClient.prototype) as QueryClient,
    calls,
    callsNamed: (construct: string) => calls.filter((c) => c.includes(`${construct}(`)),
    setStores: (next: Row[]) => {
      stores = next;
    },
    subscriptions: { subscribeGraph: () => () => {} },
  };
}

export function withSession(
  children: ReactNode,
  overrides: { userId?: string; domain?: string; role?: string } = {},
) {
  const config: OsRuntimeConfig = {
    ...UNKNOWN_RUNTIME_CONFIG,
    domain: overrides.domain ?? "memql.example.com",
  };
  const role = overrides.role ?? "owner";
  return (
    <SessionProvider
      value={{
        access: {
          userId: overrides.userId ?? "v1:identity:user:me",
          primaryEmail: "me@example.com",
          clusterRole: role,
        },
        config,
      }}
    >
      {/* The Store page's Logs button reads the shell (it is a deep link into
          another app), so the tree needs one. */}
      <OsProvider registry={OS_REGISTRY} actorRole={role} grid={{ cols: 8, rows: 5 }}>
        {children}
      </OsProvider>
    </SessionProvider>
  );
}

export function memStoresStore(
  over: Partial<StoresSettings> = {},
): StoresSettingsStore & { saved: StoresSettings[] } {
  let value: StoresSettings = { ...DEFAULT_STORES_SETTINGS, ...over };
  const saved: StoresSettings[] = [];
  return {
    saved,
    load: () => value,
    save: (next: StoresSettings) => {
      value = next;
      saved.push(next);
    },
  };
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

/**
 * One entry of the health report's `stores` array, with the shape the Go
 * handler emits (`integrations/shopify/capabilities.go`).
 *
 * NOTE WHAT IS ABSENT BY DEFAULT: no `costBucket` (nothing has called the
 * Admin API), no `health.subscriptions` (no reconcile has been recorded) and
 * an EMPTY `domains` (nothing has ever synced). Those are the three states
 * whose honest rendering is a dash, so they are what a fixture starts from --
 * a fixture that filled them in would make the zero-versus-absent tests
 * impossible to write by accident.
 */
export function storeHealth(over: Partial<Row> & { storeId: string }): Row {
  return {
    domain: `${over.storeId}.myshopify.com`,
    status: "live",
    apiVersion: "2026-07",
    mirrorApiVersion: "2026-07",
    protectedDataLevel: "level1",
    scopesGranted: ["read_products", "read_orders"],
    scopesNeeded: ["read_products", "read_orders"],
    scopesMissing: [],
    driftLast: 0,
    domains: [],
    health: {},
    ...over,
  } as unknown as Row;
}

/** One row of a store's per-domain sync table. */
export function domainState(over: Partial<Row> & { concept: string }): Row {
  return {
    phase: "idle",
    lastAppliedAt: "",
    lastReconciledAt: "",
    driftLast: 0,
    driftTotal: 0,
    staleWrites: 0,
    tombstoned: 0,
    lastError: "",
    ...over,
  } as unknown as Row;
}
