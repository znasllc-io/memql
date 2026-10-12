import type { ReactNode } from "react";
import { vi } from "vitest";
import { Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";
import { LocalMaterializerSettingsStore } from "../../src/apps/materializer/settings";
import { parseCall } from "./callArgs";

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
  accounts?: Row[];
  files?: Row[];
  runs?: Row[];
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
  outputFile?: Row;
  outputArtifact?: Row;
  /**
   * The Files entry / file row of a SPECIFIC file, for a test whose sources
   * are files of their own. A file not named here answers with
   * `outputArtifact` / `outputFile`, as every file did before these existed.
   */
  artifactsByFile?: Record<string, Row>;
  filesById?: Record<string, Row>;
  /** What the Library answers a document comment / a revision request with. */
  commentReply?: Row;
  revisionReply?: Row;
}

export function fakeConnection(seed: FakeSeed = {}) {
  const write = (reply?: Row) =>
    vi.fn(async (_args: Record<string, unknown>) => {
      if (seed.writeError) throw seed.writeError;
      return rowsResult(reply ? [reply] : []);
    });
  // Read lazily, so a test can add the Files entry AFTER the file is ready --
  // the order a slow index produces.
  const fileRow = (rows: Record<string, Row> | undefined, fallback: Row | undefined, args: Record<string, unknown>) => {
    const found = rows?.[String(args["fileId"])] ?? fallback;
    return rowsResult(found ? [found] : []);
  };
  const fake = {
    /** The seed, so a test can change what the cluster answers mid-test. */
    seed,
    query: {
      libraryArtifactsByLens: vi.fn(async (_args?: Record<string, unknown>, _opts?: unknown) => rowsResult(seed.files ?? [])),
      clientAccountsAll: vi.fn(async (_args?: Record<string, unknown>, _opts?: unknown) => rowsResult(seed.accounts ?? [{id: "client-a", name: "Client A", status: "active"}] as Row[])),
      // TYPED ARGS EVEN ON THE NO-ARGUMENT READS. A `vi.fn(async () => ...)`
      // has an empty parameter list, so `.mock.calls[0][0]` is a tuple of
      // length zero and `tsc -b` -- which covers test/ -- refuses the
      // index. A test that asserts a read was issued UNFILTERED needs it.
      compositions: vi.fn(async (_args?: Record<string, unknown>, _opts?: unknown) =>
        rowsResult(seed.compositions ?? []),
      ),
      workRunForOwner: vi.fn(async (args: Record<string, unknown>, _opts?: unknown) =>
        rowsResult((seed.runs ?? []).filter((row) => row.id === args.runId)),
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
      libraryArtifactForFile: vi.fn(async (args: Record<string, unknown>, _opts?: unknown) =>
        fileRow(seed.artifactsByFile, seed.outputArtifact, args),
      ),
      libraryFileById: vi.fn(async (args: Record<string, unknown>) => fileRow(seed.filesById, seed.outputFile, args)),
      // A TRIPWIRE, NOT A FIXTURE. The app never reads a folder itself: the
      // cluster decides whether a folder can be filed into (compose refuses one
      // the caller cannot read or that is archived), and the app shows that
      // answer. This exists so a test can prove no read was made.
      libraryFolderById: vi.fn(async (_args: Record<string, unknown>) => rowsResult([])),
      libraryAddDocumentComment: write(seed.commentReply ?? ({ saved: true, commentId: "comment-1" } as Row)),
      libraryRequestDocumentRevision: write(seed.revisionReply ?? ({ runId: "run-1", proposal: "proposal-1" } as Row)),
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
      // THE FUNNEL THE SDK HELPERS SPEAK. `startMaterialize` and
      // `handOffDescription` take an `execute(name, call)` over this method,
      // not the typed methods above, so a call to a construct that has a mock
      // here is handed to THAT mock with the args it was rendered from. A test
      // therefore sees `composeMaterialize.mock.calls[0][0]` as an object
      // whichever way the app reached it. A name with no mock here is the
      // by-id read, answered from `byId`.
      //
      // The explicit return type is what lets this refer to `fake`: without
      // it the object's type depends on its own initializer.
      executeNamed: vi.fn(async (name: string, call: string): Promise<Result> => {
        const handler = (fake.query as Record<string, unknown>)[name];
        if (name !== "executeNamed" && typeof handler === "function") {
          return (handler as (args: Record<string, unknown>) => Promise<Result>)(parseCall(call).args);
        }
        const match = /id==(\S+)/.exec(call);
        const wanted = match?.[1] ?? "";
        const row = wanted === "" ? undefined : seed.byId?.[wanted];
        return bundleResult(row ? [row] : []);
      }),
    },
    subscriptions: fakeSubscriptions(),
    dispatcher: { sendAndWait: vi.fn() },
  };
  return fake;
}

export type FakeConnection = ReturnType<typeof fakeConnection>;

/** The app's settings store over a plain map, so no test reads another's. */
export function memoryStore(over: Record<string, unknown> = {}) {
  const bag = new Map<string, string>();
  const store = new LocalMaterializerSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
  if (Object.keys(over).length > 0) store.save({ ...store.load(), ...over });
  return store;
}

export function withSession(children: ReactNode, overrides: { role?: string } = {}) {
  const config: OsRuntimeConfig = { ...UNKNOWN_RUNTIME_CONFIG, domain: "memql.example.com" };
  return (
    <SessionProvider
      value={{
        access: {
          userId: "v1:identity:user:me",
          primaryEmail: "owner@example.com",
          role: overrides.role ?? "owner",
          roleName: "",
          rank: 0,
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
