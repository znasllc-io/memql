// The three cluster-backed views go EMPTY when no cluster is selected, and the
// fourth deliberately does not (memql#4425).
//
// WHY THIS IS THE TEST THAT MATTERS FOR THE WELCOMES. VS Code renders
// `viewsWelcome` content only over a genuinely EMPTY tree. So the welcome is
// not something a provider shows -- it is something a provider gets out of the
// way of, and any row at all, however well meant, deletes it. The old
// behaviour was three different kinds of that mistake in three views:
// Constructs returned a synthetic "Not connected" row, Data returned whatever
// an unconnected `listConcepts` produced, and Deployments listed every
// registered instance regardless of what was selected.
//
// The failure is INVISIBLE to every other lane. The extension activates, the
// view has content, `getTreeItem` renders it happily, and the operator is
// looking at the wrong thing. A host smoke test cannot read back welcome
// content either. What can be checked is the provider's own answer, and that is
// the whole of the mechanism.
//
// THE SECOND CLAIM, and it is the one that keeps this from being a rule applied
// blindly: RUNS KEEPS LISTING. Its rows are `runs.json` entries -- the
// developer's own file, in their own workspace -- and emptying the view because
// no cluster happens to be selected would present their saved work as gone.
// The gate moves to `memql.runs.execute` instead, which is where a cluster is
// actually required. Asserted here beside the other three so the exception is
// visible as a decision rather than as an oversight.
//
// Refs: #4425 #4423

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import type { ConnectionManager } from "../src/connection/manager.js";
import type { ConnectionContextKeys } from "../src/state/connectionContext.js";
import type { CatalogState } from "../src/state/constructCatalog.js";
import { ConstructsTreeProvider } from "../src/views/constructsTree.js";
import { DataTreeProvider } from "../src/views/dataTree.js";
import { DeploymentsTreeProvider } from "../src/views/deploymentsTree.js";
import { RunsTreeProvider } from "../src/views/runsTree.js";

const SELECTED: ConnectionContextKeys = { clusterSelected: true, connected: true };
const NOTHING: ConnectionContextKeys = { clusterSelected: false, connected: false };

/** A manager as far as a tree uses one: a state to read and a listener slot. */
function fakeManager(): ConnectionManager {
  return {
    state: { status: "disconnected" },
    query: undefined,
    dispatcher: undefined,
    onDidChangeState: () => () => undefined,
  } as unknown as ConnectionManager;
}

// ---------------------------------------------------------------------------
// Constructs
// ---------------------------------------------------------------------------

test("Constructs returns [] with nothing selected, and does not even read", async () => {
  let reads = 0;
  const tree = new ConstructsTreeProvider({
    connections: fakeManager(),
    connectionContext: () => NOTHING,
    load: async (): Promise<CatalogState> => {
      reads += 1;
      return { kind: "loaded", groups: [], total: 0 };
    },
  });
  assert.deepEqual(await tree.getChildren(), []);
  // The read is skipped rather than performed and discarded: a view nobody has
  // selected a cluster for must not issue a ListConstructs over the wire.
  assert.equal(reads, 0, "an unselected Constructs view issued a catalog read");
});

test("Constructs still speaks when a cluster IS selected and unreachable", async () => {
  // The distinction design D2 turns on. This is NOT the empty state: a cluster
  // was chosen and is not answering, which is a fact about something, so it
  // gets a row -- and the row must not be the welcome's sentence, or the two
  // would be saying the same thing in two places with only one of them
  // reachable.
  const tree = new ConstructsTreeProvider({
    connections: fakeManager(),
    connectionContext: () => ({ clusterSelected: true, connected: false }),
    load: async (): Promise<CatalogState> => ({ kind: "unreachable" }),
  });
  const rows = await tree.getChildren();
  assert.equal(rows.length, 1);
  assert.equal(rows[0].kind, "state");
  const item = tree.getTreeItem(rows[0]);
  assert.equal(item.label, "Cluster not answering");
  assert.ok(
    !String(item.label).startsWith("Not connected"),
    "the unreachable row duplicates the welcome's sentence"
  );
});

