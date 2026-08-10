// Removing a cluster from clusters.yaml.
//
// The file is SHARED with the memQL Cockpit, so removal inherits the same two
// properties every other write here has: an operator's comments survive, and a
// key a newer cockpit wrote on an entry this version does not model survives on
// the entries that REMAIN. A parse-and-reserialize removal would satisfy the
// assertion "the entry is gone" while quietly rewriting the whole file.
//
// The third property is removal's own: `selected_cluster` is a reference BY
// NAME, and removing its target without clearing it leaves the file pointing at
// a cluster that no longer exists -- the same orphaning upsertCluster's rename
// branch already guards against.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import { readClustersFile, removeCluster } from "../src/clusters/file.js";

async function tempFile(contents: string): Promise<string> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-clusters-remove-"));
  const file = path.join(dir, "clusters.yaml");
  await fs.writeFile(file, contents, "utf8");
  return file;
}

const SAMPLE = `# my clusters
clusters:
  # the parity cluster
  - name: local
    display_name: local.znas.io
    endpoint: cockpit.local.znas.io:443
    local: true
    token: eyJhbGci.eyJzdWIi.sig
  - name: staging
    endpoint: cockpit.staging.example.com:443
    issuer: https://identity.staging.example.com
    future_cockpit_key: preserved
selected_cluster: local
`;

test("removes the named entry and leaves the others", async () => {
  const f = await tempFile(SAMPLE);
  await removeCluster(f, "local");

  const parsed = await readClustersFile(f);
  assert.deepEqual(
    parsed.clusters.map((c) => c.name),
    ["staging"],
  );
});

test("returns the entry it removed, so the caller can act on it", async () => {
  const f = await tempFile(SAMPLE);
  const removed = await removeCluster(f, "local");

  // The caller needs `local` to decide whether to offer an uninstall, and the
  // endpoint to say which cluster it disconnected from. Returning the config
  // rather than void is what makes that possible without a read-before-delete
  // that would race the cockpit.
  assert.equal(removed.name, "local");
  assert.equal(removed.local, true);
  assert.equal(removed.endpoint, "cockpit.local.znas.io:443");
});

test("clears selected_cluster when it pointed at the removed cluster", async () => {
  const f = await tempFile(SAMPLE);
  await removeCluster(f, "local");

  const parsed = await readClustersFile(f);
  assert.equal(parsed.selectedCluster, "");
});

test("leaves selected_cluster alone when it pointed elsewhere", async () => {
  const f = await tempFile(SAMPLE);
  await removeCluster(f, "staging");

  const parsed = await readClustersFile(f);
  assert.equal(parsed.selectedCluster, "local");
});

test("refuses a name that is not present", async () => {
  const f = await tempFile(SAMPLE);
  await assert.rejects(
    () => removeCluster(f, "nope"),
    /no cluster named "nope"/,
    "removing a name that is not there must be an error, not a silent no-op: " +
      "the caller purges a credential and disconnects on the strength of this call",
  );
});

test("preserves comments and unmodelled keys on the entries that remain", async () => {
  const f = await tempFile(SAMPLE);
  await removeCluster(f, "local");

  const raw = await fs.readFile(f, "utf8");
  assert.match(raw, /# my clusters/, "the operator's file comment must survive");
  assert.match(
    raw,
    /future_cockpit_key: preserved/,
    "a key written by a newer cockpit must survive on the remaining entry",
  );
});

test("removing the only cluster leaves a readable file", async () => {
  const f = await tempFile(`clusters:
  - name: only
    endpoint: cockpit.example.com:443
selected_cluster: only
`);
  await removeCluster(f, "only");

  const parsed = await readClustersFile(f);
  assert.deepEqual(parsed.clusters, []);
  assert.equal(parsed.selectedCluster, "");
});

test("removes only the first match when a file carries a duplicate name", async () => {
  // A hand-edited or concurrently-written file can carry two entries with the
  // same name. Every lookup in file.ts resolves the FIRST, so removal has to
  // agree with them -- removing both would delete an entry no other operation
  // in this module can see, and removing the second would leave the visible one
  // in place while reporting success.
  const f = await tempFile(`clusters:
  - name: dup
    endpoint: first.example.com:443
  - name: dup
    endpoint: second.example.com:443
`);
  await removeCluster(f, "dup");

  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters.length, 1);
  assert.equal(parsed.clusters[0]?.endpoint, "second.example.com:443");
});
