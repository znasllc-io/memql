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

// A live JWT by default. The bearer's CLASS and its `exp` are now load-bearing
// (memql#3383 / memql#3385): ConnectionManager resolves the credential before
// it dials, so a fixture carrying the old `mql_pat_x` would be refused rather
// than dialed, and every unrelated test here would fail for the wrong reason.
function liveJwt(secondsFromNow = 3600): string {
  const b64 = (v: unknown): string => Buffer.from(JSON.stringify(v)).toString("base64url");
  return `${b64({ alg: "RS256" })}.${b64({ sub: "u", exp: Math.floor(Date.now() / 1000) + secondsFromNow })}.sig`;
}

function cluster(name: string, overrides: Partial<ClusterConfig> = {}): ClusterConfig {
  return { name, endpoint: "api.memql.localhost:443", token: liveJwt(), ...overrides };
}

// A fake connection satisfying just what ConnectionManager touches
// (nodeId, query, subscriptions, close(), done()). Cast to Connection (a class
// with private fields, so TS would otherwise reject a plain object literal
// here) -- standard for a test double standing in for a class type.
//
// done() mirrors the real semantics deliberately: Connection.done() delegates
// to Dispatcher.done(), which resolves on ANY termination -- including the
// dispatcher stop that our own close() triggers. A double whose done() only
// fired on a server drop would make the "deliberate teardown must not publish
// a lost-connection error" tests below vacuous.
type FakeConn = Connection & {
  wasClosed: boolean;
  // Simulates a SERVER-side drop: done() resolves with no close() call.
  terminate(): void;
};

function fakeConn(nodeId: string, engineVersion = ""): FakeConn {
  let resolveDone!: () => void;
  const donePromise = new Promise<void>((resolve) => {
    resolveDone = resolve;
  });
  const conn = {
    nodeId,
    // What ServerHello stated (memql#3998). "" is the default because that is
    // what an engine predating the field states, which is every cluster
    // installed before it.
    engineVersion,
    wasClosed: false,
    // Identity markers so a test can assert WHICH connection the manager is
    // handing out (or that it is handing out none).
    query: { conn: nodeId },
    subscriptions: { conn: nodeId },
    close(): void {
      conn.wasClosed = true;
      resolveDone();
    },
    done(): Promise<void> {
      return donePromise;
    },
    terminate(): void {
      resolveDone();
    },
  };
  return conn as unknown as FakeConn;
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
// (closeCurrent, the CREDENTIAL RESOLUTION, then the injected dial) each
// resolve after a tick or two when the underlying promise is already settled;
// extra flushes are harmless no-ops.
//
// The default has headroom deliberately. Credential resolution (memql#3383)
// added an await between closeCurrent and the dial, so the number of ticks
// before "connecting" is published is no longer something a reader should have
// to count -- and a too-tight flush would fail as a confusing assertion about
// state rather than as a timing problem.
async function flush(n = 8): Promise<void> {
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

  assert.deepEqual(manager.state, {
    status: "error",
    clusterName: "a",
    message: "boom",
    reason: "unreachable",
  });
});

