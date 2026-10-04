// One run from the history, on its own page (memql#4427).
//
// THE RISK IN SURFACING RECORDED DATA IS THAT THE PAGE INVENTS. A duration
// for a run still in flight, a culprit for a failure nothing named -- each
// reads as a fact about the run when it is a fact about the read. So: every
// run kind renders, the defaultable facts are OMITTED rather than defaulted,
// and the page speaks in words -- a step's label, not its id; a sentence, not
// an exit code; a local time, not an RFC3339 stamp -- with the raw record one
// click away under Details.
//
// Which acts the page offers is instanceActions' (test/instanceActions.test.ts:
// Retry as the act the run came from, a rollback aimed at THIS run).

import test from "node:test";
import assert from "node:assert/strict";

import { roleVisibility, visibleActions } from "../src/deploy/actions.js";
import { runDetailBar } from "../src/deploy/instanceActions.js";
import { newLocalRun, type Instance, type Run, type RunKind, type RunStatus } from "../src/state/deployments.js";
import { parseItemDetail, runNoun, stepGroups } from "../src/state/deploymentsCatalog.js";
import { runDetailScreen, stepLogDisclosureId, stepLogFileOf } from "../src/webview/deploymentScreens.js";

const NOW = Date.parse("2026-08-14T12:00:00Z");

const LOCAL: Instance = {
  name: "local",
  kind: "local",
  presence: "installed-healthy",
  connected: true,
  registered: true,
  version: "v0.19.1",
  checkout: "/home/dev/memql",
};

const REMOTE: Instance = {
  name: "staging",
  kind: "remote",
  presence: "installed-healthy",
  connected: true,
  version: "v0.9.2",
  currentDeploymentId: "dep-2",
};

const LABELS = new Map([
  ["detect", "Checking this computer"],
  ["toolK3d", "Installing tools"],
  ["toolKubectl", "Installing tools"],
  ["stackCheckout", "Downloading MemQL"],
  ["clusterUp", "Creating the cluster"],
  ["seedBootstrap", "Creating your account"],
]);

function runOf(over: Partial<Run> = {}): Run {
  return {
    ...newLocalRun({ id: "run-1", instance: "local", kind: "upgrade", startedAt: "2026-08-14T10:00:00Z" }),
    ...over,
  };
}

