// The Add a cluster page's decisions, under bare `node --test`: what the
// landing offers for each verdict, what a run's state says while it stops or
// fails, how the log reads, and what the uninstall preview calls things.
//
// The panel (addClusterPanel.ts) imports `vscode`; everything here is the
// part it delegates to, so the rules are pinned where they are made.

import test from "node:test";
import assert from "node:assert/strict";

import type { ExecEvent, StepOutcome } from "../src/install/executor.js";
import type { Step } from "../src/install/graph.js";
import { removalRows, sharedToolRows, type RowStep } from "../src/install/removalPreview.js";
import { AddClusterState, landingOffers, landingView, type LandingFacts } from "../src/state/addCluster.js";
import { RunLogLines, messageFrom, remedyFrom } from "../src/state/stepRecords.js";
import { UninstallRunState } from "../src/state/uninstallRun.js";

function facts(over: Partial<LandingFacts>): LandingFacts {
  return { verdict: "absent", registered: false, hasReceipt: false, platform: "supported", signedIn: false, ...over };
}

function acts(f: LandingFacts): string[] {
  return landingView(f).choices.map((c) => c.act);
}

// -----------------------------------------------------------------------------
// the landing
// -----------------------------------------------------------------------------

test("nothing local: install, or connect to a cluster elsewhere", () => {
  assert.deepEqual(acts(facts({})), ["install", "connect"]);
  assert.equal(landingView(facts({})).state, "No local cluster");
});

test("an unsupported computer: one sentence, and connect", () => {
  const view = landingView(facts({ platform: "unsupported" }));
  assert.deepEqual(view.choices.map((c) => c.act), ["connect"]);
  assert.equal(view.line, "This computer can't run a local MemQL cluster.");
});

test("installed and listed: sign in, repair, uninstall -- and connect to another", () => {
  assert.deepEqual(acts(facts({ verdict: "installed-healthy", registered: true, hasReceipt: true })), [
    "signIn",
    "repair",
    "uninstall",
    "connect",
  ]);
  // Not answering: the fix first.
  assert.deepEqual(acts(facts({ verdict: "installed-unreachable", registered: true, hasReceipt: true })), [
    "repair",
    "signIn",
    "uninstall",
    "connect",
  ]);
  // Signed in already: MemQL OS, not a second sign-in.
  assert.equal(acts(facts({ verdict: "installed-healthy", registered: true, hasReceipt: true, signedIn: true }))[0], "openOs");
});

test("repair is offered only where there is an install to replay", () => {
  // A `make up` cluster listed by hand has no receipt; repairing it ran a
  // whole install from the default version (memql#5118 audit).
  assert.ok(!acts(facts({ verdict: "installed-healthy", registered: true, hasReceipt: false })).includes("repair"));
});

test("present but not in the list: connect to it, or uninstall it", () => {
  assert.deepEqual(acts(facts({ verdict: "installed-unreachable", registered: false, hasReceipt: true })), [
    "reconnect",
    "uninstall",
    "connect",
  ]);
  assert.deepEqual(acts(facts({ verdict: "present-unreceipted" })), ["adopt", "uninstall", "connect"]);
});

test("install is offered for nothing-local and nothing else, ever", () => {
  for (const verdict of ["installed-healthy", "installed-unreachable", "present-unreceipted"] as const) {
    for (const registered of [true, false]) {
      for (const hasReceipt of [true, false]) {
        assert.equal(landingOffers(facts({ verdict, registered, hasReceipt }), "install"), false, verdict);
      }
    }
  }
});

test("the landing's words are short and name no internal", () => {
  for (const f of [
    facts({}),
    facts({ platform: "unsupported" }),
    facts({ verdict: "installed-healthy", registered: true, hasReceipt: true }),
    facts({ verdict: "installed-unreachable", registered: false, hasReceipt: true }),
    facts({ verdict: "present-unreceipted" }),
  ]) {
    const view = landingView(f);
    assert.ok(view.state.split(" ").length <= 6, `"${view.state}" is more than six words`);
    for (const choice of view.choices) {
      assert.doesNotMatch(
        `${choice.label} ${choice.note}`,
        /--|\.\.\.|receipt|register|installer|guided|portal|console/i,
        `${choice.act}: ${choice.label} / ${choice.note}`,
      );
    }
  }
});

// -----------------------------------------------------------------------------
// a run's state: stopping, failing, and the script's own words
// -----------------------------------------------------------------------------

const STEP = (id: string, label: string, timeoutSeconds?: number): Step =>
  ({
    id,
    script: "x",
    label,
    description: `${label}.`,
    readOnly: false,
    elevation: "none",
    retained: false,
    retainedReason: "",
    shared: false,
    sharedReason: "",
    verify: { kind: "scriptOk" },
    ...(timeoutSeconds === undefined ? {} : { timeoutSeconds }),
  }) as Step;