test("a non-Error rejection is stringified into the error message", async () => {
  const dial: DialFn = () => Promise.reject("plain string failure");
  const manager = new ConnectionManager(dial);

  await manager.connect(cluster("a"));

  assert.deepEqual(manager.state, {
    status: "error",
    clusterName: "a",
    message: "plain string failure",
    reason: "unreachable",
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

test("a credential-less OIDC cluster reports a MISSING CREDENTIAL, without dialing", async () => {
  // It used to say "authenticate it in the MemQL Cockpit first" -- an
  // instruction that does not produce a credential this extension can then use
  // (memql#3383). An issuer and a client id are not a token; the honest report
  // is that there is no credential, and what kind to supply.
  let dialed = false;
  const dial: DialFn = () => {
    dialed = true;
    return Promise.resolve(fakeConn("x"));
  };
  const manager = new ConnectionManager(dial);

  await manager.connect(
    cluster("oidc", { token: undefined, issuer: "https://issuer.example.com", clientId: "abc" }),
  );

  assert.equal(dialed, false, "a cluster with no credential must never reach the dial call");
  assert.equal(manager.state.status, "error");
  const state = manager.state as { message: string; reason: string };
  assert.equal(state.reason, "missingCredential");
  assert.doesNotMatch(state.message, /Cockpit/);
  assert.match(state.message, /JWT access token/);
});

test("a PAT is refused by name, without dialing", async () => {
  // THE memql#3383 acceptance item at the connection layer: a wrong-class token
  // must produce an actionable message, not a bare handshake failure.
  let dialed = false;
  const manager = new ConnectionManager(() => {
    dialed = true;
    return Promise.resolve(fakeConn("x"));
  });

  await manager.connect(cluster("local", { token: "mql_pat_abcdef" }));

  assert.equal(dialed, false);
  const state = manager.state as { status: string; message: string; reason: string };
  assert.equal(state.reason, "wrongTokenClass");
  assert.match(state.message, /Personal Access Token/);
});

test("an expired token with no refresh path reports credentialExpired, not a transport failure", async () => {
  const manager = new ConnectionManager(() => Promise.resolve(fakeConn("x")));

  await manager.connect(cluster("local", { token: liveJwt(-60) }));

  const state = manager.state as { reason: string };
  assert.equal(state.reason, "credentialExpired");
});

test("the dial carries an onTokenExpired hook so a LIVE stream can re-auth in place", async () => {
  // sdk/ts arms a timer against the bearer's exp and calls this hook shortly
  // before it runs out, rotating over the existing socket. That is what lets a
  // long session outlive a 15-minute access token WITHOUT a reconnect -- and
  // therefore without dropping the session-defined constructs a reconnect takes
  // with it (memql#3385).
  let hook: (() => Promise<string | null>) | undefined;
  const manager = new ConnectionManager(
    (opts) => {
      hook = opts.auth?.onTokenExpired;
      return Promise.resolve(fakeConn("x"));
    },
    {
      resolve: async () => ({ ok: true, bearer: "FIRST" }),
      forceRefresh: async () => "ROTATED",
    },
  );

  await manager.connect(cluster("local"));

  assert.ok(hook, "the dial must supply a re-auth hook");
  assert.equal(await hook(), "ROTATED");
});

test("an unconfigured cluster (no endpoint) produces the generic not-configured message", async () => {
  let dialed = false;
  const dial: DialFn = () => {
    dialed = true;
    return Promise.resolve(fakeConn("x"));
  };
  const manager = new ConnectionManager(dial);

  await manager.connect(cluster("empty", { endpoint: "", token: undefined }));

  assert.equal(dialed, false);
  const state = manager.state as { status: string; message: string; reason: string };
  assert.equal(state.status, "error");
  assert.equal(state.reason, "notConfigured");
  assert.match(state.message, /not configured/);
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

// --- Connection death (the manager observing Connection.done()) -------------
//
// Before this was wired, a server-side drop was completely invisible: every
// view kept reporting "Connected", `get query()` kept handing out a client
// over a dead socket, and the concept browser's CDC subscription was silently
// gone with no notice. The generation counter is what makes observing done()
// safe -- done() also fires for OUR OWN close(), so every deliberate teardown
// resolves it too.

test("a server-side drop publishes a non-connected state", async () => {
  const conn = fakeConn("x");
  const manager = new ConnectionManager(() => Promise.resolve(conn));
  const seen: ConnectionState[] = [];
  manager.onDidChangeState((s) => seen.push(s));

  await manager.connect(cluster("a"));
  assert.equal(manager.state.status, "connected");

  conn.terminate(); // the socket dies underneath us
  await flush();

  assert.notEqual(
    manager.state.status,
    "connected",
    "a dropped connection must not keep reporting Connected",
  );
  assert.deepEqual(manager.state, {
    status: "error",
    clusterName: "a",
    message: "Connection to a was lost. Select the cluster again to reconnect.",
    reason: "lost",
  });
  assert.equal(seen.at(-1)?.status, "error", "the drop must be published to listeners");
});

test("a dropped connection stops handing out query and subscription clients", async () => {
  const conn = fakeConn("x");
  const manager = new ConnectionManager(() => Promise.resolve(conn));

  await manager.connect(cluster("a"));
  assert.ok(manager.query, "a live connection must expose a query client");
  assert.ok(manager.subscriptions);

  conn.terminate();
  await flush();

  assert.equal(manager.query, undefined, "a dead socket must not back a query client");
  assert.equal(manager.subscriptions, undefined);
});

test("the engine version the handshake stated is readable off the manager", async () => {
  // The fifth version source (memql#4018) reads this. It is exposed as a getter
  // over `conn` rather than cached, for the same reason `query` is: the manager
  // drops the connection the instant the socket dies, and a cached build fact
  // would go on describing a cluster this editor is no longer talking to.
  const manager = new ConnectionManager(() => Promise.resolve(fakeConn("x", "v0.19.0")));
  assert.equal(manager.engineVersion, undefined, "nothing is connected yet");

  await manager.connect(cluster("a"));
  assert.equal(manager.engineVersion, "v0.19.0");

  await manager.disconnect();
  assert.equal(manager.engineVersion, undefined, "a torn-down connection states nothing");
});

test("an engine predating the handshake field reports an empty string, not undefined", async () => {
  // The two answers mean different things and the version collector relies on
  // the distinction being preserved: "" is "this cluster answered and it does
  // not carry the field", undefined is "there is no connection to ask".
  const manager = new ConnectionManager(() => Promise.resolve(fakeConn("x")));
  await manager.connect(cluster("a"));
  assert.equal(manager.engineVersion, "");
});

test("a done() from a superseded connection cannot clobber the newer connection", async () => {
  const connA = fakeConn("A");
  const connB = fakeConn("B");
  const queue = [connA, connB];
  const manager = new ConnectionManager(() => Promise.resolve(queue.shift()!));

  await manager.connect(cluster("a"));
  await manager.connect(cluster("b"));
  assert.deepEqual(manager.state, { status: "connected", clusterName: "b", nodeId: "B" });

  // connect(B) already closed A, which resolves A's done(). Fire it again for
  // good measure -- either way it is a done() for a superseded connection.
  connA.terminate();
  await flush();

  assert.deepEqual(
    manager.state,
    { status: "connected", clusterName: "b", nodeId: "B" },
    "A's termination must not publish over B's state",
  );
  assert.ok(manager.query, "B's query client must survive A's termination");
});

test("a superseded connection's done() does not clear the newer connection's clients", async () => {
  // The narrow case the `this.conn !== conn` guard exists for: prove the
  // manager clears only the connection that actually died.
  const connA = fakeConn("A");
  const connB = fakeConn("B");
  const queue = [connA, connB];
  const manager = new ConnectionManager(() => Promise.resolve(queue.shift()!));

  await manager.connect(cluster("a"));
  await manager.connect(cluster("b"));
  connA.terminate();
  await flush();

  assert.deepEqual(
    manager.query,
    { conn: "B" },
    "the surviving connection's query client must still be B's",
  );
});

test("disconnect()'s own teardown does not publish a lost-connection error", async () => {
  const conn = fakeConn("x");
  const manager = new ConnectionManager(() => Promise.resolve(conn));

  await manager.connect(cluster("a"));
  await manager.disconnect();
  // disconnect() closed the connection, which resolves done(). That must not
  // overwrite the "disconnected" it just published.
  await flush();

  assert.deepEqual(manager.state, { status: "disconnected" });
});

test("a reconnect to the same cluster is not undone by the previous connection's done()", async () => {
  const first = fakeConn("first");
  const second = fakeConn("second");
  const queue = [first, second];
  const manager = new ConnectionManager(() => Promise.resolve(queue.shift()!));

  await manager.connect(cluster("a"));
  await manager.connect(cluster("a")); // same cluster name, new socket
  await flush();

  assert.deepEqual(manager.state, { status: "connected", clusterName: "a", nodeId: "second" });
  assert.equal(first.wasClosed, true);
  assert.deepEqual(manager.query, { conn: "second" });
});

test("a credential-less cluster with no endpoint reports 'not configured', not a credential problem", async () => {
  // Nowhere to dial outranks nothing to dial with: "supply a JWT" is the wrong
  // instruction for a cluster that has no address.
  let dialed = false;
  const dial: DialFn = () => {
    dialed = true;
    return Promise.resolve(fakeConn("x"));
  };
  const manager = new ConnectionManager(dial);

  await manager.connect(
    cluster("oidc-no-endpoint", {
      endpoint: "",
      token: undefined,
      issuer: "https://issuer.example.com",
      clientId: "abc",
    }),
  );

  assert.equal(dialed, false);
  const state = manager.state as { message: string; reason: string };
  assert.equal(state.reason, "notConfigured");
  assert.match(state.message, /not configured/);
});

// --- The reactive 401: the cluster refused a bearer we believed was good -----
//
// memql#3529. `src/connection/credentials.ts` carries the PROACTIVE half --
// it renews a token it can SEE is near expiry. These cases are the other half:
// the token's `exp` is comfortably in the future, and the cluster rejects it
// anyway. That happens whenever the reason is not time -- the session was
// revoked, the cluster was signed out elsewhere, identity's signing keys
// rotated, a replica is serving a keyset the token was not minted against
// (memql#3400).
//
// Until this was wired, `connect()` classified a failed dial by re-reading the
// bearer's own `exp`, so every one of those arrived at the operator as
// `unreachable` with the raw text `Unexpected server response: 401` -- which
// sends them to look at a cluster that is fine. `runAuthenticated`
// (src/auth/store.ts) was written for exactly this and was imported by nothing
// but its own unit test.

/** A bearer-rejecting dial: the shape `ws` actually produces on a refused upgrade. */
function refusedUpgrade(): Error {
  return new Error("Unexpected server response: 401");
}

test("a 401 on an unexpired bearer refreshes once and retries once", async () => {
  const attempts: string[] = [];
  const manager = new ConnectionManager(
    (opts) => {
      attempts.push(opts.auth?.bearer ?? "");
      return attempts.length === 1 ? Promise.reject(refusedUpgrade()) : Promise.resolve(fakeConn("x"));
    },
    {
      resolve: async () => ({ ok: true, bearer: "STALE" }),
      forceRefresh: async () => "ROTATED",
    },
  );

  await manager.connect(cluster("a"));

  assert.deepEqual(attempts, ["STALE", "ROTATED"], "the retry must carry the REFRESHED bearer");
  assert.equal(manager.state.status, "connected");
});

test("a 401 that survives a freshly minted token asks for a new sign-in, not a cluster hunt", async () => {
  // The retry is bounded at one on purpose: a 401 that survives a fresh access
  // token is not a stale-token problem, and spinning past it is how a client
  // ends up replaying a dead credential forever.
  let attempts = 0;
  const manager = new ConnectionManager(
    () => {
      attempts += 1;
      return Promise.reject(refusedUpgrade());
    },
    {
      resolve: async () => ({ ok: true, bearer: "STALE" }),
      forceRefresh: async () => "ALSO-REFUSED",
    },
  );

  await manager.connect(cluster("a"));

  assert.equal(attempts, 2, "exactly one retry");
  const state = manager.state as { status: string; message: string; reason: string };
  assert.equal(state.status, "error");
  assert.equal(state.reason, "reauthenticationRequired");
  assert.match(state.message, /sign in/i);
  assert.doesNotMatch(state.message, /unreachable/i);
});

test("a terminal 401 clears the stored tokens so the next action starts clean", async () => {
  // Leaving the dead credential on disk is what makes the NEXT action fail the
  // same way. signOut() is the seam: it clears SecretStorage and blanks both
  // token fields in clusters.yaml.
  const writes: Array<{ name: string; token?: string; refreshToken?: string }> = [];
  const manager = new ConnectionManager(
    () => Promise.reject(refusedUpgrade()),
    {
      resolve: async () => ({ ok: true, bearer: "STALE" }),
      forceRefresh: async () => null, // the exchange itself was refused
    },
    {
      writeCluster: async (update) => {
        writes.push(update);
      },
    },
  );

  await manager.connect(cluster("a"));

  assert.equal((manager.state as { reason: string }).reason, "reauthenticationRequired");
  assert.deepEqual(
    writes,
    [{ name: "a", token: "", refreshToken: "" }],
    "a terminal rejection must blank the credential it just proved dead",
  );
});

test("a NON-401 dial failure is still a transport failure, and is not retried", async () => {
  // The guard on the whole mechanism. Relabelling "the cluster is down" as
  // "sign in again" would be the same defect pointing the other way, and a
  // refresh + retry against an unreachable cluster is two timeouts instead of
  // one.
  let attempts = 0;
  let refreshed = false;
  const manager = new ConnectionManager(
    () => {
      attempts += 1;
      return Promise.reject(new Error("connect ECONNREFUSED 127.0.0.1:443"));
    },
    {
      resolve: async () => ({ ok: true, bearer: "GOOD" }),
      forceRefresh: async () => {
        refreshed = true;
        return "ROTATED";
      },
    },
  );

  await manager.connect(cluster("a"));

  assert.equal(attempts, 1, "a transport failure must not be retried");
  assert.equal(refreshed, false, "and must not spend a refresh");
  const state = manager.state as { status: string; reason: string; message: string };
  assert.equal(state.reason, "unreachable");
  assert.match(state.message, /ECONNREFUSED/);
});

test("a cluster whose credential never resolves still never reaches the dial", async () => {
  // runAuthenticated resolves the credential itself, so the pre-dial refusals
  // (memql#3383) now run inside it. They must behave exactly as before:
  // no dial, no "connecting" state, and the reason preserved.
  let dialed = false;
  const seen: ConnectionState[] = [];
  const manager = new ConnectionManager(() => {
    dialed = true;
    return Promise.resolve(fakeConn("x"));
  });
  manager.onDidChangeState((s) => seen.push(s));

  await manager.connect(cluster("local", { token: "mql_pat_abcdef" }));

  assert.equal(dialed, false);
  assert.equal(
    seen.some((s) => s.status === "connecting"),
    false,
    "a credential refused before the dial must never show the cluster as connecting",
  );
  assert.equal((manager.state as { reason: string }).reason, "wrongTokenClass");
});

// ---------------------------------------------------------------------------
// the context keys the manager publishes (memql#4424)
// ---------------------------------------------------------------------------
//
// WHY THEY ARE ASSERTED HERE rather than beside the mapping table. The mapping
// is pure and tested in test/connectionContext.test.ts; what cannot be checked
// there is that it is actually PUBLISHED, and published from every path that
// changes the answer. `publish` is the single funnel every state change goes
// through -- a select, a refused credential, a disconnect, a sign-out (which
// disconnects), a socket dying under `watchForTermination` -- and this is the
// evidence that it is, driven through the real state machine rather than
// asserted about it.
//
// The consequence of a missed path is not an error anywhere: the keys simply go
// stale, and three views keep showing a welcome over a cluster the editor is
// holding, or keep showing rows for one it lost.

test("the keys are published on activation, before anything has connected", async () => {
  // A key VS Code has never been told about is UNSET, and unset is falsy in a
  // `when` clause -- so the welcomes would render on a fresh window by
  // accident. Stating both up front means the keys describe the extension from
  // its first frame.
  const keys: Array<{ clusterSelected: boolean; connected: boolean }> = [];
  new ConnectionManager(() => Promise.resolve(fakeConn("n1")), undefined, undefined, (k) =>
    keys.push(k),
  );
  assert.deepEqual(keys, [{ clusterSelected: false, connected: false }]);
});

test("the keys follow a connect, a drop and a disconnect", async () => {
  const conn = fakeConn("n1");
  const keys: Array<{ clusterSelected: boolean; connected: boolean }> = [];
  const manager = new ConnectionManager(
    () => Promise.resolve(conn),
    undefined,
    undefined,
    (k) => keys.push(k),
  );

  await manager.connect(cluster("local"));
  await flush();
  assert.deepEqual(
    keys.at(-1),
    { clusterSelected: true, connected: true },
    "a held connection did not publish connected",
  );
  // "connecting" passed through on the way, and it is selected-but-not-yet-up:
  // the views render their normal shape rather than a welcome while a dial is
  // in flight.
  assert.ok(
    keys.some((k) => k.clusterSelected && !k.connected),
    "the dial-in-flight state never reached the keys",
  );

  // A SERVER-SIDE DROP. Nothing else notices a connection dying; without the
  // publish inside watchForTermination the keys would still say `connected`
  // while `query` hands out nothing.
  conn.terminate();
  await flush();
  assert.deepEqual(
    keys.at(-1),
    { clusterSelected: true, connected: false },
    "a lost connection left memql.connected true",
  );

  await manager.disconnect();
  assert.deepEqual(
    keys.at(-1),
    { clusterSelected: false, connected: false },
    "a disconnect did not empty the cluster-backed views",
  );
});

test("a refused dial leaves the cluster SELECTED and not connected", async () => {
  // Design D2's distinction, at its source. A cluster that was chosen and did
  // not answer must not empty the views: it is a fact about something, carried
  // by each view's own row and description affordances. Publishing
  // `clusterSelected: false` here would replace a cluster that is down with a
  // screen saying nothing is chosen.
  const keys: Array<{ clusterSelected: boolean; connected: boolean }> = [];
  const manager = new ConnectionManager(
    () => Promise.reject(new Error("no route to host")),
    undefined,
    undefined,
    (k) => keys.push(k),
  );
  await manager.connect(cluster("staging"));
  await flush();
  assert.equal(manager.state.status, "error");
  assert.deepEqual(keys.at(-1), { clusterSelected: true, connected: false });
});

test("a manager built with no sink still connects", async () => {
  // The default is a no-op, so every other test in this file -- and any bare
  // construction -- gets a manager that works and simply has no editor to tell.
  const manager = new ConnectionManager(() => Promise.resolve(fakeConn("n1")));
  await manager.connect(cluster("local"));
  assert.equal(manager.state.status, "connected");
});
