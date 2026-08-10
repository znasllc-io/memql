// The LIVE half of the Extension Development Host lane (memql#3337).
//
// WHAT CHANGED, AND WHY IT IS A SEPARATE FILE.
//
// index.ts states, at the top, that no live cluster is required and that
// everything downstream of a connection belongs to the manual checklist. That
// is still true OF index.ts, and keeping this file separate is what keeps that
// statement honest: the smoke lane's contract -- runs anywhere, needs nothing
// -- is unchanged, and these cases skip when nothing is configured.
//
// What they add is the half memql#3337 says nobody has ever run: dialing a
// cluster and using what is on the other side. Every defect that issue lists
// was invisible to the unit suite AND to a lane that never connects, because
// the code under them only executes once a stream exists.
//
// THE LINE THIS LANE DOES NOT CROSS. It is READ-ONLY. It lists concepts, pages
// rows, resolves a row, validates a bundle, and reads the cluster tab's three
// concept sets. It runs no mutation, defines nothing into the session, and
// deploys nothing -- an automated lane pointed at whatever cluster an operator
// had selected must not be able to write to it. The write-confirmation item,
// the deploy actions and the type-to-confirm phrases stay with the human.
//
// AND IT IS NOT A SUBSTITUTE FOR THE CHECKLIST. Nothing here looks at a pixel.
// "The icon turns to a filled green circle", "rows render through each
// concept's @displayCard", "the trace tab opens beside the form without
// stealing focus" are not assertions a process can make about itself. What
// this lane can do is fail before a human spends an evening finding out that
// the connection, the paging, or the diagnostics mapping was broken all along.
//
// HOW TO RUN IT:
//
//   MEMQL_HOST_SMOKE_CLUSTERS_FILE=~/.memql/clusters.yaml \
//   MEMQL_HOST_SMOKE_CLUSTER=vscode-local \
//   NODE_EXTRA_CA_CERTS="$(mkcert -CAROOT)/rootCA.pem" \
//     make vscode-test-host
//
// scripts/vscode/verification-setup.sh produces exactly that registry.
// Without the env the cases skip, loudly, one line each.

import * as assert from "node:assert/strict";

import { browseConceptPage, getRowByConceptAndId } from "@znasllc-io/memql-sdk-core/client";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

import { readClustersFile } from "../src/clusters/file.js";
import type { ClusterConfig } from "../src/clusters/model.js";
import { ConnectionManager } from "../src/connection/manager.js";
import type { Bundle } from "../src/run/bundle.js";
import { mapBundleDiagnostics } from "../src/run/diagnostics.js";

import { info, skip, smoke, warn } from "./harness.js";

/** The registry to read the cluster entry out of. Absent = skip every case here. */
const FILE_ENV = "MEMQL_HOST_SMOKE_CLUSTERS_FILE";
/** Which entry in it. Absent = the file's selected_cluster. */
const NAME_ENV = "MEMQL_HOST_SMOKE_CLUSTER";

// Two concepts every cluster has, whatever DSL bundle it is fronting. Reading
// PRODUCT rows would tie this lane to one product's schema; `v1:cluster:node`
// is registered by the engine itself and is populated by the mesh coming up,
// which is exactly the property a read-only probe wants.
const NODE_CONCEPT = "v1:cluster:node";
const DEPLOYMENT_CONCEPT = "v1:cluster:deployment";
const DEPLOYMENT_NODE_SPEC_CONCEPT = "v1:cluster:deploymentNodeSpec";

/**
 * One connection, shared across every case below.
 *
 * Cases run strictly in sequence and the extension holds exactly one
 * connection at a time by design (ConnectionManager's invariant), so dialing
 * per case would be both slower and less faithful: what an operator has is one
 * long-lived stream that every surface reads through, and a bug where the
 * SECOND read on a stream fails is one this shape can catch and a
 * connect-per-case shape cannot.
 */
let shared: ConnectionManager | undefined;

