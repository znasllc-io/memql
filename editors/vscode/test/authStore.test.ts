// Where a signed-in cluster's tokens live, and how they are taken away again
// (memql#3404).
//
// The two halves under test are the ones that only exist because of what
// SecretStorage is NOT:
//
//   It cannot be enumerated, so a cluster renamed or deleted in clusters.yaml
//   would strand a thirty-day refresh token under a key nothing could ever
//   find again. The index is that missing enumeration, and every case below
//   that touches rename or delete is really testing the index.
//
//   It is a keyring, so it can be locked, absent, or refuse a write. Every
//   method degrades rather than throwing, because "your keyring is locked"
//   must not become "you cannot connect".
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go):
// SecretStore is a structural interface `vscode.SecretStorage` satisfies, so
// these cases drive a Map.

import test from "node:test";
import assert from "node:assert/strict";

import {
  ClusterCredentialStore,
  accessTokenExpirySecretKey,
  isUnauthorizedError,
  persistSignIn,
  reconcileClusterCredentials,
  refreshTokenSecretKey,
  renameClusterCredentials,
  runAuthenticated,
  signOut,
  type ClusterWriter,
  type SecretStore,
} from "../src/auth/store.js";
import type { ClusterUpdate } from "../src/clusters/file.js";
import type { ClusterConfig } from "../src/clusters/model.js";
import type {
  CredentialResolution,
  CredentialSource,
} from "../src/connection/credentials.js";

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

const NOW_SECONDS = 1_800_000_000;

class FakeSecrets implements SecretStore {
  readonly values = new Map<string, string>();
  /** Keys whose write must fail, modelling a locked or refusing keyring. */
  readonly refuse = new Set<string>();

  async get(key: string): Promise<string | undefined> {
    return this.values.get(key);
  }
  async store(key: string, value: string): Promise<void> {
    if (this.refuse.has(key)) throw new Error("keyring is locked");
    this.values.set(key, value);
  }
  async delete(key: string): Promise<void> {
    this.values.delete(key);
  }
}

function writer(sink: ClusterUpdate[]): ClusterWriter {
  return async (update) => {
    sink.push(update);
  };
}

function cluster(overrides: Partial<ClusterConfig> = {}): ClusterConfig {
  return { name: "local", endpoint: "cockpit.local.znas.io:443", ...overrides };
}

function tokens(overrides: Partial<Parameters<typeof persistSignIn>[2]> = {}) {
  return {
    accessToken: "ACCESS",
    refreshToken: "REFRESH",
    expiresAtEpochSeconds: NOW_SECONDS + 900,
    clientId: "client-abc",
    ...overrides,
  };
}

// -----------------------------------------------------------------------------
// persistSignIn -- the split
// -----------------------------------------------------------------------------

test("a sign-in puts the refresh token in SecretStorage and the access token in the file", async () => {
  // The divergence from the issue's literal wording, asserted so it cannot be
  // undone by accident: the ACCESS token stays in clusters.yaml because that
  // file is shared with the memQL Cockpit, which must see a credential. Only
  // the thirty-day refresh token is a secret this extension keeps to itself.
  const secrets = new FakeSecrets();
  const written: ClusterUpdate[] = [];

  await persistSignIn({ secrets, writeCluster: writer(written) }, "local", tokens());

  assert.equal(secrets.values.get(refreshTokenSecretKey("local")), "REFRESH");
  assert.deepEqual(written, [
    { name: "local", token: "ACCESS", refreshToken: "", clientId: "client-abc" },
  ]);
});

test("the expiry is stored, so a credential carrying none of its own can still be renewed early", async () => {
  const secrets = new FakeSecrets();
  await persistSignIn({ secrets, writeCluster: writer([]) }, "local", tokens());

  assert.equal(
    secrets.values.get(accessTokenExpirySecretKey("local")),
    String(NOW_SECONDS + 900),
  );
});

test("with no SecretStorage the refresh token is written to the file instead of discarded", async () => {
  // A sign-in that silently dropped the thirty-day credential would send the
  // operator back through a browser every fifteen minutes. The file is the
  // ingest path the resolver already reads.
  const written: ClusterUpdate[] = [];
  await persistSignIn({ writeCluster: writer(written) }, "local", tokens());

  assert.equal(written[0]?.refreshToken, "REFRESH");
});

test("a keyring that refuses the write leaves the plaintext copy in place", async () => {
  const secrets = new FakeSecrets();
  secrets.refuse.add(refreshTokenSecretKey("local"));
  const written: ClusterUpdate[] = [];

  await persistSignIn({ secrets, writeCluster: writer(written) }, "local", tokens());

  assert.equal(written[0]?.refreshToken, "REFRESH", "custody was not taken, so nothing is cleared");
});

test("a sign-in that issued no refresh token clears the file's stale one", async () => {
  const secrets = new FakeSecrets();
  const written: ClusterUpdate[] = [];

  await persistSignIn({ secrets, writeCluster: writer(written) }, "local", tokens({ refreshToken: "" }));

  assert.equal(written[0]?.refreshToken, "");
  assert.equal(secrets.values.get(refreshTokenSecretKey("local")), undefined);
});

