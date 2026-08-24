// The run log, behind a disclosure, held in state and not in the DOM
// (memql#4455).
//
// THE FAILURE THIS PREVENTS is not "the toggle does not work". It is that the
// toggle works for about a second: both panels re-render by assigning
// `webview.html`, which replaces the entire document, and during a run that
// happens on every `stepLog`. A `<details>` an operator opened would close
// itself while they were reading it, roughly once a second, for the whole part
// of an install that produces output. Which is why the open/closed flag is a
// field on the state module and the pane is emitted only when that field says
// so -- and why the assertions below are about STATE transitions rather than
// about a click.
//
// AND WHY FAILURE IS THE EXCEPTION. Collapsing the log is for the twelve
// minutes an install is going well. At the moment something breaks, the log IS
// the product, and making the operator find a toggle first would be design
// spite -- so the state module opens it and the pane anchors on the step that
// failed.
//
// Refs: #4455 #4194 #4452

import test from "node:test";
import assert from "node:assert/strict";

import { AddClusterState } from "../src/state/addCluster.js";
import { UninstallRunState } from "../src/state/uninstallRun.js";
import type { StepProgress } from "../src/state/addCluster.js";
import { logLineCount, renderRunLogPane } from "../src/webview/runLogPane.js";
import { renderFailedScreen, renderRunningScreen } from "../src/webview/installScreens.js";

const HOME = "/home/operator";

function step(over: Partial<StepProgress> = {}): StepProgress {
  return {
    id: "clusterUp",
    description: "Creating the cluster",
    state: "running",
    reason: "",
    exitCode: null,
    log: "",
    guided: false,
    remedy: "",
    ...over,
  };
}

/** The executor events the state folds, in the shape `apply` reads. */
function started(id: string, description: string): never {
  return { type: "stepStarted", step: { id, description } } as never;
}
function finished(id: string, description: string, status: string, exitCode: number): never {
  return {
    type: "stepFinished",
    step: { id, description },
    outcome: { status, exitCode, reason: "", envelope: {} },
  } as never;
}

// --- the state transitions ---------------------------------------------------

test("the pane starts closed, because a run going well is not a log", () => {
  const state = new AddClusterState();
  assert.equal(state.logsOpen, false);
  assert.equal(state.logsFollow, true, "a pane opened mid-run lands on what is happening now");
});

test("the toggle opens and closes, and opening re-arms the tail", () => {
  const state = new AddClusterState();
  state.toggleLogs();
  assert.equal(state.logsOpen, true);

  // The operator scrolled up to read something. The next render must respect it.
  state.setLogsFollow(false);
  assert.equal(state.logsFollow, false);

  state.toggleLogs();
  assert.equal(state.logsOpen, false);
  assert.equal(state.logsFollow, false, "closing decides nothing about the tail");

  // Re-opening is a pane nobody has scrolled yet, so the honest landing place
  // is whatever is happening now.
  state.toggleLogs();
  assert.equal(state.logsOpen, true);
  assert.equal(state.logsFollow, true);
});

test("scrolling back to the bottom re-arms follow without a repaint", () => {
  const state = new AddClusterState();
  state.setLogsFollow(false);
  state.setLogsFollow(true);
  assert.equal(state.logsFollow, true);
});

test("A STEP FAILING OPENS THE PANE", () => {
  const state = new AddClusterState();
  assert.equal(state.logsOpen, false);
  state.apply(started("clusterUp", "Creating the cluster"));
  assert.equal(state.logsOpen, false, "a step merely running discloses nothing");
  state.apply(finished("clusterUp", "Creating the cluster", "failed", 5));
  assert.equal(state.logsOpen, true, "at failure the log is the product");
});

test("a new run starts closed, rather than on the last run's output", () => {
  const state = new AddClusterState();
  state.apply(finished("clusterUp", "Creating the cluster", "failed", 5));
  assert.equal(state.logsOpen, true);
  state.setLogsFollow(false);

  state.chooseAction("install");
  for (const [field, value] of [
    ["domain", "memql.localhost"],
    ["ownerFirstName", "A"],
    ["ownerLastName", "B"],
    ["ownerEmail", "a@b.test"],
    ["version", "v0.19.1"],
  ] as const) {
    state.setInput(field, value);
  }
  assert.equal(state.beginRun(), true, "the form is complete");
  assert.equal(state.logsOpen, false, "the disclosure belonged to the failure, not to the operator");
  assert.equal(state.logsFollow, true);
});