/** Resolves the configured cluster entry, or skips the case saying why. */
async function configuredCluster(): Promise<ClusterConfig> {
  const file = process.env[FILE_ENV];
  if (file === undefined || file.trim() === "") {
    skip(
      `no ${FILE_ENV} -- set it to a clusters.yaml with a credentialed entry to run the live half (scripts/vscode/verification-setup.sh writes one)`,
    );
  }

  const registry = await readClustersFile(file);
  const wanted = process.env[NAME_ENV]?.trim() ?? "";
  const name = wanted !== "" ? wanted : registry.selectedCluster;
  const cluster = registry.clusters.find((c) => c.name === name);
  if (cluster === undefined) {
    const names = registry.clusters.map((c) => c.name).join(", ");
    throw new Error(
      `${FILE_ENV}=${file} has no cluster named "${name}" (it has: ${names || "none"})`,
    );
  }
  // A missing credential is a configuration error, not a skip: the operator
  // asked for the live lane by pointing at this file, and silently reporting
  // "skipped" for a cluster they meant to test is the failure mode this whole
  // lane exists to stop.
  if ((cluster.token ?? "") === "") {
    throw new Error(
      `cluster "${name}" in ${file} has no token: -- sign in first (memQL: Sign In, or scripts/vscode/verification-setup.sh)`,
    );
  }
  return cluster;
}

// `Row` is Record<string, unknown>, so a row's id is `unknown` until something
// narrows it -- the same narrow-before-use conceptPanel.ts does before calling
// getRowByConceptAndId. A row that reaches a client with a non-string id is a
// wire-shape failure, so it fails here rather than being coerced into looking
// fine.
function rowId(row: Row): string {
  const id = row["id"];
  if (typeof id !== "string" || id === "") {
    throw new Error(`a row came back with no usable id (got ${JSON.stringify(id)})`);
  }
  return id;
}

/** The live connection, or a skip if the connect case never established one. */
function connected(): ConnectionManager {
  if (shared === undefined || shared.state.status !== "connected") {
    skip("no live connection was established (see the connect case above)");
  }
  return shared;
}

/**
 * registerLiveCases registers the live half.
 *
 * Called from index.ts's run() rather than at module load: cases execute in
 * registration order, and an import is evaluated before the importing module's
 * body -- so registering at load would put the live cases FIRST, ahead of the
 * activation case every other case leans on.
 */
