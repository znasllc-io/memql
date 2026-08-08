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
  addCluster,
  readClustersFile,
  readClustersFileSafe,
  setSelectedCluster,
  upsertCluster,
} from "../src/clusters/file.js";
import { isOidcOnly, needsAuth } from "../src/clusters/model.js";

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

// --- Renaming (the edit flow's name field is editable) ---------------------
//
// upsertCluster used to match by the NEW name, so an edit that changed the
// name matched nothing and appended a SECOND entry: the original was left
// orphaned, and selected_cluster could still be pointing at it.

test("upsertCluster renames a cluster in place rather than adding a second entry", async () => {
  const f = await tempFile(SAMPLE);
  await upsertCluster(
    f,
    { name: "staging-eu", endpoint: "cockpit.staging.example.com:443" },
    "staging",
  );
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters.length, 2, "a rename must not create a second entry");
  assert.deepEqual(
    parsed.clusters.map((c) => c.name),
    ["local", "staging-eu"],
    "the renamed node must keep its position, not be orphaned and re-appended",
  );
});

test("a rename carries selected_cluster with it", async () => {
  const f = await tempFile(SAMPLE); // selected_cluster: local
  await upsertCluster(
    f,
    { name: "local-dev", endpoint: "cockpit.local.znas.io:443" },
    "local",
  );
  const parsed = await readClustersFile(f);
  assert.equal(
    parsed.selectedCluster,
    "local-dev",
    "a selection pointing at the old name would reference a cluster that no longer exists",
  );
});

test("a rename leaves a selection pointing at a DIFFERENT cluster alone", async () => {
  const f = await tempFile(SAMPLE); // selected_cluster: local
  await upsertCluster(
    f,
    { name: "staging-eu", endpoint: "cockpit.staging.example.com:443" },
    "staging",
  );
  assert.equal((await readClustersFile(f)).selectedCluster, "local");
});

test("a rename preserves unknown keys and comments on the renamed node", async () => {
  const f = await tempFile(
    SAMPLE.replace("    client_id: cockpit\n", "    client_id: cockpit\n    future_field: keep-me\n"),
  );
  await upsertCluster(
    f,
    { name: "staging-eu", endpoint: "cockpit.staging.example.com:443" },
    "staging",
  );
  const raw = await fs.readFile(f, "utf8");
  assert.match(raw, /future_field: keep-me/);
  assert.match(raw, /# my clusters/);
});

test("upsertCluster refuses a rename onto a name already in use", async () => {
  const f = await tempFile(SAMPLE);
  await assert.rejects(
    () => upsertCluster(f, { name: "local", endpoint: "x:443" }, "staging"),
    /already taken/,
    "merging two nodes onto one name would make the loser unreachable",
  );
  // The file must be untouched by the refusal.
  const parsed = await readClustersFile(f);
  assert.deepEqual(parsed.clusters.map((c) => c.name), ["local", "staging"]);
});

test("an edit that does not change the name still updates in place", async () => {
  const f = await tempFile(SAMPLE);
  await upsertCluster(f, { name: "local", endpoint: "cockpit.changed:443" }, "local");
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters.length, 2);
  assert.equal(parsed.clusters[0]?.endpoint, "cockpit.changed:443");
});

// --- Clearing a field ------------------------------------------------------
//
// "" means the user emptied the input; the key must be DELETED. Treating it as
// "not supplied" meant clearing the PAT input to revoke a token silently kept
// the old token on disk while the UI showed the field empty.

test("upsertCluster deletes a key the caller explicitly cleared", async () => {
  const f = await tempFile(SAMPLE);
  await upsertCluster(f, {
    name: "local",
    endpoint: "cockpit.local.znas.io:443",
    pat: "",
  });
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters[0]?.pat, undefined, "the cleared PAT must be gone from the model");
  assert.doesNotMatch(
    await fs.readFile(f, "utf8"),
    /mql_pat_abc/,
    "the revoked token must not remain on disk",
  );
});

test("upsertCluster leaves an omitted key alone (undefined is not a clear)", async () => {
  const f = await tempFile(SAMPLE);
  await upsertCluster(f, { name: "local", endpoint: "cockpit.local.znas.io:443" });
  assert.equal(
    (await readClustersFile(f)).clusters[0]?.pat,
    "mql_pat_abc",
    "a field the caller never supplied must survive the write",
  );
});

test("clearing one field does not disturb the others", async () => {
  const f = await tempFile(SAMPLE);
  await upsertCluster(f, {
    name: "local",
    endpoint: "cockpit.local.znas.io:443",
    domain: "",
  });
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters[0]?.domain, undefined);
  assert.equal(parsed.clusters[0]?.pat, "mql_pat_abc");
  assert.equal(parsed.clusters[0]?.displayName, "local.znas.io");
});

test("a cleared field on a NEW cluster is simply not written", async () => {
  const f = await tempFile(SAMPLE);
  await upsertCluster(f, {
    name: "prod",
    endpoint: "cockpit.prod.example.com:443",
    domain: "",
    pat: "",
  });
  const parsed = await readClustersFile(f);
  const prod = parsed.clusters.find((c) => c.name === "prod");
  assert.ok(prod);
  assert.equal(prod.domain, undefined);
  assert.equal(prod.pat, undefined);
});