test("an uninstall holds its own disclosure, and discloses on its own failure", () => {
  // A SECOND PAIR OF FIELDS, not a shared one: they are two different runs with
  // two different step lists, and an operator who opened the log on a failed
  // install should meet a closed pane when they start a removal.
  const uninstall = new UninstallRunState();
  assert.equal(uninstall.logsOpen, false);
  uninstall.apply(finished("removeCluster", "Removing the cluster", "failed", 3));
  assert.equal(uninstall.logsOpen, true);

  uninstall.begin();
  assert.equal(uninstall.logsOpen, false, "a fresh removal starts closed");
});

// --- what the pane renders ---------------------------------------------------

test("an empty log renders a disabled control, never a toggle that opens nothing", () => {
  const html = renderRunLogPane({ steps: [step()], open: false, follow: true, home: HOME });
  assert.match(html, /No output yet/);
  assert.match(html, /disabled/);
  assert.doesNotMatch(html, /data-act="toggleLogs"/);
});

test("the closed control states the honest size", () => {
  const steps = [step({ log: "one\ntwo\nthree" })];
  assert.equal(logLineCount(steps), 3);
  const html = renderRunLogPane({ steps, open: false, follow: true, home: HOME });
  assert.match(html, /Show logs -- 3 lines/);
  // Closed means the output is NOT in the document, not merely hidden by CSS.
  assert.doesNotMatch(html, /three/);
});

test("one line is singular, because a product counts properly", () => {
  const html = renderRunLogPane({
    steps: [step({ log: "only" })],
    open: false,
    follow: true,
    home: HOME,
  });
  assert.match(html, /Show logs -- 1 line\b/);
});

test("THE PANE REDACTS -- the home directory and anything shaped like a key", () => {
  // `redactForDisplay` is the one redactor for human surfaces (memql#4194), and
  // a key an install seeded reaches stderr more easily than anyone would like.
  const steps = [
    step({
      state: "failed",
      log: `reading ${HOME}/.memql/key\nsk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA`,
    }),
  ];
  const html = renderRunLogPane({ steps, open: true, follow: true, home: HOME });
  assert.doesNotMatch(html, /\/home\/operator/, "the operator's home is masked");
  assert.doesNotMatch(
    html,
    /sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA/,
    "a credential never reaches the pane",
  );
  assert.match(html, /~/, "the masked form is still readable");
});

test("only steps that said something are drawn", () => {
  // A run has thirteen steps and most say nothing at all; a block each would
  // bury the two that spoke under eleven that did not.
  const html = renderRunLogPane({
    steps: [
      step({ id: "detect", state: "done" }),
      step({ id: "clusterUp", state: "done", log: "argocd ready" }),
      step({ id: "frontDoor", state: "pending" }),
    ],
    open: true,
    follow: true,
    home: HOME,
  });
  assert.match(html, /argocd ready/);
  assert.equal((html.match(/class="log-step"/g) ?? []).length, 1);
});

test("the pane carries the follow flag and the failure anchor for the panel's script", () => {
  const steps = [step({ id: "clusterUp", state: "failed", log: "boom" })];
  const following = renderRunLogPane({ steps, open: true, follow: true, home: HOME });
  assert.match(following, /data-log-pane="true"/);
  assert.match(following, /data-follow="true"/);

  const scrolled = renderRunLogPane({ steps, open: true, follow: false, home: HOME });
  assert.match(scrolled, /data-follow="false"/);

  const anchored = renderRunLogPane({
    steps,
    open: true,
    follow: true,
    focusStepId: "clusterUp",
    home: HOME,
  });
  assert.match(anchored, /data-log-focus="true"/);
});

// --- what the SCREENS do with it ---------------------------------------------

