import type { ReactNode } from "react";
import { vi } from "vitest";
import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";

// The Materializer's test harness: a connection-shaped double.
//
// CONNECTION-SHAPED rather than a mocked hook per call site, for the
// reason every harness in this suite records: every read goes through
// `connection.query.<generated method>` and every subscription through
// `connection.subscriptions`, so a fake that answers those exercises the
// real LiveCollection, the real retain/seed path, the real projections
// and the real arrival fold -- which is where the behaviour under test
// actually lives.

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

function fakeSubscriptions() {
  const handlers = new Map<string, Set<(event: FakeEvent) => void>>();
  return {
    subscribeGraph(handler: (event: FakeEvent) => void, opts: { concept?: string }) {
      const concept = opts.concept ?? "*";
      const set = handlers.get(concept) ?? new Set();
      set.add(handler);
      handlers.set(concept, set);
      return () => set.delete(handler);
    },
    emit(concept: string, payload: Row, kind = "NODE_UPDATED") {
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
  compositions?: Row[];
  templates?: Row[];
  recipes?: Row[];
  /** What `composableConcepts` answers with. */
  composables?: Row;
  /** What `composeResolveSources` answers with. */
  resolved?: Row;
  /** Make a write REFUSE: the surface must show the server's own sentence. */
  writeError?: Error;
  /** What `composeMaterialize` answers with. */
  materializeReply?: Row;
  byId?: Record<string, Row>;
}

export function fakeConnection(seed: FakeSeed = {}) {
  const write = (reply?: Row) =>
    vi.fn(async (_args: Record<string, unknown>) => {
      if (seed.writeError) throw seed.writeError;
      return rowsResult(reply ? [reply] : []);
    });
  return {
    query: {
      // TYPED ARGS EVEN ON THE NO-ARGUMENT READS. A `vi.fn(async () => ...)`
      // has an empty parameter list, so `.mock.calls[0][0]` is a tuple of
      // length zero and `tsc -b` -- which covers test/ -- refuses the
      // index. A test that asserts a read was issued UNFILTERED needs it.
      compositions: vi.fn(async (_args?: Record<string, unknown>, _opts?: unknown) =>
        rowsResult(seed.compositions ?? []),
      ),
      composeTemplates: vi.fn(async (_args?: Record<string, unknown>, _opts?: unknown) =>
        rowsResult(seed.templates ?? []),
      ),
      composeRecipes: vi.fn(async (_args?: Record<string, unknown>, _opts?: unknown) =>
        rowsResult(seed.recipes ?? []),
      ),
      composableConcepts: vi.fn(async (_args?: Record<string, unknown>, _opts?: unknown) =>
        rowsResult(seed.composables ? [seed.composables] : [{ concepts: [], registryAvailable: true }]),
      ),
      composeResolveSources: vi.fn(async (_args?: Record<string, unknown>, _opts?: unknown) =>
        rowsResult(seed.resolved ? [seed.resolved] : [{ sources: [], total: 0 }]),
      ),
      composeMaterialize: write(seed.materializeReply),
      composeRunRecipe: write(seed.materializeReply),
      composeCancel: write(),
      archiveComposition: write(),
      restoreComposition: write(),
      createComposeTemplate: write(),
      archiveComposeTemplate: write(),
      restoreComposeTemplate: write(),
      createComposeRecipe: write(),
      archiveComposeRecipe: write(),
      restoreComposeRecipe: write(),
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

// ---------------------------------------------------------------------------
// Row builders, with defaults chosen so a test has to ASK for the
// interesting state rather than getting it by accident.
// ---------------------------------------------------------------------------

export function compositionRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    name: "Q3 report",
    statement: "Draft the Q3 report for Acme from the open invoices",
    status: "ready",
    format: "pdf",
    templateId: "",
    outputFileId: "v1:library:file:f1",
    folderId: "",
    accountIds: [],
    goalId: "v1:work:goal:g1",
    // A DIFFERENT runId ON EVERY BUILD would be an accident; a test that
    // wants the re-stamp asks for it.
    runId: "v1:work:run:r1",
    recipeId: "",
    sources: [
      { kind: "concept_row", ref: "v1:x:invoice#a", label: "INV-1001", capturedAt: "2026-09-05T12:00:00Z" },
      { kind: "concept_row", ref: "v1:x:invoice#b", label: "INV-1002", capturedAt: "2026-09-05T12:00:00Z" },
    ],
    modelsUsed: [{ provider: "anthropic", model: "claude-sonnet-5", calls: 1, tokens: 4200 }],
    provenanceEmbedded: true,
    provenanceNote: "Provenance is in this PDF's XMP packet and Info dictionary.",
    deployableKind: "",
    failureReason: "",
    archived: false,
    createdAt: "2026-09-05T12:00:00Z",
    ...over,
  };
}

export function templateRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    name: "Acme quarterly",
    description: "The branded report we send Acme",
    fileId: "v1:library:file:tpl1",
    format: "pdf",
    placeholders: [],
    accountIds: [],
    archived: false,
    createdAt: "2026-08-01T09:00:00Z",
    ...over,
  };
}

export function recipeRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "v1:identity:user:me",
    name: "Acme quarterly report",
    description: "The open invoices, through the Acme template",
    sourceSelectors: [
      { kind: "concept_query", selector: "query openInvoices()", label: "open invoices" },
    ],
    templateId: "",
    format: "pdf",
    folderId: "",
    accountIds: [],
    lastRunAt: "2026-06-30T09:00:00Z",
    runCount: 2,
    archived: false,
    createdAt: "2026-03-01T09:00:00Z",
    ...over,
  };
}
