// The callable run seam: install, uninstall, and the uninstall preview.
//
// The orchestration once lived inside cli.ts, reachable only by spawning
// `npm run install-cli`, which is why the "+" button reported a command for the
// operator to copy instead of running one -- there was no function to call. The
// stub that did the reporting is gone (memql#3478); these functions are what
// replaced it.
//
// The property these cases exist to protect is that there is ONE run path. The
// CLI and the webview must not be able to diverge into "it worked from the
// terminal but not from the editor", so cli.ts is reduced to argv parsing and
// presentation over exactly these functions, and the existing CLI tests
// continue to pass unchanged as the evidence that the extraction preserved
// behaviour.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import { spawn } from "node:child_process";
import { mkdtempSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

import {
  graphDocumentPath,
  loadGraph,
  loadGraphFile,
  type Graph,
  type Step,
} from "../src/install/graph.js";
import { capabilityScriptPath, type RunScript } from "../src/install/runner.js";
import {
  installPlan,
  installSessionOptions,
  previewUninstall,
  runInstall,
  runUninstall,
  type SessionOptions,
} from "../src/install/session.js";
import {
  DEFAULT_IMAGE_REGISTRY,
  DEFAULT_STACK_TAG,
  imageTagFor,
} from "../src/install/stackPin.js";
import type { ExecEvent, StepPlan } from "../src/install/executor.js";

const REPO_ROOT = path.resolve(__dirname, "..", "..", "..", "..");

/** loadGraph parses a document, so fixtures are written as one. */
function graph(doc: unknown): Graph {
  return loadGraph(JSON.stringify(doc), "test-fixture");
}

// A two-step install graph: one tool, then something that depends on it.
const INSTALL_GRAPH: Graph = graph({
  name: "test-install",
  kind: "install",
  steps: [
    {
      id: "binary",
      description: "place a tool",
      script: "install.binary",
      elevation: "none",
      retained: false,
      retainedReason: "",
      shared: false,
      sharedReason: "",
      receipt: "binary",
      preExistingPath: "none",
      verify: { kind: "resultTrue", field: "result.installed" },
    },
    {
      id: "cluster",
      description: "create the cluster",
      script: "k3d.up",
      dependsOn: ["binary"],
      elevation: "none",
      retained: false,
      retainedReason: "",
      shared: false,
      sharedReason: "",
      receipt: "stack",
      preExistingPath: "none",
      verify: { kind: "resultTrue", field: "result.installed" },
    },
  ],
});

const UNINSTALL_GRAPH: Graph = graph({
  name: "test-uninstall",
  kind: "uninstall",
  steps: [
    {
      id: "removeCluster",
      description: "remove the cluster",
      script: "install.removeArtifact",
      reverses: "cluster",
      elevation: "none",
      retained: false,
      retainedReason: "",
      shared: false,
      sharedReason: "",
      verify: { kind: "resultTrue", field: "result.removed" },
    },
    {
      id: "removeBinary",
      description: "remove the tool",
      script: "install.removeArtifact",
      reverses: "binary",
      dependsOn: ["removeCluster"],
      elevation: "none",
      retained: false,
      retainedReason: "",
      shared: false,
      sharedReason: "",
      verify: { kind: "resultTrue", field: "result.removed" },
    },
  ],
});

/** A runner that succeeds, recording what it was asked to do. */
function okRunner(seen: string[] = []): { run: RunScript; seen: string[] } {
  const run: RunScript = async ({ params }) => {
    seen.push(JSON.stringify(params));
    return {
      argv: [],
      exitCode: 0,
      signal: null,
      stdout: "",
      stderr: "",
      envelope: {
        ok: true,
        capability: "t",
        changed: true,
        // Satisfies both graphs' verify predicates, and carries the artifact
        // fields the receipt reads back for a removal.
        result: { installed: true, removed: true, path: "/tmp/k3d", cluster: "memql" },
        error: null,
      },
    };
  };
  return { run, seen };
}

/** One receipt entry in the shape receipt.ts reads back. */
function entry(
  stepId: string,
  receipt: string,
  result: Record<string, unknown>,
  preExisting: boolean,
): unknown {
  return {
    stepId,
    script: "install.test",
    receipt,
    preExisting,
    params: {},
    result,
    changed: true,
    recordedAt: "2026-08-10T00:00:00Z",
  };
}

async function tempReceipt(entries: unknown[]): Promise<string> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-session-"));
  const file = path.join(dir, "install-receipt.json");
  await fs.writeFile(file, JSON.stringify({ version: 1, entries }, null, 2), "utf8");
  return file;
}

