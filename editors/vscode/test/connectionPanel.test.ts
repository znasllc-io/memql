// The cluster page's panel (src/webview/connectionPanel.ts), driven through
// the stub webview: one document per cluster, then patches.
//
// THE FIELD FAILURES this pins: the old panel reassigned the whole document
// every 30 seconds and on every state change (focus and scroll lost), and its
// Disconnect button disconnected whichever cluster was live rather than the
// page's own.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

import type { ExtensionContext } from "vscode";

import type { ClusterFacts } from "../src/clusters/facts.js";
import type { ConnectionManager, ConnectionState } from "../src/connection/manager.js";
import { ConnectionPanel, type ConnectionPanelDeps } from "../src/webview/connectionPanel.js";
import { recorded, resetRecorded } from "./support/vscodeStub.js";

const CONTEXT = { subscriptions: [] as { dispose(): unknown }[] } as unknown as ExtensionContext;

const NOTHING: ClusterFacts = { session: false, signedIn: false, ownerSetup: false, consoleUrl: "https://os.memql.localhost/" };

function clustersFile(): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "memql-page-"));
  const file = path.join(dir, "clusters.yaml");
  fs.writeFileSync(
    file,
    [
      "clusters:",
      "  - name: local",
      "    display_name: memql.localhost",
      "    endpoint: api.memql.localhost:443",
      "    domain: memql.localhost",
      "    local: true",
      "selected_cluster: local",
      "",
    ].join("\n"),
  );
  return file;
}

/** A manager stand-in: a state and a listener set, enough for the page. */
function fakeConnections(initial: ConnectionState): {
  manager: ConnectionManager;
  set(state: ConnectionState): void;
} {
  let state = initial;
  const listeners = new Set<(s: ConnectionState) => void>();
  const manager = {
    get state() {
      return state;
    },
    get query() {
      return undefined;
    },
    onDidChangeState(listener: (s: ConnectionState) => void) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
  } as unknown as ConnectionManager;
  return {
    manager,
    set(next) {
      state = next;
      for (const l of listeners) l(next);
    },
  };
}

function deps(connections: ConnectionManager, clustersPath: string, over: Partial<ConnectionPanelDeps> = {}): ConnectionPanelDeps {
  return {
    clustersPath,
    connections,
    factsFor: async () => NOTHING,
    signInFlight: () => undefined,
    cancelSignIn: () => undefined,
    useCode: () => undefined,
    showDetails: () => undefined,
    ...over,
  };
}

async function settle(): Promise<void> {
  for (let i = 0; i < 20; i += 1) await new Promise((r) => setImmediate(r));
}

test("the page renders the cluster's state, and a state change patches rather than repaints", async () => {
  resetRecorded();
  const conns = fakeConnections({ status: "disconnected" });
  const panel = ConnectionPanel.open(CONTEXT, deps(conns.manager, clustersFile()), "local");
  await settle();
  const webview = recorded.webviews.at(-1);
  assert.ok(webview !== undefined);
  assert.match(webview.html, /memql\.localhost/);
  assert.match(webview.html, /data-act="signIn"/, "Sign in is the page's act when nothing is stored");

  webview.send({ type: "ready" });
  const documents = webview.renders;
  conns.set({ status: "connecting", clusterName: "local" });
  await settle();
  assert.equal(webview.renders, documents, "a state change reassigned the whole document");
  const patch = webview.posted.find((m) => (m as { type?: string }).type === "patch") as
    | { regions: Record<string, string> }
    | undefined;
  assert.ok(patch !== undefined, "no patch was posted");
  assert.match(patch.regions.actions ?? "", /Connecting/);
  assert.ok(panel !== undefined);
  webview.close();
});

test("Disconnect on the page is for THIS cluster", async () => {
  resetRecorded();
  const conns = fakeConnections({ status: "connected", clusterName: "local", nodeId: "n" });
  ConnectionPanel.open(CONTEXT, deps(conns.manager, clustersFile()), "local");
  await settle();
  const webview = recorded.webviews.at(-1)!;
  webview.send({ type: "disconnect" });
  await settle();
  const at = recorded.executed.lastIndexOf("memql.clusters.disconnect");
  assert.ok(at >= 0, "the page did not ask to disconnect");
  const node = recorded.executedArgs[at]?.[0] as { cluster?: { name?: string } } | undefined;
  assert.equal(node?.cluster?.name, "local", "disconnect was not scoped to the page's cluster");
  webview.close();
});

test("the page's acts reach their commands with the page's cluster", async () => {
  resetRecorded();
  const conns = fakeConnections({ status: "disconnected" });
  ConnectionPanel.open(CONTEXT, deps(conns.manager, clustersFile()), "local");
  await settle();
  const webview = recorded.webviews.at(-1)!;
  for (const [act, command] of [
    ["signIn", "memql.clusters.signIn"],
    ["signInWithCode", "memql.clusters.signInWithCode"],
    ["edit", "memql.clusters.edit"],
    ["remove", "memql.clusters.remove"],
    ["connect", "memql.clusters.select"],
    ["repair", "memql.clusters.repair"],
  ] as const) {
    webview.send({ type: act });
    await settle();
    assert.equal(recorded.executed.at(-1), command, act);
  }
  webview.close();
});

test("Cancel during a sign-in stops the sign-in, not the connection", async () => {
  resetRecorded();
  const cancelled: string[] = [];
  const conns = fakeConnections({ status: "disconnected" });
  ConnectionPanel.open(
    CONTEXT,
    deps(conns.manager, clustersFile(), {
      signInFlight: () => ({ phase: "waiting", codeOffered: true }),
      cancelSignIn: (name) => cancelled.push(name),
    }),
    "local",
  );
  await settle();
  const webview = recorded.webviews.at(-1)!;
  assert.match(webview.html, /Waiting for you in the browser/);
  webview.send({ type: "cancel" });
  await settle();
  assert.deepEqual(cancelled, ["local"]);
  assert.equal(recorded.executed.includes("memql.clusters.disconnect"), false);
  webview.close();
});

test("a cluster no longer in the list says so", async () => {
  resetRecorded();
  const conns = fakeConnections({ status: "disconnected" });
  ConnectionPanel.open(CONTEXT, deps(conns.manager, clustersFile()), "gone");
  await settle();
  assert.match(recorded.webviews.at(-1)!.html, /This cluster is no longer in your list\./);
  recorded.webviews.at(-1)!.close();
});