test("Constructs renders its groups when connected", async () => {
  // The positive control. Without it every assertion above would pass on a
  // provider that returned nothing under all conditions.
  const tree = new ConstructsTreeProvider({
    connections: fakeManager(),
    connectionContext: () => SELECTED,
    load: async (): Promise<CatalogState> => ({
      kind: "loaded",
      total: 1,
      groups: [
        {
          kind: "query",
          label: "queries",
          count: 1,
          runnable: true,
          namespaces: [{ namespace: "identity", constructs: [] }],
        },
      ],
    }),
  });
  const rows = await tree.getChildren();
  assert.deepEqual(rows.map((r) => r.kind), ["group"]);
});

// ---------------------------------------------------------------------------
// Data
// ---------------------------------------------------------------------------

test("Data returns [] with nothing selected", async () => {
  const tree = new DataTreeProvider(fakeManager(), () => NOTHING);
  assert.deepEqual(await tree.getChildren(), []);
});

test("Data with a cluster selected but no query client is still empty, not an error row", async () => {
  // A selected cluster whose transport is down has no `query`, and the cache
  // then loads nothing. That is an empty CLUSTER as far as this view can tell,
  // and it must not manufacture an error row about it -- the connection surface
  // owns that story, and `cachedError` is reserved for a listConcepts that
  // actually failed.
  const tree = new DataTreeProvider(fakeManager(), () => ({
    clusterSelected: true,
    connected: false,
  }));
  assert.deepEqual(await tree.getChildren(), []);
});

// ---------------------------------------------------------------------------
// Deployments
// ---------------------------------------------------------------------------

function deploymentsDeps(context: ConnectionContextKeys, over: Record<string, unknown> = {}) {
  return {
    clustersPath: "/nowhere/clusters.yaml",
    receiptPath: "/nowhere/install-receipt.json",
    runsDir: "/nowhere/runs",
    presence: async () => ({
      verdict: "absent" as const,
      evidence: { receipt: false, registry: false },
      endpoint: "",
    }),
    readClusters: async () => ({ ok: true as const, file: { clusters: [], selectedCluster: "" } }),
    readReceiptFile: async () => null,
    listRunsIn: async () => [],
    connection: () => undefined,
    readDeployments: () => undefined,
    connectionContext: () => context,
    ...over,
  } as unknown as ConstructorParameters<typeof DeploymentsTreeProvider>[0];
}

test("Deployments returns [] with nothing selected, and probes nothing", async () => {
  let probes = 0;
  const descriptions: string[] = [];
  const contexts: string[] = [];
  const tree = new DeploymentsTreeProvider(
    deploymentsDeps(NOTHING, {
      presence: async () => {
        probes += 1;
        return { verdict: "absent", evidence: { receipt: false, registry: false }, endpoint: "" };
      },
      setDescription: (value: string) => descriptions.push(value),
      setInstanceContext: (value: string) => contexts.push(value),
    })
  );
  assert.deepEqual(await tree.getChildren(), []);
  // A machine nobody selected a cluster on does no front-door probe, no receipt
  // read and no `git ls-remote`. The old view did all three on every repaint.
  assert.equal(probes, 0, "an unselected Deployments view probed the machine");
  // Both sinks are written with the empty value rather than left alone: a
  // description or a menu scope left over from the previous selection is a
  // heading, and a set of buttons, naming the wrong machine.
  assert.deepEqual(descriptions, [""]);
  assert.deepEqual(contexts, [""]);
});