function options(over: Partial<SessionOptions> = {}): SessionOptions {
  // A REAL receipt path per call. An install records every executed step as it
  // finishes, so a path that cannot be written turns each of these into a test
  // of mkdir rather than of the session.
  const dir = mkdtempSync(path.join(os.tmpdir(), "memql-session-opts-"));
  return {
    root: "/nonexistent",
    receiptFile: path.join(dir, "install-receipt.json"),
    skip: new Set<string>(),
    provider: "anthropic",
    stepParams: {},
    ...over,
  };
}

// -----------------------------------------------------------------------------
// running
// -----------------------------------------------------------------------------

test("runInstall walks the graph and reports every step", async () => {
  const { run } = okRunner();
  const report = await runInstall(options(), { graph: INSTALL_GRAPH, run });

  assert.equal(report.ok, true);
  assert.deepEqual(
    report.outcomes.map((o) => `${o.id}:${o.status}`),
    ["binary:ok", "cluster:ok"],
  );
});

test("runInstall emits the events a progress view needs, without knowing about one", async () => {
  const { run } = okRunner();
  const events: ExecEvent[] = [];
  await runInstall(options(), { graph: INSTALL_GRAPH, run, onEvent: (e) => void events.push(e) });

  // The union is the executor's own, forwarded rather than re-invented: it
  // already carries status, exit code, envelope and the verify verdict per
  // step, and a parallel event type would be a second thing to keep in sync.
  const started = events.filter((e) => e.type === "stepStarted").map((e) => e.step.id);
  const finished = events.filter((e) => e.type === "stepFinished").map((e) => e.step.id);
  assert.deepEqual(started, ["binary", "cluster"]);
  assert.deepEqual(finished, ["binary", "cluster"]);
});

test("run-time params reach the steps that declare them", async () => {
  const { run, seen } = okRunner();
  await runInstall(options({ toolDir: "/opt/memql/bin" }), { graph: INSTALL_GRAPH, run });
  assert.ok(
    seen.some((p) => p.includes("/opt/memql/bin")),
    `the tool directory never reached install.binary: ${seen.join(" | ")}`,
  );
});

test("runUninstall refuses without a receipt rather than guessing", async () => {
  const { run } = okRunner();
  await assert.rejects(
    () => runUninstall(options(), { graph: UNINSTALL_GRAPH, run }),
    /receipt/,
    "an uninstall with no record of the install would be guessing at the operator's machine",
  );
});

test("runUninstall removes what the receipt recorded", async () => {
  const receiptFile = await tempReceipt([
    entry("cluster", "stack", { cluster: "memql" }, false),
    entry("binary", "binary", { path: "/tmp/k3d" }, false),
  ]);
  const { run } = okRunner();
  const report = await runUninstall(options({ receiptFile }), { graph: UNINSTALL_GRAPH, run });

  assert.equal(report.ok, true);
  assert.deepEqual(
    report.outcomes.map((o) => o.status),
    ["ok", "ok"],
  );
});

// -----------------------------------------------------------------------------
// cancellation
// -----------------------------------------------------------------------------

test("cancelling stops the run and says so", async () => {
  const controller = new AbortController();
  const seen: string[] = [];
  const run: RunScript = async ({ params }) => {
    seen.push(JSON.stringify(params));
    // Abort while the first wave is in flight: the second must not start.
    controller.abort();
    return {
      argv: [],
      exitCode: 0,
      signal: null,
      stdout: "",
      stderr: "",
      envelope: {
        ok: true,
        capability: "t",
        changed: true,
        result: { installed: true, path: "/tmp/k3d", cluster: "memql" },
        error: null,
      },
    };
  };

  const report = await runInstall(options(), {
    graph: INSTALL_GRAPH,
    run,
    signal: controller.signal,
  });

  assert.equal(report.cancelled, true, "a cancelled run must say it was cancelled");
  assert.deepEqual(
    report.outcomes.map((o) => o.id),
    ["binary"],
    "the wave after the cancellation must not run",
  );
});

