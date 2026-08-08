// clusters.yaml round-trip tests.
//
// The file is SHARED with the memQL Cockpit. Two properties matter more than
// anything else here: an unknown key written by a newer cockpit must survive a
// write from this extension, and the operator's comments must not be stripped.
// Both are silent data loss if they regress.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import {
  readClustersFile,
  readClustersFileSafe,
  setSelectedCluster,
  upsertCluster,
} from "../src/clusters/file.js";
import { needsAuth } from "../src/clusters/model.js";

async function tempFile(contents: string): Promise<string> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-clusters-"));
  const file = path.join(dir, "clusters.yaml");
  await fs.writeFile(file, contents, "utf8");
  return file;
}

const SAMPLE = `# my clusters
clusters:
  - name: local
    display_name: local.znas.io
    domain: local.znas.io
    endpoint: cockpit.local.znas.io:443
    pat: mql_pat_abc
  - name: staging
    domain: staging.example.com
    endpoint: cockpit.staging.example.com:443
    issuer: https://identity.staging.example.com
    client_id: cockpit
selected_cluster: local
`;

test("reads clusters and the selected cluster", async () => {
  const f = await tempFile(SAMPLE);
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters.length, 2);
  assert.equal(parsed.selectedCluster, "local");
  assert.equal(parsed.clusters[0]?.name, "local");
  assert.equal(parsed.clusters[0]?.displayName, "local.znas.io");
  assert.equal(parsed.clusters[0]?.endpoint, "cockpit.local.znas.io:443");
  assert.equal(parsed.clusters[0]?.pat, "mql_pat_abc");
  assert.equal(parsed.clusters[1]?.clientId, "cockpit");
});

test("returns an empty registry when the file does not exist", async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-clusters-"));
  const parsed = await readClustersFile(path.join(dir, "absent.yaml"));
  assert.deepEqual(parsed, { clusters: [], selectedCluster: "" });
});

test("returns an empty registry for an empty file", async () => {
  const f = await tempFile("");
  const parsed = await readClustersFile(f);
  assert.deepEqual(parsed, { clusters: [], selectedCluster: "" });
});

test("rejects a malformed file rather than silently returning nothing", async () => {
  const f = await tempFile("clusters: [unclosed\n");
  await assert.rejects(() => readClustersFile(f), /clusters\.yaml/);
});

test("readClustersFileSafe wraps a malformed file as an ok:false result instead of throwing", async () => {
  const f = await tempFile("clusters: [unclosed\n");
  const result = await readClustersFileSafe(f);
  assert.equal(result.ok, false);
  if (!result.ok) {
    assert.match(result.error, /clusters\.yaml/);
  }
});

test("readClustersFileSafe returns ok:true with the parsed file on success", async () => {
  const f = await tempFile(SAMPLE);
  const result = await readClustersFileSafe(f);
  assert.equal(result.ok, true);
  if (result.ok) {
    assert.equal(result.file.clusters.length, 2);
    assert.equal(result.file.selectedCluster, "local");
  }
});

test("setSelectedCluster updates the selection", async () => {
  const f = await tempFile(SAMPLE);
  await setSelectedCluster(f, "staging");
  assert.equal((await readClustersFile(f)).selectedCluster, "staging");
});

test("setSelectedCluster preserves comments", async () => {
  const f = await tempFile(SAMPLE);
  await setSelectedCluster(f, "staging");
  assert.match(await fs.readFile(f, "utf8"), /# my clusters/);
});

test("setSelectedCluster preserves unknown keys written by a newer cockpit", async () => {
  const f = await tempFile(
    SAMPLE.replace("    pat: mql_pat_abc\n", "    pat: mql_pat_abc\n    future_field: keep-me\n"),
  );
  await setSelectedCluster(f, "staging");
  assert.match(await fs.readFile(f, "utf8"), /future_field: keep-me/);
});

test("upsertCluster adds a new cluster", async () => {
  const f = await tempFile(SAMPLE);
  await upsertCluster(f, {
    name: "prod",
    endpoint: "cockpit.prod.example.com:443",
    domain: "prod.example.com",
  });
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters.length, 3);
  assert.equal(parsed.clusters[2]?.name, "prod");
});

test("upsertCluster updates an existing cluster in place", async () => {
  const f = await tempFile(SAMPLE);
  await upsertCluster(f, {
    name: "staging",
    endpoint: "cockpit.new.example.com:443",
  });
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters.length, 2);
  assert.equal(parsed.clusters[1]?.endpoint, "cockpit.new.example.com:443");
});

test("upsertCluster preserves unknown keys on the updated cluster", async () => {
  const f = await tempFile(
    SAMPLE.replace("    client_id: cockpit\n", "    client_id: cockpit\n    future_field: keep-me\n"),
  );
  await upsertCluster(f, { name: "staging", endpoint: "cockpit.new.example.com:443" });
  assert.match(await fs.readFile(f, "utf8"), /future_field: keep-me/);
});

test("writes create the file and its parent directory when absent", async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-clusters-"));
  const f = path.join(dir, "nested", "clusters.yaml");
  await upsertCluster(f, { name: "local", endpoint: "cockpit.local.znas.io:443" });
  assert.equal((await readClustersFile(f)).clusters[0]?.name, "local");
});

test("needsAuth is false for a cluster with an endpoint and a PAT", () => {
  assert.equal(needsAuth({ name: "l", endpoint: "h:443", pat: "mql_pat_x" }), false);
});

test("needsAuth is false for a cluster with an endpoint, issuer and client id", () => {
  assert.equal(
    needsAuth({ name: "s", endpoint: "h:443", issuer: "https://i", clientId: "cockpit" }),
    false,
  );
});

test("needsAuth is true without an endpoint", () => {
  assert.equal(needsAuth({ name: "l", endpoint: "", pat: "mql_pat_x" }), true);
});

test("needsAuth is true with an issuer but no client id", () => {
  assert.equal(needsAuth({ name: "s", endpoint: "h:443", issuer: "https://i" }), true);
});
