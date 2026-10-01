// The Deployments panel's deploy-control buttons, driven end to end: the page
// the panel renders, the message a click posts, and what reaches the wire.
//
// THROUGH THE REAL PANEL AND A REAL SDK CLIENT. The four defects fixed here
// were each invisible below that line:
//
//   - Rollout promote / abort posted `rolloutAction`, and the panel sent
//     `promote` with an empty rollout name -- which the SDK refuses before
//     sending, so nothing ever reached the cluster, and abort was never
//     offered.
//   - Cut version always sent a patch bump.
//   - `previewNextVersion` had no caller, so no button could name the version
//     it would cut.
//   - Roll back sent the newest `succeeded` deployment, which is usually the one
//     the cluster is running.
//
// A fake port would have accepted the empty name and reported success; the
// dispatcher under test/support/deployDispatcher.ts answers for the ENGINE
// instead, so the SDK's own argument checks run exactly as they do for an
// operator.
//
// On the page every deploy control is a kit act, `data-act="deploy"
// data-value="<key>"`, posting `{ type: "deploy", value: <key> }`. The page
// says what happened in a sentence; the engine's log line goes to Output and
// the audit id under Details.

import test from "node:test";
import assert from "node:assert/strict";

import type { Row } from "@znasllc-io/memql-sdk-core/client";
import type { ExtensionContext } from "vscode";

import { roleVisibility } from "../src/deploy/actions.js";
import { DeploymentPanel, type DeploymentPanelDeps } from "../src/webview/deploymentPanel.js";
import { actionOk, DeployDispatcher } from "./support/deployDispatcher.js";
import { recorded, resetRecorded, type StubWebviewPanel } from "./support/vscodeStub.js";

const CONTEXT = { subscriptions: [] as { dispose(): unknown }[] } as unknown as ExtensionContext;

function deploymentRow(id: string, status: string, day: number, version: string): Row {
  return {
    id: `v1:cluster:deployment:${id}`,
    concept: "v1:cluster:deployment",
    createdAt: `2026-09-${String(day).padStart(2, "0")}T00:00:00Z`,
    payload: { deploymentId: id, status, version, imageDigest: `sha256:${id}` },
  };
}

/** d2 landed last and is what the cluster runs; d1 is the release before it. */
const HISTORY: Row[] = [
  deploymentRow("d1", "succeeded", 1, "1.4.1"),
  deploymentRow("d2", "succeeded", 10, "1.4.2"),
];

const STATUS = {
  ok: true,
  deploymentStatus: {
    version: "1.4.2",
    rollouts: [
      { name: "memql-bff", kind: "bluegreen", phase: "Paused" },
      { name: "memql-agent", kind: "canary", phase: "Healthy" },
    ],
  },
};

const SUGGESTION = {
  ok: true,
  nextVersion: {
    currentVersion: "1.4.2",
    nextPatch: "1.4.3",
    nextMinor: "1.5.0",
    nextMajor: "2.0.0",
    source: "deployment",
  },
};

interface Harness {
  page: StubWebviewPanel;
  dispatcher: DeployDispatcher;
  /** Every type-to-confirm prompt the panel raised, with its title and phrase. */
  prompts: { title: string; prompt: string; phrase: string }[];
  /** Every line the panel sent to the Output channel. */
  logged: string[];
  /** Posts a deploy-control button's key, as a click would, and waits for the page to settle. */
  press(key: string): Promise<void>;
  close(): void;
}

/**
 * Opens the page on a connected remote cluster, as an owner, and waits for the
 * status read to land.
 *
 * `answer` is what the confirmation prompt types back; the default types the
 * phrase it was asked for.
 */
