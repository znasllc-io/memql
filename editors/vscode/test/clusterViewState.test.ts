// The Constructs, Data and Runs views say why they are empty, and offer the
// one act that fixes it.
//
// TWO HALVES THAT MUST AGREE. The trees go EMPTY for every state of the
// connection but a live session (state/clusterViewState.ts), and the manifest
// carries one welcome per state, keyed on `memql.connectionState`, to render
// over that empty tree. A tree that drew a row where a welcome is expected
// hides the welcome; a state with an empty tree and no welcome is a blank
// panel -- which is what the Data view used to be for a selected cluster with
// no session. Both halves are asserted here, against the same six values.
//
// AND THE BUGS THIS SLICE FIXED, each driven: the catalog read that outlived
// the connection it started on, the failed reads that looked like empty
// clusters, and the Runs rows that said less than they should and more than
// they should.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as fsp from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import type { Concept } from "@znasllc-io/memql-sdk-core/client";

import type { ConnectionManager, ConnectionState } from "../src/connection/manager.js";
import { CONNECTION_STATE_KEY, clusterViewState, type ClusterViewState } from "../src/state/clusterViewState.js";
import type { CatalogState } from "../src/state/constructCatalog.js";
import { ConstructsTreeProvider, NO_CONSTRUCTS_MESSAGE } from "../src/views/constructsTree.js";
import { DataTreeProvider, NO_CONCEPTS_MESSAGE } from "../src/views/dataTree.js";
import { RUNS_NEED_CLUSTER_MESSAGE, RunsTreeProvider } from "../src/views/runsTree.js";
import type { MarkdownString } from "./support/vscodeStub.js";

const MANIFEST = path.resolve(__dirname, "..", "..", "package.json");
const manifest = JSON.parse(fs.readFileSync(MANIFEST, "utf8")) as {
  contributes: {
    viewsWelcome: { view: string; contents: string; when?: string }[];
    menus: Record<string, { command: string; when?: string; group?: string }[]>;
  };
};

// -----------------------------------------------------------------------------
// the mapping
// -----------------------------------------------------------------------------

const STATES: readonly [ConnectionState, ClusterViewState][] = [
  [{ status: "disconnected" }, "none"],
  [{ status: "connecting", clusterName: "local" }, "connecting"],
  [{ status: "connected", clusterName: "local", nodeId: "bff-0" }, "connected"],
  [{ status: "error", clusterName: "local", message: "", reason: "missingCredential" }, "signIn"],
  [{ status: "error", clusterName: "local", message: "", reason: "credentialExpired" }, "signIn"],
  [{ status: "error", clusterName: "local", message: "", reason: "wrongTokenClass" }, "signIn"],
  // Refused and cleared: a dead end whose only way forward is a fresh sign-in.
  [{ status: "error", clusterName: "local", message: "", reason: "reauthenticationRequired" }, "signIn"],
  [{ status: "error", clusterName: "local", message: "", reason: "unreachable" }, "unreachable"],
  [{ status: "error", clusterName: "local", message: "", reason: "lost" }, "unreachable"],
  [{ status: "error", clusterName: "local", message: "", reason: "notConfigured" }, "notConfigured"],
];

test("every connection state maps to what a view shows for it", () => {
  for (const [state, view] of STATES) {
    const reason = state.status === "error" ? ` (${state.reason})` : "";
    assert.equal(clusterViewState(state), view, `${state.status}${reason}`);
  }
});

// -----------------------------------------------------------------------------
// the welcomes
// -----------------------------------------------------------------------------

function welcomes(view: string): { contents: string; when?: string }[] {
  return manifest.contributes.viewsWelcome.filter((entry) => entry.view === view);
}

function linked(contents: string): string[] {
  return [...contents.matchAll(/\(command:([A-Za-z0-9_.]+)\)/g)].map((m) => m[1]);
}

for (const [view, refresh, noun] of [
  ["memqlConstructs", "memql.constructs.refresh", "constructs"],
  ["memqlData", "memql.data.refresh", "data"],
] as const) {
  test(`${view} has one welcome, with its act, for each state it goes empty in`, () => {
    const byState = (state: ClusterViewState) =>
      welcomes(view).filter((entry) => entry.when === `${CONNECTION_STATE_KEY} == ${state}`);

    const signIn = byState("signIn");
    assert.equal(signIn.length, 1, "no sign-in welcome");
    assert.equal(signIn[0].contents.split("\n")[0], `Sign in to see ${noun}.`);
    assert.deepEqual(linked(signIn[0].contents), ["memql.clusters.signIn"]);

    const unreachable = byState("unreachable");
    assert.equal(unreachable.length, 1, "no welcome for a cluster that is not answering");
    assert.deepEqual(linked(unreachable[0].contents), [refresh], "Retry is the view's own refresh, which redials");

    const notConfigured = byState("notConfigured");
    assert.equal(notConfigured.length, 1, "no welcome for a cluster with nothing to dial");
    assert.deepEqual(linked(notConfigured[0].contents), ["memql.clusters.edit"]);

    // Connecting has NO welcome: the view draws its own progress bar and no
    // text while the dial is in flight.
    assert.equal(byState("connecting").length, 0, "connecting shows a sentence instead of the view's progress");
    // And connected has none either -- rows, or the view's message.
    assert.equal(byState("connected").length, 0);
  });
}