test("a deployment row carries the command that opens it", async () => {
  // THE DEFECT memql#4427 IS ABOUT. Run rows carried no `command` at all, so
  // clicking one did nothing -- the most direct way there is to teach an
  // operator that a view is decorative. The row must also carry the INSTANCE
  // alongside the run, because the detail page's buttons are the instance's
  // role-gated set contextualised by what the run did, and there is no second
  // catalog of run-scoped verbs to fall back on.
  const tree = new DeploymentsTreeProvider(
    deploymentsDeps(SELECTED, {
      readClusters: async () => ({
        ok: true as const,
        file: {
          clusters: [{ name: "local", endpoint: "", domain: "memql.test", local: true }],
          selectedCluster: "local",
        },
      }),
      connection: () => ({ clusterName: "local", connected: true }),
      listRunsIn: async () => [
        {
          id: "run-1",
          instance: "local",
          kind: "upgrade" as const,
          startedAt: "2026-08-14T10:00:00Z",
          status: "succeeded" as const,
          items: [],
        },
      ],
    })
  );
  const rows = await tree.getChildren();
  assert.deepEqual(rows.map((row) => row.kind), ["run"]);
  const item = tree.getTreeItem(rows[0]);
  assert.equal(item.command?.command, "memql.deployments.openRun");
  assert.deepEqual(item.command?.arguments, [rows[0]]);
  assert.equal(
    (item.command?.arguments?.[0] as { instance?: string } | undefined)?.instance,
    "local",
    "the row does not name the instance its run belongs to"
  );
});

test("the view description names the selected cluster, and clears with the selection", async () => {
  // The instance facts, promoted out of the wrapper row. Written on every pass
  // including when it is "", because the selection changes without this view
  // being told and a heading left over from the previous cluster names the
  // wrong machine.
  const descriptions: string[] = [];
  let selected = true;
  const tree = new DeploymentsTreeProvider(
    deploymentsDeps(SELECTED, {
      connectionContext: () => (selected ? SELECTED : NOTHING),
      readClusters: async () => ({
        ok: true as const,
        file: {
          clusters: [{ name: "local", endpoint: "", domain: "memql.test", local: true }],
          selectedCluster: "local",
        },
      }),
      connection: () => (selected ? { clusterName: "local", connected: true } : undefined),
      // Healthy, with no receipt behind it: the version is genuinely
      // unresolvable, and the heading prints the WORD rather than falling
      // silent -- `displayVersion`'s rule, which a blank would turn into a
      // claim that the cluster has no version.
      presence: async () => ({
        verdict: "installed-healthy" as const,
        evidence: { receipt: true, registry: true },
        endpoint: "",
      }),
      setDescription: (value: string) => descriptions.push(value),
    })
  );
  await tree.getChildren();
  assert.equal(descriptions.at(-1), "local · healthy · unknown");

  selected = false;
  tree.refresh();
  await tree.getChildren();
  assert.equal(descriptions.at(-1), "", "the heading survived the selection being dropped");
});

test("Deployments has no instance rows left to render", async () => {
  // The reversal of deploymentsTree.ts's old rule 2, asserted. A machine with
  // NO local cluster used to show a `local` row carrying Create deployment;
  // that row is what suppressed the welcome, and the entry point it protected
  // is now in the welcome itself and in the view title menu.
  const tree = new DeploymentsTreeProvider(deploymentsDeps(NOTHING));
  const rows = await tree.getChildren();
  assert.deepEqual(rows, [], "an absent local cluster still produced a row");
});

// ---------------------------------------------------------------------------
// Runs -- the stated exception
// ---------------------------------------------------------------------------

test("Runs keeps listing the workspace's own configurations with nothing selected", async () => {
  // THE EXCEPTION, driven. These rows are the developer's file; emptying the
  // view would read as data loss. The provider takes no connection dependency
  // at all, which is the strongest form the claim can take -- it CANNOT be
  // gated by accident.
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-runs-"));
  try {
    await fs.mkdir(path.join(dir, ".memql"), { recursive: true });
    await fs.writeFile(
      path.join(dir, ".memql", "runs.json"),
      JSON.stringify({
        version: 1,
        runs: [{ name: "smoke", kind: "query", construct: "identity.me", args: {} }],
      }),
      "utf8"
    );
    const tree = new RunsTreeProvider(dir);
    const rows = await tree.getChildren();
    assert.deepEqual(
      rows.map((row) => row.kind),
      ["run"],
      "the Runs view hid a developer's own saved configuration"
    );
  } finally {
    await fs.rm(dir, { recursive: true, force: true });
  }
});