async function open(
  over: {
    dispatcher?: DeployDispatcher;
    answer?: (phrase: string) => string | undefined;
  } = {},
): Promise<Harness> {
  resetRecorded();
  const dispatcher =
    over.dispatcher ??
    new DeployDispatcher().answer("getDeploymentStatus", STATUS).answer("suggestNextVersion", SUGGESTION);
  const prompts: { title: string; prompt: string; phrase: string }[] = [];
  const logged: string[] = [];
  const client = dispatcher.client();
  const deps: DeploymentPanelDeps = {
    catalog: {
      clustersPath: "/nowhere/clusters.yaml",
      receiptPath: "/nowhere/install-receipt.json",
      runsDir: "/nowhere/runs",
      presence: async () => ({
        verdict: "absent",
        evidence: { receipt: false, registry: false, liveCluster: false },
        endpoint: "",
      }),
      readClusters: async () => ({
        ok: true as const,
        file: { clusters: [{ name: "staging", endpoint: "a:443" }], selectedCluster: "" },
      }),
      readReceiptFile: async () => null,
      listRunsIn: async () => [],
    },
    installRoot: "/nowhere",
    receiptFile: "/nowhere/install-receipt.json",
    refreshTree: () => undefined,
    openInstallFlow: () => undefined,
    connection: () => ({ clusterName: "staging", connected: true }),
    readDeployments: () => async () => ({ deployments: HISTORY, specs: [] }),
    deployPort: () => client,
    readRole: async () => roleVisibility("owner"),
    confirm: async ({ title, prompt, phrase }) => {
      prompts.push({ title, prompt, phrase });
      return over.answer === undefined ? phrase : over.answer(phrase);
    },
    logLine: (line) => logged.push(line),
  };
  DeploymentPanel.show(CONTEXT, deps, "staging");
  const page = recorded.webviews.at(-1);
  assert.ok(page !== undefined, "no webview was created");
  await until(() => page.html.includes('data-act="deploy"'), "the deploy controls to render");
  return {
    page,
    dispatcher,
    prompts,
    logged,
    async press(key: string): Promise<void> {
      const renders = page.renders;
      page.send({ type: "deploy", value: key });
      await until(() => page.renders > renders, `the page to repaint after ${key}`);
      // A successful action re-reads the catalog and the status, which is two
      // more paints; settle until the page stops changing.
      await settle(page);
    },
    close: () => page.close(),
  };
}

async function tick(): Promise<void> {
  await new Promise((resolve) => setImmediate(resolve));
}

async function until(check: () => boolean, what: string): Promise<void> {
  for (let i = 0; i < 500; i++) {
    if (check()) return;
    await tick();
  }
  assert.fail(`timed out waiting for ${what}`);
}

async function settle(page: StubWebviewPanel): Promise<void> {
  let renders = -1;
  for (let i = 0; i < 500 && renders !== page.renders; i++) {
    renders = page.renders;
    for (let j = 0; j < 10; j++) await tick();
  }
}

// -----------------------------------------------------------------------------
// Rollout promote / abort
// -----------------------------------------------------------------------------

test("promote names the rollout, and the request reaches the wire", async () => {
  const h = await open();
  try {
    // One pair per rollout IN FLIGHT, on that rollout's row: memql-agent is
    // Healthy, so it has neither a row nor an act.
    assert.match(h.page.html, /data-act="deploy" data-value="rolloutAction:promote:memql-bff"/);
    assert.match(h.page.html, /data-act="deploy" data-value="rolloutAction:abort:memql-bff"/);
    assert.match(h.page.html, /aria-label="Promote memql-bff"/);
    assert.doesNotMatch(h.page.html, /memql-agent/);

    await h.press("rolloutAction:promote:memql-bff");
    assert.deepEqual(h.dispatcher.calls("rolloutAction"), [{ rollout: "memql-bff", action: "promote" }]);
    assert.deepEqual(h.prompts, [], "promote is immediate; it asked for a confirmation");
    assert.match(h.page.html, /Rollout promoted\./);
    assert.ok(h.logged.some((line) => line.startsWith("SUCCESS: rollout_action")), "the engine's line did not reach Output");
  } finally {
    h.close();
  }
});

test("abort is offered, and confirmed against the rollout's name before it is sent", async () => {
  const h = await open();
  try {
    h.dispatcher.answer("rolloutAction", actionOk("audit-abort"));
    await h.press("rolloutAction:abort:memql-bff");
    assert.equal(h.prompts.length, 1);
    assert.equal(h.prompts[0].phrase, "memql-bff");
    assert.equal(h.prompts[0].title, "Abort memql-bff");
    assert.match(h.prompts[0].prompt, /Abort the memql-bff rollout\?/);
    assert.deepEqual(h.dispatcher.calls("rolloutAction"), [{ rollout: "memql-bff", action: "abort" }]);
    assert.match(h.page.html, /Rollout aborted\./);
    // The audit id is kept for a support case, under Details.
    assert.match(h.page.html, /<dt>Audit reference<\/dt><dd class="mq-mono">audit-abort<\/dd>/);
  } finally {
    h.close();
  }
});