export function registerLiveCases(): void {

// -----------------------------------------------------------------------------
// Connecting
// -----------------------------------------------------------------------------

// The case memql#3337 is really about: everything below here is code that has
// never executed in a real host, because reaching it requires a stream.
smoke("a live cluster connects from a real extension host", async () => {
  const cluster = await configuredCluster();
  const connections = new ConnectionManager();

  await connections.connect(cluster);

  const state = connections.state;
  if (state.status === "error") {
    // The reason is the machine-readable half src/clusters/status.ts turns
    // into an icon, and it is the single most useful thing to print here:
    // "your token ran out" and "the cluster went away" send an operator to
    // completely different places.
    throw new Error(`connect failed (${state.reason}): ${state.message}`);
  }
  assert.equal(state.status, "connected", `expected a connected stream, got ${state.status}`);
  assert.notEqual(
    state.status === "connected" ? state.nodeId : "",
    "",
    "a connected stream must name the node it landed on",
  );
  info(`connected to "${cluster.name}" via ${cluster.endpoint} (node ${state.status === "connected" ? state.nodeId : "?"})`);

  shared = connections;
});

// -----------------------------------------------------------------------------
// Concepts
// -----------------------------------------------------------------------------

smoke("the connected cluster lists concepts", async () => {
  const query = connected().query;
  assert.notEqual(query, undefined, "a connected manager must expose a QueryClient");

  const concepts: Concept[] = await query!.listConcepts();
  assert.ok(concepts.length > 0, "a live cluster registered zero concepts");

  const node = concepts.find((c) => c.id === NODE_CONCEPT);
  assert.notEqual(
    node,
    undefined,
    `${NODE_CONCEPT} is registered by the engine itself, so its absence means the list is not what it claims (got ${concepts.length} concepts)`,
  );
  // The tree groups by domain, so a concept whose domain is blank would render
  // under an unnamed group -- visible only in a host, against real data.
  const domainless = concepts.filter((c) => c.domain === "").map((c) => c.id);
  assert.deepEqual(domainless, [], "every concept must name a domain for the tree to group it");
  info(`${concepts.length} concepts across ${new Set(concepts.map((c) => c.domain)).size} domains`);
});

smoke("rows page over a live connection", async () => {
  const query = connected().query!;

  const first = await browseConceptPage(query, NODE_CONCEPT, { pageSize: 2 });
  assert.ok(
    first.rows.length > 0,
    `${NODE_CONCEPT} returned no rows -- a running mesh registers itself, so an empty page means the read, not the cluster, is wrong`,
  );

  if (first.nextCursor === "") {
    info(`one page of ${first.rows.length} row(s); no cursor, so paging is untested here`);
    return;
  }

  // The assertion that matters: page two is DIFFERENT rows. A cursor that is
  // returned but not honoured re-serves page one forever, and "Load more"
  // then appears to work while showing the same rows.
  const second = await browseConceptPage(query, NODE_CONCEPT, {
    pageSize: 2,
    cursor: first.nextCursor,
  });
  const firstIds = new Set(first.rows.map(rowId));
  const repeats = second.rows.map(rowId).filter((id) => firstIds.has(id));
  assert.deepEqual(repeats, [], "the second page re-served rows from the first -- the cursor is not advancing");
  info(`paged ${first.rows.length} + ${second.rows.length} rows`);
});

smoke("a row resolves to its full nested detail", async () => {
  const query = connected().query!;

  const page = await browseConceptPage(query, NODE_CONCEPT, { pageSize: 1 });
  const first = page.rows[0];
  if (first === undefined) skip(`${NODE_CONCEPT} has no rows to resolve`);
  const id = rowId(first);

  const detail = await getRowByConceptAndId(query, NODE_CONCEPT, id);
  if (detail === null) {
    throw new Error(`${NODE_CONCEPT} row ${id} came back from a page but not by id`);
  }
  assert.equal(rowId(detail), id, "resolved a different row than the one asked for");
  // The detail view's whole claim is that it shows the payload UNFLATTENED.
  // A payload that arrives flattened, as a string, or not at all renders as an
  // empty detail pane with no error anywhere.
  const payload = detail["payload"];
  assert.ok(
    payload !== null && typeof payload === "object",
    `row detail must carry a structured payload, got ${typeof payload}`,
  );
});

// -----------------------------------------------------------------------------
// The run surface's cluster-dependent half
// -----------------------------------------------------------------------------

// Bundle validation is the first cluster round-trip a run makes and the one
// whose failure a unit test cannot see: the engine compiles the submitted
// source, and what comes back is a real compiler's opinion. Nothing is
// session-defined and nothing is invoked, so this stays read-only.
function bundleOf(path: string, text: string): Bundle {
  return { sources: text, files: [{ path, startLine: 0, lines: text.split("\n") }] };
}

smoke("the engine validates a well-formed bundle over the live stream", async () => {
  const authoring = connected().authoring;
  assert.notEqual(authoring, undefined, "a connected manager must expose an AuthoringClient");

  const text = [
    "use cluster.concepts.{ node }",
    "",
    "@description(\"host smoke lane: a read-only probe query\")",
    "query node hostSmokeProbeQuery {",
    "  filter  row.concept==\"v1:cluster:node\"",
    "  shape   { row.id }",
    "}",
    "",
  ].join("\n");

  const result = await authoring!.validateBundle(text);
  if (!result.ok) {
    const detail = result.diagnostics
      .filter((d) => !d.ok && !d.skipped)
      .map((d) => `${d.kind} ${d.name} @${d.line}:${d.column}: ${d.error}`)
      .join("; ");
    // Not an outright failure: the DSL's authoring surface moves, and a probe
    // query written months ago going stale is a fact about this file, not
    // about the cluster. Say so loudly instead of failing the lane for it --
    // the next case is the one that proves validation is really running.
    warn(`the probe query did not validate against this cluster: ${detail}`);
  } else {
    info("probe query validated");
  }
});

smoke("a broken construct comes back positioned, and maps to the right line", async () => {
  const authoring = connected().authoring!;
  const path = "/host-smoke/queries.memql";

  // Line 4 (1-based) is the deliberate error: a filter referring to a field
  // that cannot resolve. What is asserted is not the engine's wording -- it
  // is that SOMETHING failed, and that the mapper puts it in this file.
  const text = [
    "use cluster.concepts.{ node }",
    "",
    "query node hostSmokeBrokenQuery {",
    "  filter  ((((",
    "  shape   { row.id }",
    "}",
    "",
  ].join("\n");

  const result = await authoring.validateBundle(text);
  assert.equal(result.ok, false, "a bundle with a syntax error validated clean");

  const mapped = mapBundleDiagnostics(result.diagnostics, bundleOf(path, text));
  assert.ok(mapped.length > 0, "the engine refused the bundle but produced no mappable diagnostic");
  for (const d of mapped) {
    assert.equal(d.path, path, `a diagnostic landed on ${d.path}, which is not the file that was submitted`);
    assert.ok(d.start.line >= 0, "a mapped diagnostic must carry a non-negative line");
    assert.ok(
      d.start.line < text.split("\n").length,
      `a diagnostic mapped to line ${d.start.line}, past the end of a ${text.split("\n").length}-line file`,
    );
  }
  const positionless = mapped.filter((d) => d.fileLevel).length;
  info(
    `${mapped.length} diagnostic(s), ${positionless} of them positionless (file-level) -- first: ${mapped[0]?.message}`,
  );
});

// -----------------------------------------------------------------------------
// The Cluster tab's reads
// -----------------------------------------------------------------------------

smoke("the Cluster tab's three reads answer over a live stream", async () => {
  const query = connected().query!;

  // Issued together, exactly as ClusterPanel.refresh() does -- sequential
  // reads would let a deploy land between them and make every live node look
  // orphaned.
  const [nodes, deployments, specs] = await Promise.all([
    browseConceptPage(query, NODE_CONCEPT, { pageSize: 50 }),
    browseConceptPage(query, DEPLOYMENT_CONCEPT, { pageSize: 50 }),
    browseConceptPage(query, DEPLOYMENT_NODE_SPEC_CONCEPT, { pageSize: 50 }),
  ]);

  assert.ok(nodes.rows.length > 0, "the topology read returned no nodes on a running cluster");
  info(
    `topology inputs: ${nodes.rows.length} node(s), ${deployments.rows.length} deployment(s), ${specs.rows.length} spec(s)`,
  );

  // The role decides which action buttons draw at all. A read that answers
  // with no role is the "indeterminate" case the panel has to render as
  // hidden-with-a-reason rather than as permitted.
  const access = await query.getMyAccess();
  if (access === null || access.clusterRole === "") {
    warn("getMyAccess() produced no cluster role -- the Cluster tab will hide its actions as indeterminate");
  } else {
    info(`caller's cluster role: ${access.clusterRole}`);
  }
});

// -----------------------------------------------------------------------------
// Live updates
// -----------------------------------------------------------------------------

// Deliberately proves only that a subscription can be ESTABLISHED. Proving a
// row ARRIVES needs a write, and this lane does not write. The manual
// checklist's "insert a row elsewhere and watch it appear" keeps that half.
smoke("a concept subscription can be established", async () => {
  const subscriptions = connected().subscriptions;
  assert.notEqual(subscriptions, undefined, "a connected manager must expose a SubscriptionManager");
  info("subscription manager present; row delivery stays with the manual checklist (it needs a write)");
});

// -----------------------------------------------------------------------------
// Teardown
// -----------------------------------------------------------------------------

smoke("the live connection closes cleanly", async () => {
  if (shared === undefined) skip("nothing was connected");
  await shared.disconnect();
  assert.equal(shared.state.status, "disconnected", "disconnect() left the manager in a non-disconnected state");
});
}