function finished(step: Step, status: StepOutcome["status"], over: Partial<StepOutcome> = {}): ExecEvent {
  return {
    type: "stepFinished",
    step,
    outcome: {
      id: step.id,
      script: step.script,
      status,
      exitCode: status === "failed" ? 5 : 0,
      envelope: null,
      verified: status === "ok",
      preExisting: false,
      params: {},
      startedAt: "2026-09-28T10:00:00.000Z",
      finishedAt: "2026-09-28T10:00:05.000Z",
      ...over,
    },
  };
}

test("Cancel leaves the run on its screen, stopping, until it settles", () => {
  // THE DEFECT: cancel() moved to "done" at the click while the wave kept
  // working; a late failure then flipped the screen back.
  const s = new AddClusterState();
  s.chooseAction("install");
  s.setInput("ownerFirstName", "Ada");
  s.setInput("ownerLastName", "Lovelace");
  s.setInput("ownerEmail", "ada@example.com");
  assert.ok(s.beginRun());
  s.requestStop();
  assert.equal(s.screen, "running");
  assert.equal(s.stopping, true);
  s.finish({ ok: true, cancelled: true });
  assert.equal(s.screen, "done");
  assert.equal(s.cancelled, true);
  assert.equal(s.stopping, false);
  // A new attempt is not stopping.
  assert.ok(s.beginRun());
  assert.equal(s.stopping, false);
});

test("a dismissed password returns to the form with what was typed", () => {
  const s = new AddClusterState();
  s.chooseAction("install");
  s.setInput("ownerFirstName", "Ada");
  s.setInput("ownerLastName", "Lovelace");
  s.setInput("ownerEmail", "ada@example.com");
  assert.ok(s.beginRun());
  s.returnToCollect();
  assert.equal(s.screen, "collect");
  assert.equal(s.inputs.ownerFirstName, "Ada");
  assert.deepEqual(s.steps, []);
});

test("a step keeps its own ceiling, and its own words when it fails", () => {
  const s = new AddClusterState();
  const cluster = STEP("clusterUp", "Creating the cluster", 1800);
  s.apply({ type: "runStarted", steps: [{ id: "clusterUp", label: "Creating the cluster", description: "" }] });
  s.apply({ type: "stepStarted", step: cluster, params: {} });
  assert.equal(s.steps[0]?.timeoutSeconds, 1800);
  s.apply(
    finished(cluster, "failed", {
      exitCode: 124,
      reason: "exit 124: the script exited 0 but its verify did not hold",
      envelope: {
        ok: false,
        capability: "k3d.up",
        changed: false,
        result: { remedy: "k3d cluster delete memql" },
        error: { code: 124, message: "port 443 is already in use" },
      },
    }),
  );
  const failed = s.failed!;
  assert.equal(failed.message, "port 443 is already in use", "the script's own sentence, for the page");
  assert.match(failed.reason, /exit 124/, "the executor's account stays, for the log and the record");
  assert.equal(failed.remedy, "k3d cluster delete memql");
});

test("the uninstall run reads the remedy too, and stops like the install does", () => {
  // THE DEFECT: UninstallRunState never read `result.remedy`, so a hosts
  // file that needed the password got the wrong advice and no command.
  const u = new UninstallRunState();
  u.begin();
  const hosts = STEP("removeHostsBlock", "Removing local addresses");
  u.apply({ type: "runStarted", steps: [{ id: "removeHostsBlock", label: "Removing local addresses", description: "" }] });
  u.apply({ type: "stepStarted", step: hosts, params: {} });
  u.requestStop();
  assert.equal(u.phase, "running");
  assert.equal(u.stopping, true);
  u.apply(
    finished(hosts, "failed", {
      exitCode: 4,
      envelope: {
        ok: false,
        capability: "install.removeArtifact",
        changed: false,
        result: { remedy: "sudo remove-artifact.sh --kind=hostsEntries" },
        error: { code: 4, message: "couldn't edit /etc/hosts" },
      },
    }),
  );
  assert.equal(u.failure?.remedy, "sudo remove-artifact.sh --kind=hostsEntries");
  assert.equal(u.failure?.message, "couldn't edit /etc/hosts");
  u.finish({ ok: false });
  assert.equal(u.stopping, false);
});

test("the script's words and fix are read defensively off the envelope", () => {
  assert.equal(remedyFrom({ result: { remedy: "  sudo x  " } }), "sudo x");
  assert.equal(remedyFrom({ result: { remedy: 42 } }), "");
  assert.equal(remedyFrom(null), "");
  assert.equal(messageFrom({ error: { message: "it broke" } }), "it broke");
  assert.equal(messageFrom({ result: { reason: "not ready" } }), "not ready");
  assert.equal(messageFrom({ error: { message: "" }, result: {} }), "");
});

test("the connect form is pristine until something is typed", () => {
  const s = new AddClusterState();
  s.chooseAction("connect");
  assert.equal(s.connectIsPristine, true);
  s.setConnectInput("name", "staging");
  assert.equal(s.connectIsPristine, false);
  s.setConnectInput("name", "  ");
  assert.equal(s.connectIsPristine, true);
});