// -----------------------------------------------------------------------------
// signOut
// -----------------------------------------------------------------------------

test("signing out removes both halves -- the secret and the file's token keys", async () => {
  const secrets = new FakeSecrets();
  const written: ClusterUpdate[] = [];
  const deps = { secrets, writeCluster: writer(written) };
  await persistSignIn(deps, "local", tokens());
  written.length = 0;

  await signOut(deps, "local");

  assert.equal(secrets.values.get(refreshTokenSecretKey("local")), undefined);
  assert.equal(secrets.values.get(accessTokenExpirySecretKey("local")), undefined);
  assert.deepEqual(written, [{ name: "local", token: "", refreshToken: "" }]);
});

test("signing out de-indexes the cluster, so a later sweep has nothing to find", async () => {
  const secrets = new FakeSecrets();
  const deps = { secrets, writeCluster: writer([]) };
  await persistSignIn(deps, "local", tokens());

  await signOut(deps, "local");

  assert.deepEqual(await new ClusterCredentialStore(secrets).indexedClusters(), []);
});

// -----------------------------------------------------------------------------
// Rename and delete must not orphan a secret
// -----------------------------------------------------------------------------

test("renaming a cluster MOVES its secrets rather than stranding them", async () => {
  const secrets = new FakeSecrets();
  await persistSignIn({ secrets, writeCluster: writer([]) }, "old", tokens());

  await renameClusterCredentials({ secrets }, "old", "new");

  assert.equal(secrets.values.get(refreshTokenSecretKey("new")), "REFRESH");
  assert.equal(secrets.values.get(accessTokenExpirySecretKey("new")), String(NOW_SECONDS + 900));
  assert.equal(secrets.values.get(refreshTokenSecretKey("old")), undefined);
  assert.equal(secrets.values.get(accessTokenExpirySecretKey("old")), undefined);
  assert.deepEqual(await new ClusterCredentialStore(secrets).indexedClusters(), ["new"]);
});

test("a rename is not a sign-out: the renamed cluster survives the next sweep", async () => {
  // If rename left the secrets under the old key, reconcile would delete them
  // and the operator would be signed out of a cluster they only renamed.
  const secrets = new FakeSecrets();
  await persistSignIn({ secrets, writeCluster: writer([]) }, "old", tokens());
  await renameClusterCredentials({ secrets }, "old", "new");

  const swept = await reconcileClusterCredentials({ secrets }, ["new"]);

  assert.deepEqual(swept, []);
  assert.equal(secrets.values.get(refreshTokenSecretKey("new")), "REFRESH");
});

test("a deleted cluster's secrets are swept, and the surviving ones are left alone", async () => {
  // The catch-all for a deletion this extension never saw: the Cockpit writes
  // clusters.yaml too, and an operator can edit it by hand.
  const secrets = new FakeSecrets();
  const deps = { secrets, writeCluster: writer([]) };
  await persistSignIn(deps, "gone", tokens());
  await persistSignIn(deps, "staying", tokens({ refreshToken: "KEEP" }));

  const swept = await reconcileClusterCredentials({ secrets }, ["staying"]);

  assert.deepEqual(swept, ["gone"]);
  assert.equal(secrets.values.get(refreshTokenSecretKey("gone")), undefined);
  assert.equal(secrets.values.get(accessTokenExpirySecretKey("gone")), undefined);
  assert.equal(secrets.values.get(refreshTokenSecretKey("staying")), "KEEP");
  assert.deepEqual(await new ClusterCredentialStore(secrets).indexedClusters(), ["staying"]);
});

test("a sweep with nothing to remove rewrites nothing", async () => {
  const secrets = new FakeSecrets();
  await persistSignIn({ secrets, writeCluster: writer([]) }, "local", tokens());
  const before = new Map(secrets.values);

  assert.deepEqual(await reconcileClusterCredentials({ secrets }, ["local"]), []);
  assert.deepEqual([...secrets.values], [...before]);
});

test("with no SecretStorage every store operation is a no-op rather than a throw", async () => {
  const store = new ClusterCredentialStore();
  assert.equal(store.available, false);
  assert.equal(await store.readRefreshToken("local"), undefined);
  assert.equal(await store.writeRefreshToken("local", "RT"), false);
  assert.equal(await store.readExpiry("local"), undefined);
  await store.writeExpiry("local", NOW_SECONDS);
  await store.clear("local");
  await store.rename("a", "b");
  assert.deepEqual(await store.reconcile(["local"]), []);
});

test("a corrupt index reads as empty rather than taking the whole store down", async () => {
  const secrets = new FakeSecrets();
  await secrets.store("memql.cluster.credentialIndex", "{not json");

  assert.deepEqual(await new ClusterCredentialStore(secrets).indexedClusters(), []);
});

test("a stored expiry that is not a number reads as no expiry, not as expiring at zero", async () => {
  // An expiry of zero would renew a perfectly good token on every connect.
  const secrets = new FakeSecrets();
  await secrets.store(accessTokenExpirySecretKey("local"), "soon");

  assert.equal(await new ClusterCredentialStore(secrets).readExpiry("local"), undefined);
});

