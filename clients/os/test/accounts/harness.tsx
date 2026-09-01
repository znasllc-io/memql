import type { ReactNode } from "react";
import { vi } from "vitest";
import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";

// The Accounts app's test harness: a connection-shaped double.
//
// CONNECTION-SHAPED rather than a mocked hook per call site, for the reason
// the Users and Fleet harnesses record: every read goes through
// `connection.query.<generated method>` and every subscription through
// `connection.subscriptions`, so a fake that answers those exercises the real
// LiveCollection, the real retain/seed path, the real projections and the real
// arrival fold -- which is where the behaviour under test actually lives.

export function rowsResult(rows: Row[]): Result {
  return new Result({ data: rows } as never);
}

export function bundleResult(rows: Row[]): Result {
  const nodes = rows.map((row) => {
    const { id, createdAt, ...fields } = row as Record<string, unknown>;
    return { id, createdAt, payload: fields };
  });
  return new Result({ bundle: { nodes } } as never);
}

export interface FakeEvent {
  subscriptionId: string;
  kind: string;
  timestamp: Date | null;
  payload: Row | null;
  payloadOmitted: boolean;
  seq: number;
  gapBefore: boolean;
}

export interface FakeSubscriptions {
  subscribeGraph: (handler: (event: FakeEvent) => void, opts: { concept?: string }) => () => void;
  emit: (concept: string, payload: Row, kind?: string) => void;
}

function fakeSubscriptions(): FakeSubscriptions {
  const handlers = new Map<string, Set<(event: FakeEvent) => void>>();
  return {
    subscribeGraph(handler, opts) {
      const concept = opts.concept ?? "*";
      const set = handlers.get(concept) ?? new Set();
      set.add(handler);
      handlers.set(concept, set);
      return () => set.delete(handler);
    },
    emit(concept, payload, kind = "NODE_UPDATED") {
      for (const handler of handlers.get(concept) ?? []) {
        handler({
          subscriptionId: "sub-1",
          kind,
          timestamp: new Date(),
          payload,
          payloadOmitted: false,
          seq: 0,
          gapBefore: false,
        });
      }
    },
  };
}

export interface FakeSeed {
  clientAccountsAll?: Row[];
  sitesForAccount?: Row[];
  libraryItemsForAccount?: Row[];
  domainsForAccount?: Row[];
  /** Pass an Error to make the invitation rollup REFUSE, which is the
   *  interesting case: it is the one band the engine gates. */
  invitationsForAccount?: Row[] | Error;
  campaignsForAccount?: Row[] | Error;
  byId?: Record<string, Row>;
}

export function fakeConnection(seed: FakeSeed = {}) {
  const rollup = (rows: Row[] | Error | undefined) =>
    vi.fn(async () => {
      if (rows instanceof Error) throw rows;
      return rowsResult(rows ?? []);
    });
  return {
    query: {
      clientAccountsAll: vi.fn(async () => rowsResult(seed.clientAccountsAll ?? [])),
      sitesForAccount: rollup(seed.sitesForAccount),
      libraryItemsForAccount: rollup(seed.libraryItemsForAccount),
      domainsForAccount: rollup(seed.domainsForAccount),
      invitationsForAccount: rollup(seed.invitationsForAccount),
      // The fifth band (epic memql#4819). Stubbed to EMPTY rather than left
      // unstubbed, because an unstubbed read throws and the band renders the
      // refusal -- which would make the "a refusal is not a zero" assertion
      // below match two elements and fail for a reason that has nothing to do
      // with what it is testing.
      campaignsForAccount: rollup(seed.campaignsForAccount),
      // TYPED ARGS, so `.mock.calls[0][0]` is a record rather than `never` --
      // a test that asserts WHICH arguments a write received cannot do it
      // through a `vi.fn(async () => ...)` whose parameter list is empty.
      createClientAccount: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      updateClientAccount: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      archiveClientAccount: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      executeNamed: vi.fn(async (_name: string, filter: string) => {
        const match = /id==(\S+)/.exec(filter);
        const wanted = match?.[1] ?? "";
        const row = wanted === "" ? undefined : seed.byId?.[wanted];
        return bundleResult(row ? [row] : []);
      }),
    },
    subscriptions: fakeSubscriptions(),
    dispatcher: { sendAndWait: vi.fn() },
  };
}

export type FakeConnection = ReturnType<typeof fakeConnection>;

export function withSession(children: ReactNode, overrides: { role?: string } = {}) {
  const config: OsRuntimeConfig = { ...UNKNOWN_RUNTIME_CONFIG, domain: "memql.example.com" };
  return (
    <SessionProvider
      value={{
        access: {
          userId: "v1:identity:user:me",
          primaryEmail: "owner@example.com",
          clusterRole: overrides.role ?? "owner",
        },
        config,
      }}
    >
      {children}
    </SessionProvider>
  );
}

/** An account row with sane defaults, overridable field by field. */
export function accountRow(over: Partial<Row> & { id: string }): Row {
  return {
    name: "Acme Consulting",
    domain: "acme.com",
    primaryContactName: "Dana",
    primaryContactEmail: "dana@acme.com",
    notes: "",
    status: "active",
    // CONFIGURED BY DEFAULT, so a test that wants the first-run card has to
    // ASK for it. The card is the surprising state, and a harness whose
    // default produced it would make every unrelated test render a form.
    configuredAt: "2026-08-01T00:00:00Z",
    ownerUserId: "",
    createdAt: "2026-08-01T00:00:00Z",
    ...over,
  };
}