test("a rename and a clear applied together both take effect", async () => {
  const f = await tempFile(SAMPLE);
  await upsertCluster(
    f,
    { name: "local-dev", endpoint: "cockpit.local.znas.io:443", pat: "" },
    "local",
  );
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters.length, 2);
  assert.equal(parsed.clusters[0]?.name, "local-dev");
  assert.equal(parsed.clusters[0]?.pat, undefined);
  assert.equal(parsed.selectedCluster, "local-dev");
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

// --- The identity field is exempt from the "" -> delete rule ---------------
//
// `name` is what every other reference resolves a node by (findByName,
// selected_cluster, the tree's select/edit commands), so deleting it does not
// clear a value -- it orphans the whole node behind an entry nothing can
// resolve, endpoint and PAT included. Unreachable through the UI (the name
// box validates non-empty) but a latent hole in an exported function, so the
// exemption is enforced at upsertCluster's boundary.

test("upsertCluster refuses an empty name rather than deleting a node's identity", async () => {
  const f = await tempFile(SAMPLE);
  await assert.rejects(
    () => upsertCluster(f, { name: "", endpoint: "cockpit.local.znas.io:443" }, "local"),
    /cluster name is required/,
  );
});

test("a refused empty name leaves the target node completely intact", async () => {
  const f = await tempFile(SAMPLE);
  await assert.rejects(() => upsertCluster(f, { name: "", endpoint: "" }, "local"));

  const parsed = await readClustersFile(f);
  assert.deepEqual(parsed.clusters.map((c) => c.name), ["local", "staging"]);
  assert.equal(parsed.clusters[0]?.endpoint, "cockpit.local.znas.io:443");
  assert.equal(
    parsed.clusters[0]?.pat,
    "mql_pat_abc",
    "the node's credentials must not be left stranded behind an unresolvable entry",
  );
  assert.equal(parsed.selectedCluster, "local");
});

test("upsertCluster refuses an empty name on the add path too, rather than writing an anonymous node", async () => {
  const f = await tempFile(SAMPLE);
  await assert.rejects(() => upsertCluster(f, { name: "", endpoint: "x:443" }), /name is required/);
  assert.equal((await readClustersFile(f)).clusters.length, 2);
});

// --- addCluster: an add must never destroy an existing cluster -------------
//
// The add and edit flows collect the SAME four-field form, and add has no
// originalName to pass. So typing an existing name into the add form landed
// in upsertCluster's update branch, where every field the user left blank
// arrived as "" and was DELETED off the real cluster: "I tried to add a
// cluster" silently revoked the token on the one already there.

test("addCluster writes a new cluster", async () => {
  const f = await tempFile(SAMPLE);
  await addCluster(f, {
    name: "prod",
    endpoint: "cockpit.prod.example.com:443",
    domain: "prod.example.com",
  });
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters.length, 3);
  assert.equal(parsed.clusters[2]?.name, "prod");
});

test("addCluster refuses a name that already exists", async () => {
  const f = await tempFile(SAMPLE);
  await assert.rejects(
    () => addCluster(f, { name: "local", endpoint: "cockpit.local.znas.io:443" }),
    /already exists/,
  );
});

test("a refused add does not clear the existing cluster's PAT or domain", async () => {
  const f = await tempFile(SAMPLE);
  // Exactly what the add form produces when the user retypes an existing
  // name and leaves the optional inputs blank: empties, which upsertCluster
  // would have read as explicit clears.
  await assert.rejects(() =>
    addCluster(f, { name: "local", endpoint: "cockpit.local.znas.io:443", domain: "", pat: "" }),
  );

  const parsed = await readClustersFile(f);
  assert.equal(
    parsed.clusters[0]?.pat,
    "mql_pat_abc",
    "an add must not revoke the token on the cluster it collided with",
  );
  assert.equal(parsed.clusters[0]?.domain, "local.znas.io");
  assert.equal(parsed.clusters.length, 2, "and it must not append a duplicate either");
});

test("addCluster still refuses an empty name", async () => {
  const f = await tempFile(SAMPLE);
  await assert.rejects(() => addCluster(f, { name: "", endpoint: "x:443" }), /name is required/);
});

// --- isOidcOnly ------------------------------------------------------------
//
// Both callers (ConnectionManager's error message, the Clusters tree tooltip)
// check isOidcOnly BEFORE needsAuth, so an isOidcOnly that ignored the
// endpoint told an operator to "authenticate in the memQL Cockpit" when the
// truth was "this cluster has nowhere to dial".

test("isOidcOnly is true for an OIDC cluster with an endpoint and no PAT", () => {
  assert.equal(
    isOidcOnly({ name: "s", endpoint: "h:443", issuer: "https://i", clientId: "cockpit" }),
    true,
  );
});

test("isOidcOnly is false once a PAT is present", () => {
  assert.equal(
    isOidcOnly({
      name: "s",
      endpoint: "h:443",
      issuer: "https://i",
      clientId: "cockpit",
      pat: "mql_pat_x",
    }),
    false,
  );
});

test("isOidcOnly is false without an endpoint, so the message is 'not configured'", () => {
  const cluster = { name: "s", endpoint: "", issuer: "https://i", clientId: "cockpit" };
  assert.equal(isOidcOnly(cluster), false);
  assert.equal(needsAuth(cluster), true, "and needsAuth must be the one that answers instead");
});