test("the run screen renders no verbatim output until the pane is opened", () => {
  const steps = [step({ state: "running", log: "kubectl: waiting for rollout" })];
  const closed = renderRunningScreen({
    steps,
    mode: "install",
    running: true,
    logsOpen: false,
    logsFollow: true,
  });
  assert.doesNotMatch(closed, /waiting for rollout/, "the log has ONE home and it is the pane");
  assert.match(closed, /Show logs/);

  const open = renderRunningScreen({
    steps,
    mode: "install",
    running: true,
    logsOpen: true,
    logsFollow: true,
  });
  assert.match(open, /waiting for rollout/);
});

test("the failure summary names the step and the reason, never the raw output", () => {
  // D4: the summary renders the step's DESCRIPTION plus the capability's own
  // human sentence plus the remedy. This used to fall back to `failure.log`
  // when no reason was given, which put stderr in the status area -- the one
  // place it must never be.
  const failure = step({
    state: "failed",
    exitCode: 5,
    description: "Creating the cluster and starting MemQL's services in it.",
    reason: "the cluster did not become ready within the time allowed",
    log: "E0814 kubectl.go:214] the server could not find the requested resource",
  });
  const html = renderFailedScreen({
    steps: [failure],
    failures: [failure],
    mode: "install",
    running: false,
    logsOpen: false,
    logsFollow: true,
  });
  assert.match(html, /Creating the cluster and starting MemQL/);
  assert.match(html, /did not become ready within the time allowed/);
  assert.doesNotMatch(html, /kubectl\.go:214/, "raw output stays inside the pane");
});

test("a failure with no reason still keeps its output out of the summary", () => {
  // The fallback that was removed. With no `reason`, the guidance for the exit
  // code stands on its own; the output is one disclosure away.
  const failure = step({
    state: "failed",
    exitCode: 1,
    reason: "",
    log: "Traceback: something internal",
  });
  const html = renderFailedScreen({
    steps: [failure],
    failures: [failure],
    mode: "install",
    running: false,
    logsOpen: false,
    logsFollow: true,
  });
  assert.doesNotMatch(html, /Traceback/);
  assert.match(html, /without classifying what went wrong/);
});

test("the remedy stays OUTSIDE the pane, because it is an action and not a log line", () => {
  const failure = step({
    state: "failed",
    exitCode: 4,
    remedy: "sudo memql install hosts",
    log: "permission denied",
  });
  const html = renderFailedScreen({
    steps: [failure],
    failures: [failure],
    mode: "install",
    running: false,
    logsOpen: true,
    logsFollow: true,
  });
  const remedy = html.indexOf("sudo memql install hosts");
  const pane = html.indexOf(`<div class="disclosure"`);
  assert.notEqual(remedy, -1, "the command that fixes it is on the page");
  assert.ok(remedy < pane, "the remedy is action-adjacent, above the disclosure");
});

test("THE RUN BLOCK NAMES THE FAILED STEP, not whatever is still running", () => {
  // FOUND BY RENDERING THE SCREEN. The block read the narration line first --
  // the description of the steps currently IN FLIGHT -- so a failure in one
  // branch of a wave was announced under the name of a healthy step in another:
  // "Issuing the certificate that lets this cluster answer over https -- and 1
  // more in progress failed -- see the log below." A wave runs under
  // Promise.all and independent branches are deliberately allowed to finish, so
  // the failed step and the running one are routinely different steps.
  const failed = step({
    id: "clusterUp",
    description: "Creating the cluster and starting MemQL's services in it.",
    state: "failed",
    exitCode: 5,
  });
  const stillRunning = step({
    id: "browserTrust",
    description: "Setting up the tools your browsers need to trust local certificates.",
    state: "running",
  });
  const html = renderFailedScreen({
    steps: [failed, stillRunning],
    failures: [failed],
    mode: "install",
    running: false,
    logsOpen: true,
    logsFollow: true,
  });
  assert.match(html, /Creating the cluster and starting MemQL&#39;s services in it failed/);
  assert.doesNotMatch(
    html,
    /browsers need to trust local certificates[^<]*failed/,
    "a healthy step in the same wave is never named as the failure",
  );
});