test("a cancelled run leaves a receipt that still describes what happened", async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-session-cancel-"));
  const receiptFile = path.join(dir, "install-receipt.json");
  const controller = new AbortController();
  const run: RunScript = async () => {
    controller.abort();
    return {
      argv: [],
      exitCode: 0,
      signal: null,
      stdout: "",
      stderr: "",
      envelope: {
        ok: true,
        capability: "t",
        changed: true,
        result: { installed: true, path: "/tmp/k3d" },
        error: null,
      },
    };
  };

  await runInstall(options({ receiptFile }), {
    graph: INSTALL_GRAPH,
    run,
    signal: controller.signal,
  });

  // This is what makes cancel safe to offer at all: the step that DID run is
  // recorded, so the uninstall can still take it back. A cancel that discarded
  // the receipt would strand whatever the run had already created.
  const raw = JSON.parse(await fs.readFile(receiptFile, "utf8")) as {
    entries: { stepId: string }[];
  };
  assert.deepEqual(
    raw.entries.map((e) => e.stepId),
    ["binary"],
  );
});

test("the receipt a cancelled install produces is one an uninstall can actually reverse", async () => {
  // TWO HALVES THAT USED TO NEVER MEET. One case proved the cancelled run leaves
  // a valid receipt by reading it back; the uninstall cases proved a removal
  // works by feeding in a HAND-BUILT fixture. A mismatch between what `record()`
  // writes and what `removalParams()` reads would have passed both -- and the
  // criterion is "a test proves the cancelled install is still uninstallable",
  // which is this chain and not either half.
  //
  // Nothing here is hand-authored: the receipt under test is the one the
  // executor produced, byte for byte.
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-session-chain-"));
  const receiptFile = path.join(dir, "install-receipt.json");

  const controller = new AbortController();
  const install: RunScript = async () => {
    // Cancelled the moment the first step returns, so the SECOND wave never
    // runs -- the state an operator who pressed Cancel is left in.
    controller.abort();
    return {
      argv: [],
      exitCode: 0,
      signal: null,
      stdout: "",
      stderr: "",
      envelope: {
        ok: true,
        capability: "install.binary",
        changed: true,
        result: { installed: true, path: "/tmp/k3d" },
        error: null,
      },
    };
  };

  const installReport = await runInstall(options({ receiptFile }), {
    graph: INSTALL_GRAPH,
    run: install,
    signal: controller.signal,
  });
  assert.equal(installReport.cancelled, true);

  // The preview, over the receipt the run just wrote. This is the screen the
  // operator confirms against, so what it lists is what consent is given to.
  const preview = await previewUninstall(options({ receiptFile }), { graph: UNINSTALL_GRAPH });
  assert.deepEqual(
    preview.removals.map((r) => r.id),
    ["removeBinary"],
    "exactly what ran is offered for removal -- no more, and not nothing",
  );
  assert.equal(
    preview.removals[0]!.params.path,
    "/tmp/k3d",
    "the removal is aimed by the path the install RECORDED, not by a fixture that agrees with it",
  );

  // And the removal itself runs, which is the half a preview cannot prove: a
  // step planned with no params would reach remove-artifact.sh and exit 2.
  const removal = okRunner();
  const uninstallReport = await runUninstall(options({ receiptFile }), {
    graph: UNINSTALL_GRAPH,
    run: removal.run,
  });
  assert.equal(uninstallReport.ok, true, "the cancelled install must be fully uninstallable");
  assert.deepEqual(
    uninstallReport.outcomes.filter((o) => o.status === "ok").map((o) => o.id),
    ["removeBinary"],
  );
  // removeCluster is SKIPPED-satisfied rather than failed: the install stopped
  // before the cluster, so there is nothing to remove and the removals waiting
  // on it may still proceed.
  const cluster = uninstallReport.outcomes.find((o) => o.id === "removeCluster");
  assert.equal(cluster?.status, "skipped");
  assert.equal(cluster?.satisfied, true);
});

// -----------------------------------------------------------------------------
// the uninstall preview
// -----------------------------------------------------------------------------

test("previewUninstall itemizes removals without running anything", async () => {
  const receiptFile = await tempReceipt([
    entry("cluster", "stack", { cluster: "memql" }, false),
    entry("binary", "binary", { path: "/tmp/k3d" }, false),
  ]);
  let ran = false;
  const run: RunScript = async () => {
    ran = true;
    throw new Error("preview must not execute anything");
  };

  const preview = await previewUninstall(options({ receiptFile }), { graph: UNINSTALL_GRAPH, run });

  assert.equal(ran, false, "a preview that runs the graph is not a preview");
  assert.deepEqual(
    preview.removals.map((s) => s.id),
    ["removeCluster", "removeBinary"],
  );
  assert.deepEqual(preview.preserved, []);
});

