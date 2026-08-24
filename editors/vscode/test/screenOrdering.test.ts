// Actions first, on every screen, enforced rather than remembered (memql#4453).
//
// WHAT WAS WRONG. Every screen in both panels rendered its buttons LAST --
// heading, lede, facts or a thirteen-row step checklist, and then, below the
// fold, the thing the operator opened the page to do. Nothing about that was a
// decision; it was accretion, one screen at a time, each appending its
// `<div class="actions">` to the end of a template literal. Which is exactly
// why a convention would not have held: nobody chose the old order either.
//
// SO THE ORDER IS A FUNCTION, AND THIS ASSERTS THE FUNCTION IS USED. Two
// halves, because the screens live on two sides of a line this lane cannot
// cross:
//
//   1. RENDERED. The pure screens -- installScreens.ts and
//      deploymentScreens.ts -- are rendered here and their output inspected.
//   2. SCANNED. The wizard's landing / connect / uninstall / done screens are
//      methods on a panel that imports `vscode`, which
//      cmd/memql-lsp/vscodeimportrule_test.go keeps out of this lane by
//      design. They cannot be rendered, so what is asserted instead is that
//      NO screen anywhere builds its own heading-led page: `renderScreen` is
//      the only producer of an `<h1>`, so a screen that reaches the operator
//      at all reached them through the doctrine.
//
// The scanned half is the weaker claim and is worth being clear about: it
// proves composition, not pixels. What makes it enough is that composition is
// the only way the order can be got wrong once there is one function -- a
// screen either calls it or has no heading.
//
// Refs: #4453 #4452

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import { PLATFORM_DETECT_STEP } from "../src/install/platform.js";
import type { StepProgress } from "../src/state/addCluster.js";
import { newLocalRun, type Instance, type Run } from "../src/state/deployments.js";
import {
  renderCollectScreen,
  renderFailedScreen,
  renderRebuildScreen,
  renderRunningScreen,
} from "../src/webview/installScreens.js";
import {
  renderChooseTag,
  renderInstanceOverview,
  renderRemoteInstance,
  renderRunDetail,
} from "../src/webview/deploymentScreens.js";
import { DEFAULT_INPUTS } from "../src/state/addCluster.js";

// dist-test/test/<name>.js -> the package root, then the repository.
const PKG = path.resolve(__dirname, "..", "..");
const REPO = path.resolve(PKG, "..", "..");

function step(over: Partial<StepProgress> = {}): StepProgress {
  return {
    id: "toolK3d",
    description: "Installing k3d",
    state: "pending",
    reason: "",
    exitCode: null,
    log: "",
    guided: false,
    remedy: "",
    ...over,
  };
}

const LOCAL: Instance = {
  name: "local",
  kind: "local",
  presence: "installed-healthy",
  connected: true,
  version: "v0.19.1",
};

const REMOTE: Instance = { ...LOCAL, name: "staging", kind: "remote", version: "v0.9.2" };

const RUN: Run = newLocalRun({
  id: "run-1",
  instance: "local",
  kind: "upgrade",
  startedAt: "2026-08-14T10:00:00Z",
});

/**
 * The doctrine, as an assertion.
 *
 * A screen's actions row must be the FIRST thing after the heading. Not merely
 * "before the status block" -- that would pass for a row sitting anywhere in
 * the top half, and the owner's ask was that the buttons are the first thing on
 * the page. A screen with no actions at all passes trivially and is listed as
 * such below, which is the exemption the design record calls for.
 */
function assertActionsFirst(name: string, html: string): void {
  assert.match(
    html,
    /^<h1>[\s\S]*?<\/h1>/,
    `${name}: a screen begins with its heading, from renderScreen`,
  );
  const afterHeading = html.slice(html.indexOf("</h1>") + "</h1>".length).trimStart();
  if (!html.includes(`<div class="actions">`)) return;
  assert.ok(
    afterHeading.startsWith(`<div class="actions">`),
    `${name}: the actions row must be the first thing under the heading, so it is ` +
      `visible without scrolling. Got:\n${afterHeading.slice(0, 160)}`,
  );
}

/** And the disclosure is last, when the screen carries one. */
function assertLogsLast(name: string, html: string): void {
  const disclosure = html.indexOf(`<div class="disclosure"`);
  if (disclosure === -1) return;
  const tail = html.slice(disclosure);
  assert.doesNotMatch(
    tail,
    /<h2|class="facts"|class="run-block"/,
    `${name}: nothing renders after the log/diagnostics disclosure`,
  );
}

