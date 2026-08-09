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
    token: eyJhbGci.eyJzdWIi.sig
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
  assert.equal(parsed.clusters[0]?.token, "eyJhbGci.eyJzdWIi.sig");
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
    SAMPLE.replace("    token: eyJhbGci.eyJzdWIi.sig\n", "    token: eyJhbGci.eyJzdWIi.sig\n    future_field: keep-me\n"),
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
    token: "",
  });
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters[0]?.token, undefined, "the cleared PAT must be gone from the model");
  assert.doesNotMatch(
    await fs.readFile(f, "utf8"),
    /eyJhbGci\.eyJzdWIi\.sig/,
    "the revoked token must not remain on disk",
  );
});

test("upsertCluster leaves an omitted key alone (undefined is not a clear)", async () => {
  const f = await tempFile(SAMPLE);
  await upsertCluster(f, { name: "local", endpoint: "cockpit.local.znas.io:443" });
  assert.equal(
    (await readClustersFile(f)).clusters[0]?.token,
    "eyJhbGci.eyJzdWIi.sig",
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
  assert.equal(parsed.clusters[0]?.token, "eyJhbGci.eyJzdWIi.sig");
  assert.equal(parsed.clusters[0]?.displayName, "local.znas.io");
});

test("a cleared field on a NEW cluster is simply not written", async () => {
  const f = await tempFile(SAMPLE);
  await upsertCluster(f, {
    name: "prod",
    endpoint: "cockpit.prod.example.com:443",
    domain: "",
    token: "",
  });
  const parsed = await readClustersFile(f);
  const prod = parsed.clusters.find((c) => c.name === "prod");
  assert.ok(prod);
  assert.equal(prod.domain, undefined);
  assert.equal(prod.token, undefined);
});

test("a rename and a clear applied together both take effect", async () => {
  const f = await tempFile(SAMPLE);
  await upsertCluster(
    f,
    { name: "local-dev", endpoint: "cockpit.local.znas.io:443", token: "" },
    "local",
  );
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters.length, 2);
  assert.equal(parsed.clusters[0]?.name, "local-dev");
  assert.equal(parsed.clusters[0]?.token, undefined);
  assert.equal(parsed.selectedCluster, "local-dev");
});

test("writes create the file and its parent directory when absent", async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-clusters-"));
  const f = path.join(dir, "nested", "clusters.yaml");
  await upsertCluster(f, { name: "local", endpoint: "cockpit.local.znas.io:443" });
  assert.equal((await readClustersFile(f)).clusters[0]?.name, "local");
});

test("needsAuth is false for a cluster with an endpoint and a token", () => {
  assert.equal(needsAuth({ name: "l", endpoint: "h:443", token: "eyJhbGci.eyJzdWIi.sig" }), false);
});

test("needsAuth is false for a cluster holding only a refresh token", () => {
  // A refresh token IS a credential: the resolver exchanges it for an access
  // token before dialing (memql#3385), so the row must not read as unconfigured.
  assert.equal(needsAuth({ name: "l", endpoint: "h:443", refreshToken: "rt" }), false);
});

test("needsAuth is TRUE for an issuer/clientId pair with no token", () => {
  // memql#3383. An issuer and a client id name WHERE tokens come from; they are
  // not a token. Counting them as "configured" is what let a credential-less
  // cluster look ready and then fail at the handshake with nothing useful said.
  assert.equal(
    needsAuth({ name: "s", endpoint: "h:443", issuer: "https://i", clientId: "cockpit" }),
    true,
  );
});

test("needsAuth is true without an endpoint", () => {
  assert.equal(needsAuth({ name: "l", endpoint: "", token: "eyJhbGci.eyJzdWIi.sig" }), true);
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
    parsed.clusters[0]?.token,
    "eyJhbGci.eyJzdWIi.sig",
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
    addCluster(f, { name: "local", endpoint: "cockpit.local.znas.io:443", domain: "", token: "" }),
  );

  const parsed = await readClustersFile(f);
  assert.equal(
    parsed.clusters[0]?.token,
    "eyJhbGci.eyJzdWIi.sig",
    "an add must not revoke the token on the cluster it collided with",
  );
  assert.equal(parsed.clusters[0]?.domain, "local.znas.io");
  assert.equal(parsed.clusters.length, 2, "and it must not append a duplicate either");
});

test("addCluster still refuses an empty name", async () => {
  const f = await tempFile(SAMPLE);
  await assert.rejects(() => addCluster(f, { name: "", endpoint: "x:443" }), /name is required/);
});

// --- the refresh token round trip (memql#3385) ------------------------------
//
// `refresh_token:` in the shared file is an INGEST path, not storage: the
// credential resolver presents it once, moves the rotated token into VS Code's
// SecretStorage, and then CLEARS this key. Both halves of that have to work
// through the same read/write rules every other field uses.

test("a refresh token round-trips through the wire spelling", async () => {
  const f = await tempFile(SAMPLE);
  await upsertCluster(f, { name: "local", refreshToken: "rt-abc" });
  assert.match(await fs.readFile(f, "utf8"), /refresh_token: rt-abc/);
  assert.equal((await readClustersFile(f)).clusters[0]?.refreshToken, "rt-abc");
});