test("previewUninstall reports a pre-existing artifact as PRESERVED, not removed", async () => {
  // The two-tier model (design D6). A k3d cluster the developer already had is
  // kept, and the preview has to say so BEFORE the operator confirms -- an
  // itemized list that quietly included it would be asking consent for
  // something that is not going to happen, and a list that omitted it would
  // hide that the uninstall leaves something behind.
  const receiptFile = await tempReceipt([
    entry("cluster", "stack", { cluster: "mine" }, true),
    entry("binary", "binary", { path: "/tmp/k3d" }, false),
  ]);
  const { run } = okRunner();

  const preview = await previewUninstall(options({ receiptFile }), { graph: UNINSTALL_GRAPH, run });

  assert.deepEqual(
    preview.preserved.map((s) => s.id),
    ["removeCluster"],
  );
  // The preview is the confirmation, so a preserved row has to say WHY it is
  // being left alone. Only this layer knows -- it is holding the receipt's
  // pre-existence verdict -- so an empty reason would push that sentence into
  // whichever consumer rendered the row.
  assert.match(
    preview.preserved[0]?.reason ?? "",
    /already on this machine/,
    "a preserved step must carry its own reason",
  );
  assert.deepEqual(
    preview.removals.map((s) => s.id),
    ["removeBinary"],
  );
});

test("previewUninstall skips a step whose install left no receipt entry", async () => {
  const receiptFile = await tempReceipt([
    entry("binary", "binary", { path: "/tmp/k3d" }, false),
  ]);
  const { run } = okRunner();

  const preview = await previewUninstall(options({ receiptFile }), { graph: UNINSTALL_GRAPH, run });

  const cluster = preview.steps.find((s) => s.id === "removeCluster");
  assert.equal(cluster?.action, "skip");
  assert.match(cluster?.reason ?? "", /nothing to remove/);
  assert.ok(
    !preview.removals.some((s) => s.id === "removeCluster"),
    "a step with nothing to remove must not appear in the list of what will go",
  );
});

test("previewUninstall refuses without a receipt", async () => {
  const { run } = okRunner();
  await assert.rejects(() => previewUninstall(options(), { graph: UNINSTALL_GRAPH, run }), /receipt/);
});

// -----------------------------------------------------------------------------
// the checkout both steps have to agree about (memql#3491)
// -----------------------------------------------------------------------------

test("clusterUp is told where the checkout is, rather than deriving it", async () => {
  // The packaged case. k3d.up derives its root from its own location, which in
  // an installed extension is the staged tree -- scripts/ and nothing else. No
  // deploy/, and not a git tree, so the ArgoCD target revision fell through to
  // "main" with nothing reported. The graph has always declared clusterUp's
  // dependency on stackCheckout; this asserts the VALUE now flows along it.
  const plan = installPlan(options({ stackDir: "/opt/memql/src" }));
  const decision = plan({
    id: "clusterUp",
    script: "k3d.up",
    description: "",
    elevation: "none",
    retained: false,
    retainedReason: "",
    shared: false,
    sharedReason: "",
    verify: { kind: "scriptOk" },
  });

  assert.equal(decision.action, "run");
  if (decision.action === "run") {
    assert.equal(decision.params["repo-root"], "/opt/memql/src");
  }
});

test("both steps are given the SAME directory", async () => {
  // The divergence this exists to prevent: each script has its own default, so
  // a caller that set only one would point the cluster bring-up at a directory
  // the checkout never landed in -- and the failure would be a missing deploy/
  // rather than anything naming the real mistake.
  const plan = installPlan(options({ stackDir: "/opt/memql/src" }));
  const checkout = plan({
    id: "stackCheckout",
    script: "install.cloneStack",
    description: "",
    elevation: "none",
    retained: false,
    retainedReason: "",
    shared: false,
    sharedReason: "",
    verify: { kind: "scriptOk" },
  });
  const cluster = plan({
    id: "clusterUp",
    script: "k3d.up",
    description: "",
    elevation: "none",
    retained: false,
    retainedReason: "",
    shared: false,
    sharedReason: "",
    verify: { kind: "scriptOk" },
  });

  assert.equal(checkout.action, "run");
  assert.equal(cluster.action, "run");
  if (checkout.action === "run" && cluster.action === "run") {
    assert.equal(checkout.params.dest, cluster.params["repo-root"]);
  }
});

