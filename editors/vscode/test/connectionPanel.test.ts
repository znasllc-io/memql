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

/**
 * Waits for the page to reach a state, on real timers.
 *
 * NOT A FIXED NUMBER OF TICKS. The page reads the cluster list from disk before
 * it renders, and how many turns of the event loop a real file read takes
 * depends on the machine: twenty setImmediate turns was enough on a laptop and
 * not on a CI runner, where these tests read an unopened page. So each test
 * waits for the thing it is about to assert on, and fails naming it.
 */
async function waitFor<T>(probe: () => T | undefined | false, what: string, ms = 5000): Promise<T> {
  const deadline = Date.now() + ms;
  for (;;) {
    const value = probe();
    if (value !== undefined && value !== false) return value;
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 5));
  }
}

/** The page's webview once it has rendered its first document. */
function openedPage(): Promise<NonNullable<(typeof recorded.webviews)[number]>> {
  return waitFor(() => {
    const webview = recorded.webviews.at(-1);
    return webview !== undefined && webview.html !== "" ? webview : undefined;
  }, "the page to render");
}

test("the page renders the cluster's state, and a state change patches rather than repaints", async () => {
  resetRecorded();
  const conns = fakeConnections({ status: "disconnected" });
  const panel = ConnectionPanel.open(CONTEXT, deps(conns.manager, clustersFile()), "local");
  const webview = await openedPage();
  await waitFor(() => /memql\.localhost/.test(webview.html), "the page to name the cluster");
  assert.match(webview.html, /memql\.localhost/);
  assert.match(webview.html, /data-act="signIn"/, "Sign in is the page's act when nothing is stored");

  webview.send({ type: "ready" });
  const documents = webview.renders;
  conns.set({ status: "connecting", clusterName: "local" });
  await waitFor(() => webview.posted.some((m) => (m as { type?: string }).type === "patch"), "a patch");
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
  const webview = await openedPage();
  await waitFor(() => /data-act="disconnect"/.test(webview.html) || undefined, "the connected page");
  webview.send({ type: "disconnect" });
  const at = await waitFor(() => {
    const i = recorded.executed.lastIndexOf("memql.clusters.disconnect");
    return i >= 0 ? i : undefined;
  }, "the page to ask to disconnect").catch(() => -1);
  assert.ok(at >= 0, "the page did not ask to disconnect");
  const node = recorded.executedArgs[at]?.[0] as { cluster?: { name?: string } } | undefined;
  assert.equal(node?.cluster?.name, "local", "disconnect was not scoped to the page's cluster");
  webview.close();
});

test("the page's acts reach their commands with the page's cluster", async () => {
  resetRecorded();
  const conns = fakeConnections({ status: "disconnected" });
  ConnectionPanel.open(CONTEXT, deps(conns.manager, clustersFile()), "local");
  const webview = await openedPage();
  // The first document is the page's loading shape, which offers no act; an
  // act posted before the facts are read is ignored, rightly. Wait for them.
  await waitFor(() => /data-act="signIn"/.test(webview.html), "the signed-out page").catch(() => undefined);
  for (const [act, command] of [
    ["signIn", "memql.clusters.signIn"],
    ["signInWithCode", "memql.clusters.signInWithCode"],
    ["edit", "memql.clusters.edit"],
    ["remove", "memql.clusters.remove"],
    ["connect", "memql.clusters.select"],
    ["repair", "memql.clusters.repair"],
  ] as const) {
    const before = recorded.executed.length;
    webview.send({ type: act });
    await waitFor(() => recorded.executed.length > before, `${act} to run a command`).catch(() => undefined);
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
  const webview = await openedPage();
  await waitFor(() => /Waiting for you in the browser/.test(webview.html), "the sign-in state");
  assert.match(webview.html, /Waiting for you in the browser/);
  webview.send({ type: "cancel" });
  await waitFor(() => cancelled.length > 0, "the sign-in to be cancelled").catch(() => undefined);
  await settle();
  assert.deepEqual(cancelled, ["local"]);
  assert.equal(recorded.executed.includes("memql.clusters.disconnect"), false);
  webview.close();
});

test("a cluster no longer in the list says so", async () => {
  resetRecorded();
  const conns = fakeConnections({ status: "disconnected" });
  ConnectionPanel.open(CONTEXT, deps(conns.manager, clustersFile()), "gone");
  const webview = await openedPage();
  await waitFor(() => /This cluster is no longer in your list\./.test(webview.html), "the removed-cluster page").catch(() => undefined);
  assert.match(webview.html, /This cluster is no longer in your list\./);
  webview.close();
});