// -----------------------------------------------------------------------------
// the trees go empty exactly when a welcome is there to say why
// -----------------------------------------------------------------------------

interface FakeManager {
  manager: ConnectionManager;
  set(state: ConnectionState, query?: unknown): void;
}

function fakeManager(initial: ConnectionState, query?: unknown): FakeManager {
  const listeners: ((state: ConnectionState) => void)[] = [];
  const fake = {
    state: initial,
    query,
    dispatcher: undefined,
    onDidChangeState: (listener: (state: ConnectionState) => void) => {
      listeners.push(listener);
      return () => undefined;
    },
  };
  return {
    manager: fake as unknown as ConnectionManager,
    set(state, nextQuery) {
      fake.state = state;
      fake.query = nextQuery;
      for (const listener of listeners) listener(state);
    },
  };
}

const CONNECTED: ConnectionState = { status: "connected", clusterName: "local", nodeId: "bff-0" };

test("Constructs is empty, and reads nothing, in every state but a live session", async () => {
  for (const [state, view] of STATES) {
    if (view === "connected") continue;
    let reads = 0;
    let cleared = 0;
    const tree = new ConstructsTreeProvider({
      connections: fakeManager(state).manager,
      load: async () => {
        reads += 1;
        return { kind: "loaded", groups: [], total: 0 };
      },
      unavailable: () => {
        cleared += 1;
      },
    });
    assert.deepEqual(await tree.getChildren(), [], `${view} drew a row over its welcome`);
    assert.equal(reads, 0, `${view} read the catalog with no session`);
    // The read-only marking that rode the last read is withdrawn.
    assert.ok(cleared > 0, `${view} left the last cluster's marking in place`);
  }
});

test("Data is empty, and reads nothing, in every state but a live session -- never blank without a welcome", async () => {
  for (const [state, view] of STATES) {
    if (view === "connected") continue;
    let reads = 0;
    const query = {
      listConcepts: async () => {
        reads += 1;
        return [];
      },
    };
    const tree = new DataTreeProvider(fakeManager(state, query).manager);
    assert.deepEqual(await tree.getChildren(), [], `${view} drew a row over its welcome`);
    assert.equal(reads, 0, `${view} listed concepts with no session`);
    // Every one of these states has a welcome to say why the view is empty.
    assert.ok(
      view === "connecting" || view === "none" || welcomes("memqlData").some((e) => e.when === `${CONNECTION_STATE_KEY} == ${view}`),
      `${view} leaves the Data view blank`,
    );
  }
});

// -----------------------------------------------------------------------------
// the stale read (the "Cluster not answering" screenshot)
// -----------------------------------------------------------------------------