// -----------------------------------------------------------------------------
// the release tag (memql#3560)
// -----------------------------------------------------------------------------

test("stackCheckout is given a tag even when the caller names none", async () => {
  // The wizard has no tag field, so it passed none, and `present()` drops empty
  // values -- every install started from the "+" button reached clone-stack.sh
  // with no --tag and died there on exit 2. The CLI's `--tag` worked throughout,
  // which is exactly the "worked from the terminal, not from the editor" split
  // this module exists to make impossible.
  const plan = installPlan(options());
  const decision = plan({
    id: "stackCheckout",
    script: "install.cloneStack",
    description: "",
    elevation: "none",
    retained: false,
    retainedReason: "",
    shared: false,
    sharedReason: "",
    verify: { kind: "scriptOk" },
  });

  assert.equal(decision.action, "run");
  if (decision.action === "run") {
    assert.equal(decision.params.tag, DEFAULT_STACK_TAG);
    assert.notEqual(decision.params.tag, "");
  }
});

test("an explicit tag still wins over the pin", async () => {
  const plan = installPlan(options({ tag: "v9.9.9" }));
  const decision = plan({
    id: "stackCheckout",
    script: "install.cloneStack",
    description: "",
    elevation: "none",
    retained: false,
    retainedReason: "",
    shared: false,
    sharedReason: "",
    verify: { kind: "scriptOk" },
  });
  if (decision.action === "run") {
    assert.equal(decision.params.tag, "v9.9.9");
  }
});

test("the pinned tag is a release tag, not a branch", async () => {
  // clone-stack.sh rejects a branch outright: a branch MOVES, so two installs
  // of "the same version" a week apart are not the same install. A default that
  // was a branch name would be refused at run time by that gate -- assert the
  // shape here, where the failure is one line rather than a failed install.
  //
  // The RECENCY of the pin is deliberately not asserted. No test can tell a
  // deliberate pin from a stale one, and one that asked the network would fail
  // offline for reasons unrelated to the change under test.
  assert.match(DEFAULT_STACK_TAG, /^v\d+\.\d+\.\d+$/);
});

// Every param a capability script REQUIRES and has no default for has to come
// from somewhere -- the graph pins some, the plan supplies the rest. Nothing
// checked that, which is how `tag` went missing for the whole life of the
// wizard's install path.
//
// The requirement is read off the scripts themselves rather than from a list
// kept beside them: a list is a second thing to forget.
// Every param a capability script cannot run without has to come from
// somewhere -- the graph pins some, the plan supplies the rest. Nothing checked
// that, which is how the wizard shipped twice with an input it never passed:
// `--tag`, and then `--registration-mode`, which got all the way through
// creating a Kubernetes cluster before failing (memql#3568).
//
// REQUIREDNESS IS READ OFF `--print-spec`, by running each script. It used to be
// parsed out of the shell source by looking for `cap_require` beside an empty
// default -- which found `--tag` and could not find `--registration-mode`,
// because seed-bootstrap.sh checks its five as a set and reports them together.
// A test that infers a contract from an implementation only sees the shapes it
// was taught; --print-spec is the contract itself.
test("every required param is supplied by the graph or the plan", async () => {
  const graphDoc = await loadGraphFile(graphDocumentPath("install", REPO_ROOT));
  // THE WIZARD'S OWN OPTIONS, not a hand-populated SessionOptions. The point is
  // to catch a required input the PAGE does not supply -- which is what both
  // misses were -- so anything filled in by hand here is a hole it cannot see.
  const plan = installPlan(
    installSessionOptions({
      root: REPO_ROOT,
      receiptFile: path.join(mkdtempSync(path.join(os.tmpdir(), "memql-audit-")), "receipt.json"),
      provider: "anthropic",
      providerKeyFile: "/tmp/key",
      domain: "memql.localhost",
      ownerEmail: "op@example.test",
      ownerFirstName: "Op",
      ownerLastName: "Erator",
    }),
  );

  const missing: string[] = [];
  for (const step of graphDoc.steps) {
    const decision = plan(step);
    assert.equal(decision.action, "run", `${step.id} was not planned as a run`);
    if (decision.action !== "run") continue;
    const supplied = new Set([...Object.keys(step.params ?? {}), ...Object.keys(decision.params)]);

    for (const name of await requiredParams(capabilityScriptPath(step.script, REPO_ROOT))) {
      if (!supplied.has(name)) missing.push(`${step.id} (${step.script}): --${name}`);
    }
  }

  assert.deepEqual(
    missing,
    [],
    `these steps would fail with "missing required parameter":\n  ${missing.join("\n  ")}`,
  );
});

