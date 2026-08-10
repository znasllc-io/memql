// Removing a cluster COMPLETELY: the entry, its credential, and its connection.
//
// removeCluster (src/clusters/file.ts) drops the YAML entry and nothing else.
// That alone is not "the operator deleted this cluster": the thirty-day refresh
// token stays in SecretStorage under a key nothing will look at again, and the
// live connection keeps running against a cluster the registry no longer knows.
// This module is the whole operation, and these cases are what "whole" means.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go):
// SecretStore is structural, so these drive a Map, and disconnect is injected.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import {
  accessTokenExpirySecretKey,
  refreshTokenSecretKey,
  credentialIndexKey,
  type SecretStore,
} from "../src/auth/store.js";
import { readClustersFile } from "../src/clusters/file.js";
import { removeClusterCompletely } from "../src/clusters/registry.js";

class FakeSecrets implements SecretStore {
  readonly values = new Map<string, string>();
  failOn: string | undefined;

  async get(key: string): Promise<string | undefined> {
    return this.values.get(key);
  }
  async store(key: string, value: string): Promise<void> {
    if (this.failOn === key) throw new Error("keyring locked");
    this.values.set(key, value);
  }
  async delete(key: string): Promise<void> {
    if (this.failOn === key) throw new Error("keyring locked");
    this.values.delete(key);
  }
}

const SAMPLE = `clusters:
  - name: local
    endpoint: cockpit.local.znas.io:443
    local: true
  - name: staging
    endpoint: cockpit.staging.example.com:443
selected_cluster: local
`;

async function tempFile(contents = SAMPLE): Promise<string> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-registry-"));
  const file = path.join(dir, "clusters.yaml");
  await fs.writeFile(file, contents, "utf8");
  return file;
}

/** A secrets store already holding a signed-in cluster's material. */
function signedIn(clusterName: string): FakeSecrets {
  const secrets = new FakeSecrets();
  secrets.values.set(refreshTokenSecretKey(clusterName), "refresh-token-value");
  secrets.values.set(accessTokenExpirySecretKey(clusterName), "1800000000");
  secrets.values.set(credentialIndexKey, JSON.stringify([clusterName]));
  return secrets;
}

test("removes the entry from the registry", async () => {
  const file = await tempFile();
  await removeClusterCompletely(file, "local", {});

  const parsed = await readClustersFile(file);
  assert.deepEqual(
    parsed.clusters.map((c) => c.name),
    ["staging"],
  );
});

test("purges the stored credential, not just the YAML row", async () => {
  const file = await tempFile();
  const secrets = signedIn("local");

  await removeClusterCompletely(file, "local", { secrets });

  // The assertion is on the STORE's contents, not on a call being made. A
  // cluster the operator believes they deleted must not leave a thirty-day
  // credential behind, and SecretStorage cannot be enumerated to find it later.
  assert.equal(secrets.values.get(refreshTokenSecretKey("local")), undefined);
  assert.equal(secrets.values.get(accessTokenExpirySecretKey("local")), undefined);
  assert.equal(
    secrets.values.get(credentialIndexKey),
    JSON.stringify([]),
    "the credential index must stop claiming a cluster that is gone",
  );
});

test("leaves another cluster's credential alone", async () => {
  const file = await tempFile();
  const secrets = signedIn("staging");

  await removeClusterCompletely(file, "local", { secrets });

  assert.equal(secrets.values.get(refreshTokenSecretKey("staging")), "refresh-token-value");
});

test("disconnects when the removed cluster is the live connection", async () => {
  const file = await tempFile();
  const disconnected: string[] = [];

  await removeClusterCompletely(file, "local", {
    connectedClusterName: "local",
    disconnect: async (name) => {
      disconnected.push(name);
    },
  });

  assert.deepEqual(disconnected, ["local"]);
});

test("does not disconnect when a different cluster is live", async () => {
  const file = await tempFile();
  const disconnected: string[] = [];

  await removeClusterCompletely(file, "local", {
    connectedClusterName: "staging",
    disconnect: async (name) => {
      disconnected.push(name);
    },
  });

  assert.deepEqual(disconnected, []);
});

test("disconnects BEFORE the entry is removed", async () => {
  const file = await tempFile();
  const order: string[] = [];

  await removeClusterCompletely(file, "local", {
    connectedClusterName: "local",
    disconnect: async () => {
      // Read the file from inside the disconnect: if the entry is still there,
      // the connection was dropped first. The order is what stops a failure
      // partway through leaving a live connection to a cluster the registry no
      // longer carries -- which nothing would then be able to name or close.
      const parsed = await readClustersFile(file);
      order.push(parsed.clusters.some((c) => c.name === "local") ? "entry-present" : "entry-gone");
    },
  });

  assert.deepEqual(order, ["entry-present"]);
});

test("refusing an unknown name changes nothing", async () => {
  const file = await tempFile();
  const secrets = signedIn("local");
  const disconnected: string[] = [];

  await assert.rejects(
    () =>
      removeClusterCompletely(file, "nope", {
        secrets,
        connectedClusterName: "nope",
        disconnect: async (name) => {
          disconnected.push(name);
        },
      }),
    /no cluster named "nope"/,
  );

  const parsed = await readClustersFile(file);
  assert.equal(parsed.clusters.length, 2, "no entry may be removed");
  assert.equal(
    secrets.values.get(refreshTokenSecretKey("local")),
    "refresh-token-value",
    "no credential may be purged for a removal that did not happen",
  );
  assert.deepEqual(disconnected, [], "nothing may be disconnected for a removal that did not happen");
});

test("a keyring that refuses the purge still removes the cluster", async () => {
  const file = await tempFile();
  const secrets = signedIn("local");
  secrets.failOn = refreshTokenSecretKey("local");

  // A locked or absent keyring is a normal Linux condition, and it must not
  // leave the operator unable to delete a cluster. The orphaned secret is
  // recoverable -- reconcile() sweeps credentials whose cluster is gone, and
  // this removal is precisely what makes it eligible.
  const removed = await removeClusterCompletely(file, "local", { secrets });

  assert.equal(removed.name, "local");
  const parsed = await readClustersFile(file);
  assert.deepEqual(
    parsed.clusters.map((c) => c.name),
    ["staging"],
  );
});

test("returns the removed cluster so the caller can report what went", async () => {
  const file = await tempFile();
  const removed = await removeClusterCompletely(file, "local", {});

  assert.equal(removed.name, "local");
  assert.equal(removed.local, true);
});
