// ConnectionManager tests.
//
// ConnectionManager owns the extension's single live cluster connection and
// carries the only concurrency logic in the connection layer: a generation
// counter that stops a slow, superseded dial from clobbering the current
// cluster's state. It is built free of `vscode` imports specifically so it
// can be driven here with a fake DialFn instead of a real WebSocket
// handshake -- these tests never touch the network or the real SDK.

import test from "node:test";
import assert from "node:assert/strict";

import { ConnectionManager, type ConnectionState, type DialFn } from "../src/connection/manager.js";
import type { ClusterConfig } from "../src/clusters/model.js";
import type { Connection } from "@znasllc-io/memql-sdk-core/client";

function cluster(name: string, overrides: Partial<ClusterConfig> = {}): ClusterConfig {
  return { name, endpoint: "cockpit.local.znas.io:443", pat: "mql_pat_x", ...overrides };
}

// A fake connection satisfying just what ConnectionManager touches
// (nodeId, close()). Cast to Connection (a class with private fields, so
// TS would otherwise reject a plain object literal here) -- standard for a
// test double standing in for a class type.
function fakeConn(nodeId: string): Connection & { wasClosed: boolean } {
  const conn = {
    nodeId,
    wasClosed: false,
    close(): void {
      conn.wasClosed = true;
    },
  };
  return conn as unknown as Connection & { wasClosed: boolean };
}

function deferred<T>(): {
  promise: Promise<T>;
  resolve: (v: T) => void;
  reject: (e: unknown) => void;
} {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

// Flushes the microtask queue `n` times. connect()'s internal awaits
// (closeCurrent, then the injected dial) each resolve after exactly one
// microtask tick when the underlying promise is already settled; a few
// extra flushes are harmless no-ops.
async function flush(n = 3): Promise<void> {
  for (let i = 0; i < n; i++) await Promise.resolve();
}

test("A-then-B supersede: A's late resolution closes A and does not clobber B's state", async () => {
  const dials: Array<ReturnType<typeof deferred<Connection>>> = [];
  const dial: DialFn = () => {
    const d = deferred<Connection>();
    dials.push(d);
    return d.promise;
  };
  const manager = new ConnectionManager(dial);

  const connectA = manager.connect(cluster("a"));
  await flush();
  assert.deepEqual(manager.state, { status: "connecting", clusterName: "a" });

  const connectB = manager.connect(cluster("b"));
  await flush();
  assert.deepEqual(manager.state, { status: "connecting", clusterName: "b" });

  // Resolve A's dial AFTER B has superseded it.
  const connA = fakeConn("A");
  dials[0].resolve(connA);
  await connectA;

  assert.equal(connA.wasClosed, true, "A's connection must be closed rather than adopted");
  assert.deepEqual(
    manager.state,
    { status: "connecting", clusterName: "b" },
    "A's late resolution must not publish over B's current state",
  );

  const connB = fakeConn("B");
  dials[1].resolve(connB);
  await connectB;

  assert.deepEqual(manager.state, { status: "connected", clusterName: "b", nodeId: "B" });
  assert.equal(connB.wasClosed, false);
});

test("issuing connect(B) immediately after connect(A) never publishes a stale connecting/error for A", async () => {
  const dial: DialFn = () => new Promise<Connection>(() => {}); // never resolves
  const manager = new ConnectionManager(dial);
  const seen: ConnectionState[] = [];
  manager.onDidChangeState((s) => seen.push(s));

  void manager.connect(cluster("a"));
  void manager.connect(cluster("b"));
  await flush();

  // A's connect() call was superseded before its own `closeCurrent` await
  // resolved, so it must never have published anything.
  assert.deepEqual(seen, [{ status: "connecting", clusterName: "b" }]);
});

test("a dial rejection publishes the error state for the cluster that was dialing", async () => {
  const dial: DialFn = () => Promise.reject(new Error("boom"));
  const manager = new ConnectionManager(dial);

  await manager.connect(cluster("a"));

  assert.deepEqual(manager.state, { status: "error", clusterName: "a", message: "boom" });
});

test("a non-Error rejection is stringified into the error message", async () => {
  const dial: DialFn = () => Promise.reject("plain string failure");
  const manager = new ConnectionManager(dial);

  await manager.connect(cluster("a"));

  assert.deepEqual(manager.state, {
    status: "error",
    clusterName: "a",
    message: "plain string failure",
  });
});

test("onDidChangeState's disposer stops delivering further state changes", async () => {
  const dial: DialFn = () => Promise.resolve(fakeConn("x"));
  const manager = new ConnectionManager(dial);
  const seen: ConnectionState["status"][] = [];
  const dispose = manager.onDidChangeState((s) => seen.push(s.status));

  await manager.connect(cluster("a"));
  assert.deepEqual(seen, ["connecting", "connected"]);

  dispose();
  await manager.disconnect();

  assert.deepEqual(seen, ["connecting", "connected"], "disposed listener must not fire again");
  assert.deepEqual(manager.state, { status: "disconnected" });
});

test("an OIDC-only cluster (no PAT) produces the cockpit-authentication message without dialing", async () => {
  let dialed = false;
  const dial: DialFn = () => {
    dialed = true;
    return Promise.resolve(fakeConn("x"));
  };
  const manager = new ConnectionManager(dial);

  await manager.connect(
    cluster("oidc", { pat: undefined, issuer: "https://issuer.example.com", clientId: "abc" }),
  );

  assert.equal(dialed, false, "an OIDC-only cluster must never reach the dial call");
  assert.deepEqual(manager.state, {
    status: "error",
    clusterName: "oidc",
    message:
      "This cluster is configured for OIDC. Authenticate it in the memQL Cockpit first, or add a PAT to clusters.yaml.",
  });
});

test("an unconfigured cluster (no endpoint) produces the generic not-configured message", async () => {
  let dialed = false;
  const dial: DialFn = () => {
    dialed = true;
    return Promise.resolve(fakeConn("x"));
  };
  const manager = new ConnectionManager(dial);

  await manager.connect(cluster("empty", { endpoint: "", pat: undefined }));

  assert.equal(dialed, false);
  assert.deepEqual(manager.state, {
    status: "error",
    clusterName: "empty",
    message: "This cluster is not configured. Set an endpoint and a PAT.",
  });
});

test("disconnect() closes the live connection and publishes disconnected", async () => {
  const conn = fakeConn("x");
  const dial: DialFn = () => Promise.resolve(conn);
  const manager = new ConnectionManager(dial);

  await manager.connect(cluster("a"));
  assert.equal(manager.state.status, "connected");

  await manager.disconnect();

  assert.equal(conn.wasClosed, true);
  assert.deepEqual(manager.state, { status: "disconnected" });
});