// EVERY SCREEN THIS LANE CAN RENDER, with the exemptions stated inline rather
// than in a comment somewhere else. There is exactly one today: a screen that
// offers no actions has no row to hoist, and the assertion above passes it
// without needing to know that.
const SCREENS: readonly { name: string; html: string }[] = [
  {
    name: "collect",
    html: renderCollectScreen({ action: "install", values: DEFAULT_INPUTS, errors: [] }),
  },
  {
    name: "rebuild",
    html: renderRebuildScreen({ checkoutDir: "/home/me/src", nodes: "" }),
  },
  {
    name: "running",
    html: renderRunningScreen({
      steps: [step({ state: "running" })],
      mode: "install",
      running: true,
      logsOpen: false,
      logsFollow: true,
    }),
  },
  {
    name: "running (settled)",
    html: renderRunningScreen({
      steps: [step({ state: "done" })],
      mode: "install",
      running: false,
      logsOpen: false,
      logsFollow: true,
    }),
  },
  {
    name: "failure",
    html: renderFailedScreen({
      steps: [step({ state: "failed", exitCode: 5, log: "boom" })],
      failures: [step({ state: "failed", exitCode: 5, log: "boom" })],
      mode: "install",
      running: false,
      logsOpen: true,
      logsFollow: true,
    }),
  },
  {
    name: "instance overview",
    html: renderInstanceOverview({
      instance: LOCAL,
      runs: [],
      actions: [{ id: "repair", label: "Repair", detail: "", flow: "repairGraph" }],
      nowMs: 0,
      error: "",
      releases: undefined,
      upgrade: { kind: "none", reason: "not under test" },
      diagnosticsOpen: false,
    }),
  },
  {
    name: "remote instance",
    html: renderRemoteInstance({
      instance: REMOTE,
      runs: [],
      pipeline: { kind: "present", title: "Deploy", detail: "", actions: [] },
      nowMs: 0,
      outcome: "",
      error: "",
      releases: undefined,
      upgrade: { kind: "none", reason: "not under test" },
      diagnosticsOpen: false,
    }),
  },
  {
    name: "choose tag",
    html: renderChooseTag({
      instance: LOCAL,
      listing: { tags: ["v0.19.1"], error: "" },
      target: "",
      tagError: "",
      plan: [],
      summary: "",
      sameVersion: false,
    }),
  },
  {
    name: "run detail",
    html: renderRunDetail({
      instance: LOCAL,
      run: RUN,
      actions: [],
      nowMs: 0,
      outcome: "",
      error: "",
      diagnosticsOpen: false,
    }),
  },
];

test("every rendered screen puts its actions row first", () => {
  for (const screen of SCREENS) assertActionsFirst(screen.name, screen.html);
});

test("nothing renders after a screen's log or diagnostics disclosure", () => {
  for (const screen of SCREENS) assertLogsLast(screen.name, screen.html);
});

test("the failure screen is not an exception -- Retry is at the top", () => {
  // The screen most likely to be argued into one. An operator reading a failure
  // is an operator deciding what to do next, and the summary is the input to
  // that decision rather than a queue in front of it.
  const failure = step({ state: "failed", exitCode: 5, reason: "the port was busy" });
  const html = renderFailedScreen({
    steps: [failure],
    failures: [failure],
    mode: "install",
    running: false,
    logsOpen: true,
    logsFollow: true,
  });
  assert.ok(
    html.indexOf(`data-act="retry"`) < html.indexOf("the port was busy"),
    "Retry must precede the failure summary",
  );
});

test("the ordering never changes WHICH actions a screen offers", () => {
  // The doctrine moves buttons; `instanceActions` decides which exist. A reader
  // -- no actions offered -- must still see no actions, which is the property
  // that would break if the layout ever synthesised a default row.
  const html = renderInstanceOverview({
    instance: LOCAL,
    runs: [],
    actions: [],
    nowMs: 0,
    error: "",
    releases: undefined,
    upgrade: { kind: "none", reason: "not under test" },
    diagnosticsOpen: false,
  });
  assert.doesNotMatch(html, /<div class="actions">/);
  assert.doesNotMatch(html, /data-choose=/);
});

test("no screen builds its own heading-led page -- renderScreen is the only producer", () => {
  // THE HALF THIS LANE CANNOT RENDER. The wizard's landing, connect, uninstall
  // and done screens are methods on a panel that imports `vscode`. What is
  // asserted is composition: a template literal that opens with `<h1>` is a
  // screen laying itself out, which is the thing memql#4453 replaced.
  const files = [
    path.join("src", "webview", "addClusterPanel.ts"),
    path.join("src", "webview", "deploymentPanel.ts"),
    path.join("src", "webview", "installScreens.ts"),
    path.join("src", "webview", "deploymentScreens.ts"),
  ];
  for (const file of files) {
    const text = fs.readFileSync(path.join(PKG, file), "utf8");
    assert.equal(
      text.includes("`<h1>"),
      false,
      `${file} builds a screen's page itself instead of composing through ` +
        `renderScreen(); the actions-first order cannot be enforced on a page ` +
        `that lays itself out (memql#4453)`,
    );
  }
});

test("the detect step's second copy still says what the install document says", () => {
  // FOUND BY THE memql#4456 SWEEP, not by review. `PLATFORM_DETECT_STEP` is a
  // hand-written copy of one step used when the wizard refuses before a run
  // starts -- so a content rewrite of the document leaves it reading in the old
  // voice, on precisely the path an operator hits when something is wrong.
  const doc = JSON.parse(
    fs.readFileSync(path.join(REPO, "scripts", "install", "graph", "install.json"), "utf8"),
  ) as { steps: { id: string; description: string }[] };
  const detect = doc.steps.find((s) => s.id === "detect");
  assert.notEqual(detect, undefined, "install.json still has a detect step");
  assert.equal(
    PLATFORM_DETECT_STEP.description,
    detect?.description,
    "src/install/platform.ts holds a second copy of the detect step's sentence; " +
      "it must say what scripts/install/graph/install.json says",
  );
});