/** The params a capability script declares it cannot run without. */
async function requiredParams(scriptPath: string): Promise<string[]> {
  const out = await new Promise<string>((resolve, reject) => {
    const child = spawn(scriptPath, ["--print-spec"], { stdio: ["ignore", "pipe", "ignore"] });
    let stdout = "";
    child.stdout.on("data", (c) => (stdout += String(c)));
    child.on("error", reject);
    child.on("close", () => resolve(stdout));
  });
  const spec = JSON.parse(out) as { params?: { name: string; required?: boolean }[] };
  return (spec.params ?? []).filter((p) => p.required === true).map((p) => p.name);
}

test("the root a packaged run passes is NOT the script's own parent", async () => {
  // The acceptance criterion, stated as the property rather than a path: a
  // staged tree's derived root would be <extension>/staged, and what matters is
  // that the value supplied is independent of wherever the script happens to
  // live.
  const plan = installPlan(options({ stackDir: "/home/op/.memql/src" }));
  const cluster = plan({
    id: "clusterUp",
    script: "k3d.up",
    description: "",
    elevation: "none",
    retained: false,
    retainedReason: "",
    shared: false,
    sharedReason: "",
    verify: { kind: "scriptOk" },
  });
  if (cluster.action === "run") {
    assert.ok(!cluster.params["repo-root"].includes("staged"));
    assert.ok(cluster.params["repo-root"].endsWith(".memql/src"));
  }
});

// -----------------------------------------------------------------------------
// where the node images come from (memql#3572)
// -----------------------------------------------------------------------------

test("clusterUp is told to pull published images, not locally built ones", async () => {
  // The local overlay renames every node image to `memql-<node>:local`, which
  // exists only after a developer has built and imported it. An install has
  // built nothing, so without this every pod lands in ImagePullBackOff -- with
  // the cluster otherwise healthy, which is what made it hard to see.
  const plan = installPlan(options());
  const decision = plan({
    id: "clusterUp",
    script: "k3d.up",
    description: "",
    elevation: "none",
    retained: false,
    retainedReason: "",
    shared: false,
    sharedReason: "",
    verify: { kind: "scriptOk" },
  });

  assert.equal(decision.action, "run");
  if (decision.action === "run") {
    assert.equal(decision.params["image-registry"], DEFAULT_IMAGE_REGISTRY);
    // THE SAME RELEASE as the manifests -- pinning one and floating the other
    // would run this version's images against another version's deploy tree --
    // but in the IMAGE spelling. Git tags carry the `v`, image tags do not
    // (memql#3574), and asking a registry for `memql-bff:v0.16.0` is an
    // ImagePullBackOff whose cause is one character.
    assert.equal(decision.params["image-tag"], imageTagFor(DEFAULT_STACK_TAG));
    assert.doesNotMatch(decision.params["image-tag"]!, /^v/);
  }
});

test("an explicit tag moves the images with it", async () => {
  const plan = installPlan(options({ tag: "v9.9.9" }));
  const decision = plan({
    id: "clusterUp",
    script: "k3d.up",
    description: "",
    elevation: "none",
    retained: false,
    retainedReason: "",
    shared: false,
    sharedReason: "",
    verify: { kind: "scriptOk" },
  });
  if (decision.action === "run") {
    assert.equal(decision.params["image-tag"], "9.9.9");
  }
});

// -----------------------------------------------------------------------------
// the domain the operator typed (memql#3590)
// -----------------------------------------------------------------------------
//
// The page asks for a domain. It reached `seedBootstrap` and `enrolmentLink`, and
// three steps that need it took their own defaults instead:
//
//   hostsBlock  wrote api.memql.localhost / identity.memql.localhost / memql.localhost
//   localCA     issued for *.memql.localhost
//   frontDoor   probed api.memql.localhost / identity.memql.localhost
//
// while identity WAS bootstrapped for the typed domain -- so the cluster's issuer
// named one domain and its hosts block, certificate and front-door probe named
// another. Invisible at the default, because the field's default IS
// `memql.localhost`; broken for anyone who changed it, and surfacing at `frontDoor`
// as a DNS failure against hostnames nobody asked for.
//
// A field whose answer silently reaches two steps out of five is worse than a
// constant: it invites an answer that cannot be honoured.