// -----------------------------------------------------------------------------
// the log: chronological, a step's label once where its lines begin
// -----------------------------------------------------------------------------

test("a step's label is drawn once, where its lines begin, and again when it takes over again", () => {
  const log = new RunLogLines();
  const lines = [
    log.add("detect", "Checking this computer", "Platform: darwin/arm64"),
    log.add("detect", "Checking this computer", "k3d present=false"),
    log.add("toolK3d", "Installing tools", "Downloading k3d"),
    log.add("hostsBlock", "Adding local addresses", "3 entries"),
    log.add("toolK3d", "Installing tools", "sha256 verified"),
    log.add("toolK3d", "Installing tools", "ERROR: disk full"),
  ];
  assert.deepEqual(
    lines.map((l) => l.label ?? ""),
    ["Checking this computer", "", "Installing tools", "Adding local addresses", "Installing tools", ""],
  );
  assert.equal(lines[5]?.tone, "error", "an error line reads as one");
  assert.match(log.text(), /^Checking this computer\n {2}Platform: darwin\/arm64\n {2}k3d present=false\nInstalling tools/);
});

test("the log opens at the failed step's first line", () => {
  const log = new RunLogLines();
  log.add("detect", "Checking this computer", "a");
  log.add("clusterUp", "Creating the cluster", "b");
  log.add("detect", "Checking this computer", "c");
  log.add("clusterUp", "Creating the cluster", "d");
  const anchored = log.anchoredAt("clusterUp");
  assert.deepEqual(
    anchored.map((l) => l.anchor === true),
    [false, true, false, false],
    "exactly one anchor, on the first line of the failed step",
  );
});

test("the log is bounded, oldest first to go", () => {
  const log = new RunLogLines(3);
  for (let i = 0; i < 5; i += 1) log.add("s", "Step", `line ${i}`);
  assert.deepEqual(log.lines().map((l) => l.text), ["line 2", "line 3", "line 4"]);
});

// -----------------------------------------------------------------------------
// the uninstall preview's rows
// -----------------------------------------------------------------------------

function step(id: string, over: Partial<RowStep> = {}): RowStep {
  return {
    id,
    description: "Removing something.",
    action: "run",
    reason: "",
    preserved: false,
    target: "",
    elevation: "none",
    shared: false,
    sharedReason: "A long paragraph about shared tools.",
    ...over,
  };
}

const HOME = "/Users/ada";

test("a removal is named for what it is, its place masked, and a kept one says why", () => {
  const rows = removalRows(
    {
      removals: [
        step("removeCluster", { params: { kind: "stack", cluster: "memql" } }),
        step("removeCheckout", { params: { kind: "checkout", path: `${HOME}/.memql/src` } }),
        step("removeHostsBlock", { elevation: "sudo", params: { kind: "hostsEntries", path: "/etc/hosts" } }),
        step("removeLocalCA", { shared: true, params: { kind: "mkcertCA", caroot: `${HOME}/.memql/mkcert` } }),
      ],
      preserved: [step("removeImages", { preserved: true, description: "Removing local images.", params: {} })],
    },
    HOME,
  );
  assert.deepEqual(
    rows.map((r) => [r.name, r.detail, r.kept, r.asks ?? ""]),
    [
      ["The cluster", "memql, and everything running in it", false, ""],
      ["Downloaded MemQL files", "~/.memql/src", false, ""],
      ["Local addresses", "/etc/hosts", false, "password"],
      ["Removing local images", "", true, ""],
    ],
    "shared removals are the switches, not rows",
  );
  assert.equal(rows[3]?.reason, "Was here before MemQL");
  for (const row of rows) assert.doesNotMatch(row.detail, /\/Users\/ada|^(path|cluster|caroot|hosts-file) /);
});

test("shared tools are switches by name, and a dependent says what it needs", () => {
  const tools = sharedToolRows({
    removals: [
      step("removeLocalCA", { shared: true, dependsOn: ["removeCluster"] }),
      step("removeToolK3d", { shared: true, dependsOn: ["removeCluster"] }),
      step("removeToolKubectl", { shared: true, dependsOn: ["removeCluster"] }),
      step("removeToolMkcert", { shared: true, dependsOn: ["removeLocalCA"] }),
      step("removeCluster", { shared: false }),
    ],
  });
  assert.deepEqual(
    tools.map((t) => [t.name, t.requires ?? ""]),
    [
      ["k3d", ""],
      ["kubectl", ""],
      ["Local certificate authority", ""],
      ["mkcert", "removeLocalCA"],
    ],
    "mkcert sits under the authority it cannot be removed without; the cluster is no shared choice",
  );
  for (const tool of tools) assert.ok(tool.note.split(" ").length <= 8, `"${tool.note}" is a paragraph`);
});
