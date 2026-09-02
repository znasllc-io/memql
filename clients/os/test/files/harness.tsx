import type { ReactNode } from "react";
import { act, fireEvent, render } from "@testing-library/react";
import { vi } from "vitest";
import { QueryClient, Result, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { OsProvider } from "../../src/chrome/state";
import { MachinesProvider } from "../../src/live/machines";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";
import { OS_REGISTRY } from "../../src/apps/registry";
import { FilesApp } from "../../src/apps/files/FilesApp";
import type { FilesSettings } from "../../src/apps/files/settings";
import { DEFAULT_FILES_SETTINGS } from "../../src/apps/files/settings";
import type { UploadProvider } from "../../src/items/upload";
import type { DesktopItem } from "../../src/system/desktop";

// The Files app's test harness.
//
// THE FAKE SITS UNDER `executeNamed`, NOT OVER THE GENERATED METHODS -- the
// deployables harness's rule, kept for the reason its header states: a double
// that stubs `query.createLibraryFolder` records arguments and never renders
// the call, so the generated builder (the thing that turns arguments into
// MemQL text the engine has to parse) would run in production and nowhere
// else. The stub answers at the funnel, so every test exercises the real
// builders, the real LiveCollection retain/seed path and the real
// projections.

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
  /** Live handler count per concept -- what the desk's subscription-free
   *  contract asserts. */
  activeCount: (concept: string) => number;
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
    activeCount(concept) {
      return handlers.get(concept)?.size ?? 0;
    },
  };
}

export interface FakeSeed {
  artifacts?: Row[];
  folders?: Row[];
  machines?: Row[];
  files?: Row[];
  /** The Bin's population -- what libraryArchivedArtifacts answers. */
  archived?: Row[];
  /** Archived folders, which libraryFolders deliberately cannot see. */
  archivedFolders?: Row[];
  /** Superseded version rows, answering libraryFileVersionsForFile. */
  versions?: Row[];
  /** The watched-folder arrangements (epic memql#4783). */
  backups?: Row[];
  byId?: Record<string, Row>;
  /** Refusal sentences, keyed by construct name (e.g. archiveArtifact). */
  refuse?: Record<string, string>;
}

export interface FakeConnection {
  query: QueryClient;
  calls: string[];
  callsNamed: (construct: string) => string[];
  subscriptions: FakeSubscriptions;
}

export function fakeConnection(seed: FakeSeed = {}): FakeConnection {
  const calls: string[] = [];
  const stub = {
    executeNamed: vi.fn(async (name: string, call: string) => {
      calls.push(call);
      const refusal = seed.refuse?.[name];
      if (refusal !== undefined) throw new Error(refusal);

      if (call.startsWith("query libraryArtifactsByLens(")) return rowsResult(seed.artifacts ?? []);
      if (call === "query libraryFolders()") return rowsResult(seed.folders ?? []);
      if (call === "query myWorkersWithStatus()") return rowsResult(seed.machines ?? []);
      if (call.startsWith("query libraryFileById(")) return rowsResult(seed.files ?? []);
      if (call === "query libraryFilesForOwner()") return rowsResult(seed.files ?? []);
      if (call === "query libraryArchivedArtifacts()") return rowsResult(seed.archived ?? []);
      if (call === "query libraryArchivedFolders()") return rowsResult(seed.archivedFolders ?? []);
      if (call.startsWith("query libraryFileVersionsForFile(")) return rowsResult(seed.versions ?? []);
      if (call.startsWith("query libraryWatchedFolders(")) return rowsResult(seed.backups ?? []);
      if (call.startsWith("mutation ") || call.startsWith("builtin ")) return rowsResult([]);

      const match = /id==(\S+)/.exec(call);
      const wanted = match?.[1] ?? "";
      const row = wanted === "" ? undefined : seed.byId?.[wanted];
      return bundleResult(row ? [row] : []);
    }),
  };
  return {
    query: Object.setPrototypeOf(stub, QueryClient.prototype) as QueryClient,
    calls,
    callsNamed: (construct: string) => calls.filter((c) => c.includes(`${construct}(`)),
    subscriptions: fakeSubscriptions(),
  };
}