test("an abort whose phrase does not match sends nothing", async () => {
  const h = await open({ answer: () => "staging" });
  try {
    await h.press("rolloutAction:abort:memql-bff");
    assert.deepEqual(h.dispatcher.calls("rolloutAction"), []);
    assert.match(h.page.html.replace(/&#39;/g, "'"), /That didn't match, so nothing changed\./);
  } finally {
    h.close();
  }
});

test("the old message -- the action with no rollout -- sends nothing", async () => {
  // What the single "Rollout promote / abort" button used to post. It names no
  // control this page built, so it resolves to nothing rather than to an
  // empty-named request.
  const h = await open();
  try {
    await h.press("rolloutAction");
    assert.deepEqual(h.dispatcher.calls("rolloutAction"), []);
    assert.match(h.page.html.replace(/&#39;/g, "'"), /That's out of date, so nothing ran\./);
  } finally {
    h.close();
  }
});

test("with no rollout in flight the page says so instead of drawing a button", async () => {
  const dispatcher = new DeployDispatcher()
    .answer("getDeploymentStatus", {
      ok: true,
      deploymentStatus: { rollouts: [{ name: "memql-bff", kind: "bluegreen", phase: "Healthy" }] },
    })
    .answer("suggestNextVersion", SUGGESTION);
  const h = await open({ dispatcher });
  try {
    assert.doesNotMatch(h.page.html, /data-value="rolloutAction/);
    assert.match(h.page.html, /Rollouts<\/h2><div class="mq-empty"><p class="mq-empty-line">None in progress\.<\/p>/);
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// Cut version, and the preview that names it
// -----------------------------------------------------------------------------

test("the page reads the version preview and offers a cut per bump, naming each version", async () => {
  const h = await open();
  try {
    assert.equal(h.dispatcher.calls("suggestNextVersion").length, 1, "previewNextVersion was never called");
    // "Prepare" is the page's word for a cut; the bump is the quiet note beside it.
    for (const [bump, version] of [["patch", "1.4.3"], ["minor", "1.5.0"], ["major", "2.0.0"]]) {
      const v = version.replace(/\./g, "\\.");
      assert.match(
        h.page.html,
        new RegExp(`data-act="deploy" data-value="cutVersion:${bump}:${v}"[^>]*><span class="dp-row-label">Prepare ${v}</span><span class="dp-row-desc">${bump}<`),
      );
    }
  } finally {
    h.close();
  }
});

test("Cut sends the bump and the version the pressed button named", async () => {
  const h = await open();
  try {
    await h.press("cutVersion:minor:1.5.0");
    assert.deepEqual(h.dispatcher.calls("cutVersion"), [{ bump: "minor", version: "1.5.0" }]);
    assert.match(h.page.html, /1\.5\.0 is ready to deploy\./);
  } finally {
    h.close();
  }
});

test("a failed preview still offers every bump, and the engine computes the version", async () => {
  const dispatcher = new DeployDispatcher()
    .answer("getDeploymentStatus", STATUS)
    .answer("suggestNextVersion", { ok: false, errorCode: 14, errorMessage: "no identity peer" });
  const h = await open({ dispatcher });
  try {
    assert.match(h.page.html, /data-value="cutVersion:major"[^>]*><span class="dp-row-label">Prepare next major</);
    assert.match(h.page.html.replace(/&#39;/g, "'"), /Couldn't read the next version numbers\./);
    // The engine's own words are kept under Details.
    assert.match(h.page.html, /<dt>Next version<\/dt><dd>[^<]*no identity peer/);
    await h.press("cutVersion:major");
    // No version: the SDK omits an empty one, and the engine bumps the
    // current version itself.
    assert.deepEqual(h.dispatcher.calls("cutVersion"), [{ bump: "major" }]);
  } finally {
    h.close();
  }
});

// -----------------------------------------------------------------------------
// Roll back
// -----------------------------------------------------------------------------

test("Roll back returns to the release before the one running, never the running one", async () => {
  const h = await open();
  try {
    // Named before anything is pressed: the act says the release, Details the record.
    assert.match(h.page.html, /data-act="deploy" data-value="rollback:d1"[^>]*>Roll back to 1\.4\.1…</);
    assert.match(h.page.html, /<dt>Roll back to<\/dt><dd class="mq-mono">d1<\/dd>/);
    assert.doesNotMatch(h.page.html, /rollback:d2/);
    await h.press("rollback:d1");
    assert.equal(h.prompts.length, 1);
    assert.equal(h.prompts[0].phrase, "d1");
    assert.deepEqual(h.dispatcher.calls("rollbackDeployment"), [{ toDeploymentId: "d1" }]);
  } finally {
    h.close();
  }
});

test("a rollback key naming the running release sends nothing", async () => {
  // What the button used to send, posted directly: d2 is current, so no
  // control this page built names it.
  const h = await open();
  try {
    await h.press("rollback:d2");
    assert.deepEqual(h.dispatcher.calls("rollbackDeployment"), []);
    assert.deepEqual(h.prompts, []);
  } finally {
    h.close();
  }
});