test("clearing the refresh token deletes it from disk, leaving the access token", async () => {
  const f = await tempFile(SAMPLE);
  await upsertCluster(f, { name: "local", refreshToken: "rt-abc" });
  await upsertCluster(f, { name: "local", token: "eyJ.fresh.sig", refreshToken: "" });

  const raw = await fs.readFile(f, "utf8");
  assert.doesNotMatch(raw, /rt-abc/, "the long-lived secret must not linger in plaintext");
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters[0]?.refreshToken, undefined);
  assert.equal(parsed.clusters[0]?.token, "eyJ.fresh.sig");
});

test("a name-only update leaves every other field alone", async () => {
  // The shape the credential resolver writes: one field, identified by name,
  // with no endpoint restated.
  const f = await tempFile(SAMPLE);
  await upsertCluster(f, { name: "local", token: "eyJ.fresh.sig" });
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters[0]?.endpoint, "cockpit.local.znas.io:443");
  assert.equal(parsed.clusters[0]?.displayName, "local.znas.io");
  assert.equal(parsed.clusters[0]?.token, "eyJ.fresh.sig");
});

// -----------------------------------------------------------------------------
// The `local` flag (memql#3309)
// -----------------------------------------------------------------------------
//
// This is the first NON-STRING field in clusters.yaml, and the write rules for
// a boolean are genuinely different from a string's. A string carries three
// states -- undefined means "not supplied, leave disk alone", "" means
// "explicitly cleared, delete the key" -- and a boolean carries two, because
// the COCKPIT declares this field `yaml:"local,omitempty"`
// (znasllc-io/memql-cockpit#332) and drops the key whenever the value is
// false. A tool that wrote `local: false` back would make the two churn the
// file against each other on every save.
//
// The flag also gates the mutation confirmation, so a read that is wrong in
// the permissive direction disables a safety prompt on a cluster nobody marked.

test("readClustersFile parses local: true", async () => {
  const f = await tempFile("clusters:\n  - name: dev\n    endpoint: localhost:50051\n    local: true\n");
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters[0]?.local, true);
});

test("readClustersFile leaves local ABSENT when the key is missing", async () => {
  // Absent must not become `false` in the model either: the distinction
  // between "not supplied" and "supplied as false" is what upsertCluster's
  // leave-alone branch reads.
  const f = await tempFile("clusters:\n  - name: staging\n    endpoint: h:443\n");
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters[0]?.local, undefined);
});

test("readClustersFile does NOT accept a quoted string as local", async () => {
  // An operator hand-editing the file can easily write `local: "true"`. The
  // flag disables a confirmation prompt, so the safe direction for anything
  // ambiguous is "not local" -- which prompts.
  const f = await tempFile('clusters:\n  - name: dev\n    endpoint: h:443\n    local: "true"\n');
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters[0]?.local, undefined);
});

test("upsertCluster writes local: true on a NEW cluster", async () => {
  const f = await tempFile("clusters: []\n");
  await upsertCluster(f, { name: "dev", endpoint: "localhost:50051", local: true });
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters[0]?.local, true);
});

test("upsertCluster OMITS local on a new cluster when it is false", async () => {
  const f = await tempFile("clusters: []\n");
  await upsertCluster(f, { name: "staging", endpoint: "h:443", local: false });
  const raw = await fs.readFile(f, "utf8");
  assert.equal(
    /\blocal\b/.test(raw),
    false,
    "false must serialise as ABSENT -- the cockpit's omitempty drops the key, so writing it makes the two tools churn the file",
  );
});

test("upsertCluster DELETES local when an edit turns it off", async () => {
  const f = await tempFile("clusters:\n  - name: dev\n    endpoint: h:443\n    local: true\n");
  await upsertCluster(f, { name: "dev", endpoint: "h:443", local: false });
  const raw = await fs.readFile(f, "utf8");
  assert.equal(/\blocal\b/.test(raw), false);
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters[0]?.local, undefined);
});

test("upsertCluster leaves local alone when the write does not mention it", async () => {
  // The token-only edit path. Undefined still means "not supplied" for a
  // boolean, exactly as for a string -- otherwise changing an endpoint would
  // silently clear the flag.
  const f = await tempFile("clusters:\n  - name: dev\n    endpoint: h:443\n    local: true\n");
  await upsertCluster(f, { name: "dev", endpoint: "h:5051" });
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters[0]?.local, true);
  assert.equal(parsed.clusters[0]?.endpoint, "h:5051");
});

test("a local: true cluster survives a read-modify-write round trip", async () => {
  // The cross-tool contract: the cockpit writes the file too, so a value this
  // extension does not change must come back byte-identical in meaning.
  const f = await tempFile(
    "clusters:\n  - name: dev\n    endpoint: h:443\n    local: true\n  - name: staging\n    endpoint: s:443\n",
  );
  await setSelectedCluster(f, "staging");
  const parsed = await readClustersFile(f);
  assert.equal(parsed.clusters[0]?.local, true);
  assert.equal(parsed.clusters[1]?.local, undefined);
  assert.equal(parsed.selectedCluster, "staging");
});