export function withSession(children: ReactNode, overrides: { userId?: string; role?: string } = {}) {
  const config: OsRuntimeConfig = { ...UNKNOWN_RUNTIME_CONFIG, domain: "memql.example.com" };
  return (
    <SessionProvider
      value={{
        access: {
          userId: overrides.userId ?? "u-me",
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

export function memSettingsStore(over: Partial<FilesSettings> = {}) {
  let value: FilesSettings = { ...DEFAULT_FILES_SETTINGS, ...over };
  return {
    load: () => value,
    save: (next: FilesSettings) => void (value = next),
  };
}

export const NO_UPLOADS: UploadProvider = {
  upload: () => ({ done: new Promise(() => {}), abort: () => {} }),
};

/** A seeded desktop document, for tests exercising the Desktop place: one
 *  desk carrying the given file icons and folder shortcuts. */
export function deskDocumentWith(desk: {
  files?: Array<{ artifactId: string; title: string }>;
  folders?: Array<{ folderId: string; name: string }>;
}) {
  const items: Record<string, DesktopItem> = {};
  const positions: Record<string, { col: number; row: number }> = {};
  let at = 0;
  for (const file of desk.files ?? []) {
    const id = `seed-file-${at}`;
    items[id] = {
      kind: "file",
      id,
      artifactId: file.artifactId,
      title: file.title,
      fileKind: "file",
      source: "uploaded",
    };
    positions[id] = { col: at, row: 0 };
    at += 1;
  }
  for (const folder of desk.folders ?? []) {
    const id = `seed-folder-${at}`;
    items[id] = { kind: "folder", id, folderId: folder.folderId, name: folder.name };
    positions[id] = { col: at, row: 0 };
    at += 1;
  }
  const document = {
    version: 1 as const,
    desks: [{ id: "desk-seeded", createdBy: "user" as const }],
    activeDeskId: "desk-seeded",
    surfaces: { "desk-seeded": { items, positions } },
    dock: { pinned: [] },
    themePack: "graphite",
    installedPacks: [],
  };
  return { load: () => document, save: () => {} };
}

/** Render the Files app inside the providers it really mounts under. */
export async function renderFiles(opts: {
  section?: string;
  settings?: Partial<FilesSettings>;
  uploads?: UploadProvider;
  /** Seed the shell's desktop, for the Desktop place. */
  desk?: Parameters<typeof deskDocumentWith>[0];
  /** A standing open intent, delivered as the window would deliver it. */
  intent?: { id: string; payload: Record<string, unknown> };
  consumeIntent?: (intentId: string) => void;
  /** The Ask tag the surface hands the shell, for the tests that pin it. */
  askContext?: (tag: string) => void;
} = {}) {
  const view = render(
    withSession(
      <OsProvider
        registry={OS_REGISTRY}
        actorRole="owner"
        grid={{ cols: 8, rows: 5 }}
        {...(opts.desk ? { store: deskDocumentWith(opts.desk) } : {})}
      >
        <MachinesProvider>
          <FilesApp
            sectionId={opts.section ?? "browse"}
            navigate={() => {}}
            askContext={opts.askContext ?? (() => {})}
            intent={opts.intent}
            consumeIntent={opts.consumeIntent ?? (() => {})}
            store={memSettingsStore(opts.settings ?? {})}
            uploads={opts.uploads ?? NO_UPLOADS}
          />
        </MachinesProvider>
      </OsProvider>,
    ),
  );
  // Let the collections run their seeds.
  await act(async () => {});
  return view;
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

export function artifactRow(over: Partial<Row> & { id: string }): Row {
  return {
    lens: "artifact",
    kind: "file",
    source: "uploaded",
    sourceConceptRef: `v1:library:file:${over.id}`,
    title: `${over.id}.bin`,
    summary: "",
    labels: [],
    archived: false,
    folderId: "",
    createdAt: "2026-08-20T10:00:00Z",
    ...over,
  };
}

/** A v1:library:file head row, at whatever version the caller names. */
export function fileRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "u-me",
    name: `${over.id}.bin`,
    mimeType: "application/octet-stream",
    size: 1024,
    sha256: "",
    blobUrl: `library/u-me/${over.id}/${over.id}.bin`,
    source: "uploaded",
    format: "other",
    status: "ready",
    summary: "",
    archived: false,
    folderId: "",
    uploadedFromWorkerId: "",
    uploadedFromWorkerName: "",
    uploadedFromPath: "",
    versionNumber: 1,
    versionUploadedAt: "2026-08-20T10:00:00Z",
    createdAt: "2026-08-20T10:00:00Z",
    ...over,
  };
}

/** One superseded v1:library:fileVersion row. */
export function versionRow(over: Partial<Row> & { id: string; fileId: string; versionNumber: number }): Row {
  return {
    ownerUserId: "u-me",
    name: `${over.fileId}.bin`,
    mimeType: "application/octet-stream",
    size: 512,
    sha256: "",
    blobUrl: `library/u-me/${over.fileId}/v${over.versionNumber}/${over.fileId}.bin`,
    format: "other",
    summary: "",
    uploadedFromWorkerId: "",
    uploadedFromWorkerName: "",
    uploadedFromPath: "",
    uploadedAt: "2026-08-01T10:00:00Z",
    supersededAt: "2026-08-20T10:00:00Z",
    createdAt: "2026-08-20T10:00:00Z",
    ...over,
  };
}

/** A watched folder. The three fields a cockpit reports are ABSENT by default,
 *  which is the state every backup starts in and the one most easily got
 *  wrong -- "no machine has checked in yet" must not read as "everything is
 *  fine". Pass them explicitly to model a machine that has reported. */
export function watchedFolderRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "user-1",
    workerId: "wkr-1",
    localPath: "/Users/ana/Clients",
    folderId: "",
    status: "active",
    excludeGlobs: [],
    includeHidden: false,
    archived: false,
    createdAt: "2026-08-30T12:00:00Z",
    ...over,
  } as Row;
}

export function folderRow(over: Partial<Row> & { id: string }): Row {
  return {
    name: over.id,
    parentFolderId: "",
    archived: false,
    createdAt: "2026-08-19T10:00:00Z",
    ...over,
  };
}

export async function emit(
  connection: FakeConnection,
  concept: string,
  payload: Row,
  kind = "NODE_UPDATED",
): Promise<void> {
  await act(async () => {
    connection.subscriptions.emit(concept, payload, kind);
  });
}

export async function click(el: Element | null | undefined): Promise<void> {
  if (!el) throw new Error("click() was handed nothing to click");
  await act(async () => {
    fireEvent.click(el);
  });
}