// -----------------------------------------------------------------------------
// Refresh once, retry once
// -----------------------------------------------------------------------------

test("isUnauthorizedError recognises what a rejected WebSocket upgrade actually looks like", () => {
  // `ws` surfaces a refused upgrade as a bare message with no status field:
  //   Error: Unexpected server response: 401
  assert.equal(isUnauthorizedError(new Error("Unexpected server response: 401")), true);
  assert.equal(isUnauthorizedError(new Error("Unexpected server response: 403")), true);
  assert.equal(isUnauthorizedError(new Error("unauthenticated")), true);
  assert.equal(isUnauthorizedError("Unauthorized"), true);
  assert.equal(isUnauthorizedError(new Error("Unexpected server response: 502")), false);
  assert.equal(isUnauthorizedError(new Error("connect ECONNREFUSED 127.0.0.1:443")), false);
});

class FakeCredentials implements CredentialSource {
  forceRefreshCalls = 0;
  constructor(
    private readonly resolution: CredentialResolution = { ok: true, bearer: "BEARER-1" },
    private readonly refreshed: string | null = "BEARER-2",
  ) {}
  async resolve(): Promise<CredentialResolution> {
    return this.resolution;
  }
  async forceRefresh(): Promise<string | null> {
    this.forceRefreshCalls += 1;
    return this.refreshed;
  }
}

test("a successful attempt runs once, with the resolved bearer and no refresh", async () => {
  const credentials = new FakeCredentials();
  const seen: string[] = [];

  const result = await runAuthenticated(cluster(), { credentials }, async (bearer) => {
    seen.push(bearer);
    return "connected";
  });

  assert.deepEqual(result, { ok: true, value: "connected", refreshed: false });
  assert.deepEqual(seen, ["BEARER-1"]);
  assert.equal(credentials.forceRefreshCalls, 0);
});

test("a 401 refreshes ONCE and retries ONCE", async () => {
  const credentials = new FakeCredentials();
  const seen: string[] = [];

  const result = await runAuthenticated(cluster(), { credentials }, async (bearer) => {
    seen.push(bearer);
    if (bearer === "BEARER-1") throw new Error("Unexpected server response: 401");
    return "connected";
  });

  assert.deepEqual(result, { ok: true, value: "connected", refreshed: true });
  assert.deepEqual(seen, ["BEARER-1", "BEARER-2"]);
  assert.equal(credentials.forceRefreshCalls, 1);
});

test("a 401 that survives a fresh token clears the tokens and asks for a sign-in", async () => {
  // Retrying past this point is how a client spins forever against an identity
  // service issuing tokens the cluster will not accept.
  const secrets = new FakeSecrets();
  const written: ClusterUpdate[] = [];
  const store = { secrets, writeCluster: writer(written) };
  await persistSignIn(store, "local", tokens());
  written.length = 0;
  const credentials = new FakeCredentials();
  let attempts = 0;

  const result = await runAuthenticated(cluster(), { credentials, store }, async () => {
    attempts += 1;
    throw new Error("Unexpected server response: 401");
  });

  assert.equal(attempts, 2, "exactly one retry");
  assert.equal(credentials.forceRefreshCalls, 1);
  assert.equal(result.ok, false);
  assert.equal(result.ok === false ? result.reason : "", "reauthenticationRequired");
  assert.match(result.ok === false ? result.message : "", /sign in/i);
  assert.equal(secrets.values.get(refreshTokenSecretKey("local")), undefined);
  assert.deepEqual(written, [{ name: "local", token: "", refreshToken: "" }]);
});

test("a 401 with nothing left to refresh with does not bother retrying", async () => {
  const credentials = new FakeCredentials({ ok: true, bearer: "BEARER-1" }, null);
  let attempts = 0;

  const result = await runAuthenticated(cluster(), { credentials }, async () => {
    attempts += 1;
    throw new Error("Unexpected server response: 401");
  });

  assert.equal(attempts, 1);
  assert.equal(result.ok === false ? result.reason : "", "reauthenticationRequired");
});

test("a failure that is NOT a 401 is rethrown untouched", async () => {
  // Relabelling "the cluster is down" as "sign in again" would send the
  // operator to fix the one thing that is not broken.
  const credentials = new FakeCredentials();

  await assert.rejects(
    runAuthenticated(cluster(), { credentials }, async () => {
      throw new Error("connect ECONNREFUSED 10.0.0.5:50051");
    }),
    /ECONNREFUSED/,
  );
  assert.equal(credentials.forceRefreshCalls, 0);
});

test("a credential that will not resolve is reported without the attempt being made", async () => {
  const credentials = new FakeCredentials({
    ok: false,
    reason: "wrongTokenClass",
    message: "that is a PAT",
  });
  let attempts = 0;

  const result = await runAuthenticated(cluster(), { credentials }, async () => {
    attempts += 1;
    return "connected";
  });

  assert.equal(attempts, 0);
  assert.equal(result.ok === false ? result.reason : "", "wrongTokenClass");
});