/** A step shell, so each case names only what it is about. */
function stepFor(id: string, script: string): Step {
  return {
    id,
    script,
    description: "",
    elevation: "none",
    retained: false,
    retainedReason: "",
    shared: false,
    sharedReason: "",
    verify: { kind: "scriptOk" },
  };
}

function paramsFor(plan: (step: Step) => StepPlan, id: string, script: string): Record<string, string> {
  const decision = plan(stepFor(id, script));
  assert.equal(decision.action, "run", `${id} was not planned as a run`);
  return decision.action === "run" ? decision.params : {};
}

test("every step that needs the domain is told the domain", () => {
  const plan = installPlan(options({ domain: "memql.example.test" }));

  const hosts = paramsFor(plan, "hostsBlock", "install.hostsEntries");
  assert.ok(
    (hosts["hostnames"] ?? "").includes("api.memql.example.test"),
    `hostsBlock was given ${JSON.stringify(hosts["hostnames"])} -- without the typed domain it writes a\n` +
      `hosts block for names the operator never asked for, and nothing they DID ask for resolves`,
  );

  const ca = paramsFor(plan, "localCA", "install.mkcert");
  assert.ok(
    (ca["hostnames"] ?? "").includes("memql.example.test"),
    `localCA was given ${JSON.stringify(ca["hostnames"])} -- a certificate that does not cover the\n` +
      `front door is an untrusted front door`,
  );

  const door = paramsFor(plan, "frontDoor", "install.verifyFrontDoor");
  assert.ok(
    (door["hosts"] ?? "").includes("api.memql.example.test"),
    `frontDoor was given ${JSON.stringify(door["hosts"])} -- probing the wrong hostnames reports a\n` +
      `broken installer for a cluster that is fine`,
  );
});

// The three have to agree, and the failure of disagreement is silent: a hosts
// block for one name, a certificate for another, and a probe for a third would
// each look correct on its own.
test("the hosts block, the certificate and the probe name the same front door", () => {
  const plan = installPlan(options({ domain: "memql.example.test" }));
  const hosts = paramsFor(plan, "hostsBlock", "install.hostsEntries")["hostnames"] ?? "";
  const door = paramsFor(plan, "frontDoor", "install.verifyFrontDoor")["hosts"] ?? "";
  const ca = paramsFor(plan, "localCA", "install.mkcert")["hostnames"] ?? "";

  for (const host of door.split(",")) {
    assert.ok(
      hosts.split(",").includes(host),
      `frontDoor probes ${host}, which hostsBlock never pointed at 127.0.0.1 (${hosts})`,
    );
  }
  assert.ok(
    ca.split(",").some((n) => n === "*.memql.example.test" || n === "memql.example.test"),
    `the certificate (${ca}) does not cover the domain the other two use`,
  );
});

// NO DOMAIN, NO OVERRIDE. `present()` drops empty values, so a run without a
// domain must fall through to each script's own default rather than passing an
// empty flag -- which is a different thing from passing none, and the scripts
// treat it as one.
test("a run with no domain leaves every script's own default alone", () => {
  const plan = installPlan(options());
  for (const [id, script, key] of [
    ["hostsBlock", "install.hostsEntries", "hostnames"],
    ["localCA", "install.mkcert", "hostnames"],
    ["frontDoor", "install.verifyFrontDoor", "hosts"],
  ] as const) {
    const params = paramsFor(plan, id, script);
    assert.ok(
      !(key in params),
      `${id} was handed ${key}=${JSON.stringify(params[key])} with no domain collected`,
    );
  }
});

// The apex matters on its own: `memql.localhost` (no subdomain) is in the hosts
// block today, and dropping it would break any link written against the bare
// domain.
test("the hosts block carries the apex as well as the subdomains", () => {
  const plan = installPlan(options({ domain: "memql.example.test" }));
  const hosts = (paramsFor(plan, "hostsBlock", "install.hostsEntries")["hostnames"] ?? "").split(",");
  assert.ok(hosts.includes("memql.example.test"), `the apex is missing from ${hosts.join(",")}`);
});
