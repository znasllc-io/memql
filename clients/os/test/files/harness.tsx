import { parseCall } from "../callArgs";
import type { ReactNode } from "react";
import { act, fireEvent, render } from "@testing-library/react";
import { vi } from "vitest";
import { QueryClient, Result, SubscriptionManager, type Dispatcher, type QueryCallOptions, type Row } from "@znasllc-io/memql-sdk-core/client";

import { SessionProvider } from "../../src/chrome/access";
import { OsProvider } from "../../src/chrome/state";
import { MachinesProvider } from "../../src/live/machines";
import { UNKNOWN_RUNTIME_CONFIG, type OsRuntimeConfig } from "../../src/cluster/config";
import { OS_REGISTRY } from "../../src/apps/registry";
import type { OsAppManifest, OsRegistry } from "../../src/system/registry";
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
  activeCount: (concept: string, idOnly?: boolean) => number;
}

function fakeSubscriptions(): FakeSubscriptions {
  // Exercise the actual wire sharing contract, including reconnect observers.
  const active = new Map<string, { concept?: string; config?: { idOnly?: boolean } }>();
  let deliver: (message: unknown) => void = () => {};
  const dispatcher = {
    send(message: { subscribe?: { subscriptionId: string; concept?: string }; unsubscribe?: { subscriptionId: string } }) {
      if (message.subscribe) active.set(message.subscribe.subscriptionId, message.subscribe);
      if (message.unsubscribe) active.delete(message.unsubscribe.subscriptionId);
      return "message";
    },
    addEventListener(handler: (message: unknown) => void) { deliver = handler; return () => {}; },
  } as unknown as Dispatcher;
  const manager = new SubscriptionManager(dispatcher);
  return {
    subscribeGraph: (handler, opts) => manager.subscribeGraph(handler, opts),
    emit(concept, payload, kind = "NODE_UPDATED") {
      for (const [subscriptionId, options] of [...active]) {
        if (options.concept && options.concept !== concept) continue;
        deliver({ event: { subscriptionId, kind: `EVENT_KIND_${kind}`, payload, seq: 0, gapBefore: false } });
      }
    },
    activeCount: (concept, idOnly) => [...active.values()].filter(options => options.concept === concept && (idOnly === undefined || !!options.config?.idOnly === idOnly)).length,
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
  transfers?: Row[];
  /** The Materializer's records (epic memql#4981) -- what `compositions`
   *  answers. */
  compositions?: Row[];
  byId?: Record<string, Row>;
  /** The account choices -- what `clientAccountsAll` answers. */
  accounts?: Row[];
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
    executeNamed: vi.fn(async (name: string, call: string, options?: QueryCallOptions) => {
      calls.push(call);
      const refusal = seed.refuse?.[name];
      if (refusal !== undefined) throw new Error(refusal);

      const args = /^(query|builtin|mutation) \w+\(/.test(call) ? parseCall(call).args : {};
      const page = (rows: Row[], size = 10) => {
        const offset = Number(options?.cursor ?? 0);
        return new Result({ data: rows.slice(offset, offset + size), meta: { cursor: offset + size < rows.length ? String(offset + size) : "" } } as never);
      };
      const artifactMatches = (row: Row, omitLabel = false) => {
        const search = String(args.search ?? "").trim().toLowerCase();
        const list = (field: string) => Array.isArray(row[field]) ? row[field] as string[] : [];
        return (args.archived === true ? row.archived === true : row.archived !== true)
          && (args.folderId == null || String(row.folderId ?? "") === args.folderId)
          && (args.kind == null || row.kind === args.kind) && (args.source == null || row.source === args.source)
          && (args.accountId == null || (args.accountId === "" ? list("accountIds").length === 0 : list("accountIds").includes(String(args.accountId))))
          && (omitLabel || args.label == null || list("labels").includes(String(args.label)))
          && (args.ids == null || (args.ids as string[]).includes(String(row.id)) || ((args.folderIds as string[] | undefined) ?? []).includes(String(row.folderId)))
          && (args.sourceRefs == null || (args.sourceRefs as string[]).includes(String(row.sourceConceptRef)))
          && (!search || [row.title, row.summary, row.producedByWorkerName, row.id, ...list("labels")].some(value => String(value ?? "").toLowerCase().includes(search)));
      };
      if (name === "libraryArtifactsBrowse" || name === "libraryArtifactsBrowseOldest") {
        const direction = name.endsWith("Oldest") ? 1 : -1;
        const rows = (args.archived === true ? [...new Map([...(seed.artifacts ?? []), ...(seed.archived ?? [])].map(row => [row.id, row])).values()] : seed.artifacts ?? []).filter(row => artifactMatches(row)).sort((a, b) => direction * (String(a.createdAt ?? "").localeCompare(String(b.createdAt ?? "")) || String(a.id).localeCompare(String(b.id))));
        return page(rows);
      }
      if (name === "libraryArchivedFoldersBrowse") {
        const search = String(args.search ?? "").toLowerCase();
        return page((seed.archivedFolders ?? []).filter(row => row.archived === true && row.deleted !== true && (!search || String(row.name ?? "").toLowerCase().includes(search))).sort((a, b) => String(b.createdAt ?? "").localeCompare(String(a.createdAt ?? "")) || String(b.id).localeCompare(String(a.id))));
      }
      if (name === "libraryFolderDirectory") return page([...new Map([...(seed.folders ?? []), ...(seed.archivedFolders ?? [])].map(row => [row.id, row])).values()].filter(row => row.deleted !== true), 500);
      if (name === "libraryBrowseFacets") {
        const rows = (args.archived === true ? [...new Map([...(seed.artifacts ?? []), ...(seed.archived ?? [])].map(row => [row.id, row])).values()] : seed.artifacts ?? []).filter(row => artifactMatches(row, true));
        const folders = new Map<string, number>();
        for (const row of rows) { const id = String(row.folderId ?? ""); folders.set(id, (folders.get(id) ?? 0) + 1); }
        return rowsResult([{ folders: [...folders].map(([folderId, count]) => ({ folderId, count })), labelGroups: rows.map(row => row.labels ?? []) }]);
      }
      if (name === "clientAccountsAll") return rowsResult(seed.accounts ?? []);
      if (name === "libraryFilesByIds") return rowsResult((seed.files ?? []).filter(row => (args.fileIds as string[] ?? []).includes(String(row.id))));
      if (name === "compositionOutputIndex") return rowsResult((seed.compositions ?? []).filter(row => args.fileIds == null || (args.fileIds as string[]).includes(String(row.outputFileId))).map(row => ({ id: row.id, ownerUserId: row.ownerUserId, name: row.name, status: row.status, outputFileId: row.outputFileId, format: row.format, archived: row.archived })));
      if (name === "libraryArtifactsInFolders") return page([...(seed.artifacts ?? []), ...(seed.archived ?? [])].filter(row => (args.folderIds as string[] ?? []).includes(String(row.folderId))), 50);
      if (name === "myMachinePresence") return rowsResult((seed.machines ?? []).map(row => Object.fromEntries(["id", "createdAt", "ownerUserId", "name", "displayName", "connectedNodeId", "lastSeenAt", "revokedAt", "registeredAt"].map(key => [key, row[key]]))));

      if (name === "libraryArtifactById" || name === "libraryArtifactForFile") {
        const value = /(?:artifactId|fileId): "([^"]+)"/.exec(call)?.[1];
        return rowsResult((seed.artifacts ?? []).filter(row => name === "libraryArtifactById"
          ? row.id === value : row.sourceConceptRef === value));
      }
      if (call.startsWith("query libraryArtifactsByLens(")) return rowsResult(seed.artifacts ?? []);
      if (call === "query libraryFolders()") return rowsResult(seed.folders ?? []);
      if (call === "query myWorkersWithStatus()") return rowsResult(seed.machines ?? []);
      if (call.startsWith("query libraryFileById(")) return rowsResult(seed.files ?? []);
      if (call === "query libraryFilesForOwner()") return rowsResult(seed.files ?? []);
      if (call === "query libraryArchivedArtifacts()") return rowsResult(seed.archived ?? []);
      if (call === "query libraryArchivedFolders()") return rowsResult(seed.archivedFolders ?? []);
      if (call.startsWith("query libraryFileVersionsForFile(")) return rowsResult(seed.versions ?? []);
      if (call.startsWith("query libraryWatchedFolders(")) return rowsResult(seed.backups ?? []);
      if (call.startsWith("query compositions(")) return rowsResult(seed.compositions ?? []);
      if (name === "libraryBackupUsage") return rowsResult([{ used: 0, reserved: 0, available: 10_000_000_000, limit: 10_000_000_000, clusterUsed: 0, clusterReserved: 0, clusterLimit: 5_000_000_000_000, exempt: false } as never]);
      if (name === "libraryBackupList") {
        const search = /search: "([^"]*)"/.exec(call)?.[1]?.toLowerCase() ?? "";
        const after = Number(/after: "([^"]*)"/.exec(call)?.[1] ?? 0);
        const values = (seed.transfers ?? []).filter(row => !search || [row.name,row.path,row.workerId,row.kind].some(value => String(value ?? "").toLowerCase().includes(search)));
        return rowsResult([{ transfers: values.slice(after,after+10), nextCursor: after+10<values.length ? String(after+10):"" } as never]);
      }
      if (call.startsWith("mutation ") || call.startsWith("builtin ")) return rowsResult([]);

      // Decode the SDK's quoted ID so a reread does not retain stale seed data.
      const match = /id==("(?:[^"\\]|\\.)*")$/.exec(call);
      const wanted = match ? JSON.parse(match[1]!) as string : "";
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
  /** The explicit "Ask about this" the shell always provides. */
  askAbout?: (tag: string) => void;
  /**
   * The app registry, for the cases that turn on whether ANOTHER app is
   * installed (epic memql#4981: the Materializer handoff is absent when the
   * app is not there, and present when it is). Defaults to the real one.
   */
  registry?: OsRegistry;
} = {}) {
  const frame = (intent: typeof opts.intent) => (
    withSession(
      <OsProvider
        registry={opts.registry ?? OS_REGISTRY}
        actorRole="owner"
        grid={{ cols: 8, rows: 5 }}
        {...(opts.desk ? { store: deskDocumentWith(opts.desk) } : {})}
      >
        <MachinesProvider>
          <FilesApp
            sectionId={opts.section ?? "browse"}
            navigate={() => {}}
            askContext={opts.askContext ?? (() => {})}
            askAbout={opts.askAbout ?? (() => {})}
            intent={intent}
            consumeIntent={opts.consumeIntent ?? (() => {})}
            store={memSettingsStore(opts.settings ?? {})}
            uploads={opts.uploads ?? NO_UPLOADS}
          />
        </MachinesProvider>
      </OsProvider>,
    )
  );
  const view = render(frame(opts.intent));
  // Let the collections run their seeds.
  await act(async () => {});
  return { ...view, setIntent: (intent: typeof opts.intent) => view.rerender(frame(intent)) };
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

/**
 * The real registry with one app added or removed.
 *
 * FOR THE CASES THAT TURN ON WHETHER ANOTHER APP IS INSTALLED. A test that
 * reached for the real registry alone would pass or fail on when a SIBLING
 * epic happens to land -- both directions of "is it there" have to be
 * expressible from one fixture, or the absent case quietly stops being tested
 * the day the app appears.
 */
export function registryWith(app: OsAppManifest | null, without = ""): OsRegistry {
  const apps = OS_REGISTRY.apps.filter((a) => a.id !== without && a.id !== app?.id);
  return { ...OS_REGISTRY, apps: app ? [...apps, app] : apps };
}

export function artifactRow(over: Partial<Row> & { id: string }): Row {
  return {
    ownerUserId: "u-me",
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
    ownerUserId: "u-me",
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