function deferred<T>(): { promise: Promise<T>; resolve(value: T): void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

const GROUPS: CatalogState = {
  kind: "loaded",
  total: 1,
  groups: [{ kind: "query", label: "Queries", count: 1, runnable: true, namespaces: [] }],
};

test("a catalog read that outlives its connection is dropped, not painted", async () => {
  // THE BUG: refresh() reset the state but kept the in-flight read, and the
  // next ask reused it -- so a read started on the old connection landed on the
  // new one and stayed there, with nothing firing another change.
  const conn = fakeManager(CONNECTED);
  const reads = [deferred<CatalogState>(), deferred<CatalogState>()];
  let calls = 0;
  const tree = new ConstructsTreeProvider({
    connections: conn.manager,
    load: () => reads[calls++].promise,
  });

  const first = tree.getChildren();
  // The connection changes while the first read is in flight.
  conn.set({ status: "connected", clusterName: "staging", nodeId: "bff-1" });
  const second = tree.getChildren();
  assert.equal(calls, 2, "the new connection reused the old connection's read");

  reads[1].resolve(GROUPS);
  // The OLD read settles last, with the old connection's answer.
  reads[0].resolve({ kind: "failed", message: "stream closed" });

  const [a, b] = await Promise.all([first, second]);
  assert.deepEqual(b.map((row) => row.kind), ["group"]);
  // Even the caller that asked on the old connection gets the current answer.
  assert.deepEqual(a.map((row) => row.kind), ["group"], "the superseded read was painted");
  assert.deepEqual((await tree.getChildren()).map((row) => row.kind), ["group"]);
});

test("a connected cluster with nothing loaded says so, and a failed read is a row whose click retries", async () => {
  let message: string | undefined = "unset";
  const empty = new ConstructsTreeProvider({
    connections: fakeManager(CONNECTED).manager,
    load: async () => ({ kind: "loaded", groups: [], total: 0 }),
    setMessage: (m) => {
      message = m;
    },
  });
  assert.deepEqual(await empty.getChildren(), []);
  assert.equal(message, NO_CONSTRUCTS_MESSAGE);

  const failed = new ConstructsTreeProvider({
    connections: fakeManager(CONNECTED).manager,
    load: async () => ({ kind: "failed", message: "PERMISSION_DENIED: nope" }),
  });
  const rows = await failed.getChildren();
  assert.equal(rows.length, 1);
  const item = failed.getTreeItem(rows[0]);
  assert.equal(item.label, "Couldn't load constructs");
  assert.equal(item.command?.command, "memql.constructs.refresh");
  // No claim about a channel the row cannot back up.
  assert.doesNotMatch(String(item.tooltip), /output channel/i);
});

test("a session that drops between the state check and the read leaves no blank row", async () => {
  // load() answers `unreachable` when the dispatcher is gone by the time it
  // runs. That used to be drawn as a row with an empty label.
  const tree = new ConstructsTreeProvider({
    connections: fakeManager(CONNECTED).manager,
    load: async () => ({ kind: "unreachable" }),
  });
  assert.deepEqual(await tree.getChildren(), []);
});

test("a runnable construct's row carries the value the inline Run is keyed on, and a view-only one does not", async () => {
  const construct = (runnable: boolean) => ({
    name: "spaceParticipants",
    kind: runnable ? "query" : "concept",
    namespace: "cognition",
    origin: "bundle" as const,
    originPath: "cognition/queries.memql",
    description: "Get space participants",
    runnable,
    ...(runnable ? { runnableKind: "query" as const } : {}),
    args: [],
    boundConcept: "",
    sourceHash: "",
    source: "",
  });
  const tree = new ConstructsTreeProvider({
    connections: fakeManager(CONNECTED).manager,
    load: async () => GROUPS,
  });
  const run = tree.getTreeItem({ kind: "construct", construct: construct(true) });
  const view = tree.getTreeItem({ kind: "construct", construct: construct(false) });
  assert.equal(run.contextValue, "memqlRunnableConstruct");
  assert.equal(view.contextValue, "memqlConstruct");
  // The default origin is not repeated on every row.
  assert.equal(run.description, undefined);
  // The tooltip is one line of facts, not key: value code.
  const tooltip = (run.tooltip as MarkdownString).value;
  assert.match(tooltip, /Query · Bundle/);
  assert.doesNotMatch(tooltip, /kind: `/);

  const inline = manifest.contributes.menus["view/item/context"].find(
    (entry) => entry.command === "memql.constructs.run",
  );
  assert.ok(inline !== undefined, "the Constructs view has no Run act");
  assert.match(inline.when ?? "", /viewItem == memqlRunnableConstruct/);
  assert.match(inline.when ?? "", /memql\.connected/);
  assert.equal(inline.group, "inline@1");
});

// -----------------------------------------------------------------------------
// Data
// -----------------------------------------------------------------------------

function concept(id: string, description = ""): Concept {
  const [, domain, entity] = id.split(":");
  return { id, domain, entity, description } as unknown as Concept;
}

test("Data: domains carry counts, concept rows carry no id, and an empty cluster says so", async () => {
  const query = {
    listConcepts: async () => [concept("v1:cognition:space", "A space"), concept("v1:cognition:participant"), concept("v1:identity:user")],
  };
  const tree = new DataTreeProvider(fakeManager(CONNECTED, query).manager);
  const domains = await tree.getChildren();
  assert.deepEqual(
    domains.map((node) => (node.kind === "domain" ? `${node.domain} ${node.count}` : node.kind)),
    ["cognition 2", "identity 1"],
  );
  assert.equal(tree.getTreeItem(domains[0]).description, "2");
  const concepts = await tree.getChildren(domains[0]);
  const space = tree.getTreeItem(concepts[1]);
  assert.equal(space.label, "space");
  assert.equal(space.description, undefined, "the id is repeated on the row");
  assert.match(String(space.tooltip), /v1:cognition:space/);

  let message: string | undefined = "unset";
  const empty = new DataTreeProvider(fakeManager(CONNECTED, { listConcepts: async () => [] }).manager, {
    setMessage: (m) => {
      message = m;
    },
  });
  assert.deepEqual(await empty.getChildren(), []);
  assert.equal(message, NO_CONCEPTS_MESSAGE);
});

test("Data: a failed read is a row whose click retries, and it is recorded once", async () => {
  const noted: string[] = [];
  const query = {
    listConcepts: async () => {
      throw new Error("PERMISSION_DENIED: nope");
    },
  };
  const tree = new DataTreeProvider(fakeManager(CONNECTED, query).manager, {
    noteFailure: (message) => noted.push(message),
  });
  const rows = await tree.getChildren();
  assert.equal(rows.length, 1);
  const item = tree.getTreeItem(rows[0]);
  assert.equal(item.label, "Couldn't load concepts");
  assert.equal(item.command?.command, "memql.data.refresh");
  await tree.getChildren();
  assert.equal(noted.length, 1, "a repaint recorded the same failure again");
});

// -----------------------------------------------------------------------------
// Runs
// -----------------------------------------------------------------------------

async function workspaceWith(runsJson: string | undefined): Promise<string> {
  const dir = await fsp.mkdtemp(path.join(os.tmpdir(), "memql-runs-"));
  if (runsJson !== undefined) {
    await fsp.mkdir(path.join(dir, ".memql"), { recursive: true });
    await fsp.writeFile(path.join(dir, ".memql", "runs.json"), runsJson, "utf8");
  }
  return dir;
}

test("Runs: no folder and no saved runs are both an empty tree, for their welcomes", async () => {
  assert.deepEqual(await new RunsTreeProvider(undefined).getChildren(), []);
  const dir = await workspaceWith(undefined);
  try {
    assert.deepEqual(await new RunsTreeProvider(dir).getChildren(), []);
  } finally {
    await fsp.rm(dir, { recursive: true, force: true });
  }
});

test("Runs: a file that does not parse is one row that opens it, with no path in the label", async () => {
  const dir = await workspaceWith("{ not json");
  try {
    const tree = new RunsTreeProvider(dir);
    const rows = await tree.getChildren();
    assert.equal(rows.length, 1);
    const item = tree.getTreeItem(rows[0]);
    assert.equal(item.label, "Can't read runs.json");
    assert.ok(!String(item.description).includes(dir), "the row carries the absolute path");
    assert.equal(item.command?.command, "memql.runs.open");
  } finally {
    await fsp.rm(dir, { recursive: true, force: true });
  }
});

test("Runs: skipped entries are counted and opened, and a row never shows an argument value", async () => {
  const dir = await workspaceWith(
    JSON.stringify({
      version: 1,
      runs: [
        { name: "smoke", kind: "query", construct: "spaceParticipants", args: { spaceId: "sk-secret-value" }, file: "cognition/queries.memql" },
        { nope: true },
      ],
    }),
  );
  try {
    let message: string | undefined = "unset";
    const tree = new RunsTreeProvider(dir, {
      connected: () => false,
      setMessage: (m) => {
        message = m;
      },
    });
    const rows = await tree.getChildren();
    assert.deepEqual(rows.map((row) => row.kind), ["run", "dropped"]);
    const run = tree.getTreeItem(rows[0]);
    assert.equal(run.description, "spaceParticipants");
    assert.equal((run.iconPath as { id?: string } | undefined)?.id, "search", "the row's icon is the kind, not the act");
    assert.doesNotMatch(String(run.tooltip), /sk-secret-value/);
    assert.match(String(run.tooltip), /1 saved argument/);
    const dropped = tree.getTreeItem(rows[1]);
    assert.equal(dropped.label, "1 saved run skipped");
    assert.match(String(dropped.tooltip), /entry 2/);
    assert.equal(dropped.command?.command, "memql.runs.open");
    // Listed while disconnected, with the reason Run is not offered.
    assert.equal(message, RUNS_NEED_CLUSTER_MESSAGE);
  } finally {
    await fsp.rm(dir, { recursive: true, force: true });
  }
});

test("Runs: Run is offered only while connected, Delete is not inline, and Refresh left the title", () => {
  const items = manifest.contributes.menus["view/item/context"].filter((entry) => (entry.when ?? "").includes("view == memqlRuns"));
  const inline = items.filter((entry) => (entry.group ?? "").startsWith("inline"));
  assert.deepEqual(inline.map((entry) => entry.command), ["memql.runs.execute"]);
  assert.match(inline[0].when ?? "", /memql\.connected/);
  const remove = items.find((entry) => entry.command === "memql.runs.delete");
  assert.ok(remove !== undefined, "Delete is gone");
  assert.match(remove.group ?? "", /^9_/, "Delete is not in its own last group");
  const title = manifest.contributes.menus["view/title"].filter((entry) => (entry.when ?? "").includes("memqlRuns"));
  assert.deepEqual(title.map((entry) => entry.command), ["memql.runs.open"]);
  assert.match(title[0].when ?? "", /workbenchState != empty/);
});