function page(over: { instance?: Instance; run?: Run; logs?: Map<string, string>; open?: Set<string> } = {}): string {
  const instance = over.instance ?? LOCAL;
  const run = over.run ?? runOf();
  const parts = runDetailScreen({
    instance,
    run,
    bar: runDetailBar({
      instance,
      run,
      connection: "connected",
      pipeline: { kind: "present", line: "", engineMessage: "", actions: visibleActions(roleVisibility("owner")), rollouts: [] },
      visibility: roleVisibility("owner"),
      runInFlight: false,
    }),
    nowMs: NOW,
    labels: LABELS,
    logs: over.logs ?? new Map(),
    openLogs: over.open ?? new Set(),
    detailsOpen: false,
  });
  // The kit escapes an apostrophe; the assertions read the words.
  return (parts.head + parts.body + parts.actions).replace(/&#39;/g, "'");
}

/** The page without its Details disclosure, which is where raw stamps and ids belong. */
function visible(html: string): string {
  return html.replace(/<div class="mq-disclosure-body" id="dp-details"[\s\S]*$/, "");
}

// ---------------------------------------------------------------------------
// every run kind renders
// ---------------------------------------------------------------------------

const KINDS: readonly RunKind[] = ["install", "upgrade", "repair", "uninstall", "rebuild", "update", "rollout"];

for (const kind of KINDS) {
  test(`a ${kind} run heads its page with what it was, and its outcome is on the bar`, () => {
    const instance = kind === "rollout" ? REMOTE : LOCAL;
    const run = runOf({
      kind,
      instance: instance.name,
      status: "succeeded",
      finishedAt: "2026-08-14T10:04:12Z",
      ...(kind === "rebuild" ? {} : { fromVersion: "v0.19.0", toVersion: "v0.19.1" }),
    });
    const html = page({ instance, run });
    assert.ok(html.includes(`>${runNoun(run)}</h1>`), `${kind} did not head the page`);
    assert.ok(html.includes("Succeeded"), `${kind} did not say how it ended`);
    assert.ok(html.includes("4m 12s"), `${kind} did not print its duration`);
    // Back to the cluster it belongs to.
    assert.ok(html.includes(`data-act="back"`) && html.includes(instance.name));
  });
}

const STATUSES: readonly RunStatus[] = ["running", "succeeded", "failed", "cancelled", "interrupted", "superseded", "rolled_back"];

for (const status of STATUSES) {
  test(`a ${status} run says so in words, not as the raw enum`, () => {
    const html = page({ instance: REMOTE, run: runOf({ kind: "rollout", status, instance: "staging" }) });
    assert.ok(!html.includes(">rolled_back<") && !html.includes(">superseded<"));
    assert.match(html, /mq-actbar-word">[A-Z][a-z]+/);
  });
}

test("a run still in flight is given no duration", () => {
  assert.ok(!page({ run: runOf({ status: "running" }) }).includes(">Took<"));
});

test("an interrupted run keeps its start, admits no finish, and says what happened", () => {
  const html = page({ run: runOf({ status: "interrupted" }) });
  assert.ok(html.includes(">Started<"));
  assert.ok(!html.includes(">Took<"), "an interrupted run was given a duration");
  assert.ok(html.includes("This run stopped when the editor closed."));
});

test("a run that recorded no versions prints no transition", () => {
  assert.ok(!page({ run: runOf({ kind: "repair" }) }).includes("→"));
});

test("times are this machine's words, and the raw stamps are only under Details", () => {
  const html = page({ run: runOf({ status: "succeeded", finishedAt: "2026-08-14T10:04:12Z" }) });
  assert.ok(!visible(html).includes("2026-08-14T10:00:00Z"), "an RFC3339 stamp reached the page");
  assert.ok(html.includes("2026-08-14T10:00:00Z"), "the stamp left Details too");
  assert.match(visible(html), /\d{1,2} Aug, \d{2}:\d{2}/);
});

// ---------------------------------------------------------------------------
// what failed, in words
// ---------------------------------------------------------------------------

const FAILED: Run = runOf({
  status: "failed",
  finishedAt: "2026-08-14T10:02:00Z",
  items: [
    { label: "detect", status: "skipped", detail: "the condition dependents needed already holds" },
    { label: "toolK3d", status: "skipped" },
    { label: "toolKubectl", status: "skipped" },
    { label: "stackCheckout", status: "ok", detail: "commit=abc1234 dest=/home/dev/memql refKind=tag" },
    { label: "clusterUp", status: "failed", detail: "Port 443 is already in use. · log=run-1.clusterUp.log" },
    { label: "seedBootstrap", status: "pending" },
  ],
});

test("a failed run says why in one sentence, from the step that failed", () => {
  const html = page({ run: FAILED });
  assert.ok(html.includes("Port 443 is already in use."));
  assert.ok(html.includes('role="alert"'));
});

test("steps are named in words, and the ones already in place are one line", () => {
  const html = visible(page({ run: FAILED }));
  assert.ok(html.includes("Downloading MemQL"));
  assert.ok(html.includes("Creating the cluster"));
  assert.ok(!html.includes(">stackCheckout<"), "a step id reached the page");
  assert.ok(!html.includes("commit=abc1234"), "a raw result reached the page");
  assert.ok(html.includes("3 already in place"));
  assert.ok(html.includes("1 didn't run"));
});

test("a failed step's saved log opens beneath it, keyed by its file", () => {
  const html = page({ run: FAILED, logs: new Map([["run-1.clusterUp.log", "Bind for 0.0.0.0:443 failed"]]), open: new Set(["run-1.clusterUp.log"]) });
  assert.ok(html.includes(`data-disclosure="${stepLogDisclosureId("run-1.clusterUp.log")}"`));
  assert.ok(html.includes("Bind for 0.0.0.0:443 failed"));
  assert.equal(stepLogFileOf(stepLogDisclosureId("run-1.clusterUp.log")), "run-1.clusterUp.log");
  assert.equal(stepLogFileOf("dp-details"), "");
});

test("a failed run with no failed item invents no culprit", () => {
  const html = page({ run: runOf({ status: "failed", items: [{ label: "detect", status: "ok" }] }) });
  assert.ok(!html.includes('role="alert"'));
});

test("the recorded detail is taken apart: sentence, exit code, log, fields", () => {
  const parsed = parseItemDetail("Port 443 is busy. · exit 4 · commit=abc dest=/x · log=r.s.log");
  assert.equal(parsed.reason, "Port 443 is busy.");
  assert.equal(parsed.exitCode, 4);
  assert.equal(parsed.logFile, "r.s.log");
  assert.equal(parsed.fields, "commit=abc dest=/x");
  assert.equal(parseItemDetail("the condition dependents needed already holds").alreadyInPlace, true);
  assert.deepEqual(parseItemDetail(undefined), { reason: "", alreadyInPlace: false, fields: "", logFile: "" });
});

test("a step that failed without a sentence is explained, never shown as 'exit 4'", () => {
  const html = visible(page({ run: runOf({ status: "failed", items: [{ label: "clusterUp", status: "failed", detail: "exit 4" }] }) }));
  assert.ok(!html.includes("exit 4"));
  assert.ok(html.includes('role="alert"'));
});

test("consecutive steps under one label are one line, and the worst status speaks for it", () => {
  const groups = stepGroups(
    [
      { label: "toolK3d", status: "ok" },
      { label: "toolKubectl", status: "failed" },
      { label: "stackCheckout", status: "ok" },
    ],
    LABELS,
  );
  assert.deepEqual(
    groups.map((g) => [g.label, g.status, g.items.length]),
    [
      ["Installing tools", "failed", 2],
      ["Downloading MemQL", "ok", 1],
    ],
  );
});

// ---------------------------------------------------------------------------
// local steps, remote services
// ---------------------------------------------------------------------------

test("a local run's items are Steps and a remote run's are Services, the digest under Details", () => {
  const local = page({ run: runOf({ items: [{ label: "clusterUp", status: "ok" }] }) });
  assert.ok(local.includes(">Steps</h2>") && !local.includes(">Services</h2>"));

  const remote = page({
    instance: REMOTE,
    run: runOf({
      kind: "rollout",
      instance: "staging",
      status: "succeeded",
      items: [{ label: "bff", status: "ok", detail: "v0.9.2 (inherited) · 2 replicas · digest sha256:abcd123" }],
    }),
  });
  assert.ok(remote.includes(">Services</h2>") && !remote.includes(">Steps</h2>"));
  assert.ok(remote.includes("v0.9.2 (inherited) · 2 replicas"));
  assert.ok(!visible(remote).includes("sha256:abcd123"), "a digest reached the service line");
  assert.ok(remote.includes("sha256:abcd123"), "the digest left Details too");
});

test("no items says which no-items case it is", () => {
  assert.ok(page().includes("No steps were recorded."));
  assert.ok(page({ instance: REMOTE, run: runOf({ kind: "rollout", instance: "staging" }) }).includes("No service details for this deployment."));
});
