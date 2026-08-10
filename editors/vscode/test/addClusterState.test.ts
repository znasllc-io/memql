// The add-a-cluster wizard's state machine.
//
// It is a separate module from the webview for the reason every other state
// module in this extension is: `cmd/memql-lsp/vscodeimportrule_test.go` keeps
// `vscode` out of here, which is what lets the operator's whole path through an
// install be driven under bare `node --test` with no workbench and no cluster.
//
// The load-bearing property is that NO DECISION LIVES HERE either. Step order,
// dependencies, what may overlap, what requires elevation and what an uninstall
// touches are all the graph's and the receipt's. This module holds where the
// operator is, what they have typed, and what the run has reported so far.

import test from "node:test";
import assert from "node:assert/strict";

import { AddClusterState, requiredFields } from "../src/state/addCluster.js";
import type { ExecEvent } from "../src/install/executor.js";
import type { Step } from "../src/install/graph.js";

function step(id: string, description = id): Step {
  return { id, script: "install.binary", description, elevation: "none", verify: { kind: "scriptOk" } };
}

function finished(id: string, over: Record<string, unknown> = {}): ExecEvent {
  return {
    type: "stepFinished",
    step: step(id),
    outcome: {
      id,
      script: "install.binary",
      status: "ok",
      exitCode: 0,
      envelope: null,
      verified: true,
      preExisting: false,
      params: {},
      startedAt: "",
      finishedAt: "",
      ...over,
    },
  } as ExecEvent;
}

// -----------------------------------------------------------------------------
// routing
// -----------------------------------------------------------------------------

test("the page opens on the landing screen", () => {
  assert.equal(new AddClusterState().screen, "landing");
});

test("each action routes to the screen that action actually needs", () => {
  const cases = [
    ["install", "collect"],
    ["installGuided", "collect"],
    ["repair", "collect"],
    ["uninstall", "uninstallPreview"],
    ["connect", "connect"],
  ] as const;
  for (const [action, screen] of cases) {
    const s = new AddClusterState();
    s.chooseAction(action);
    assert.equal(s.screen, screen, `${action} routed to ${s.screen}`);
  }
});

test("guided is remembered as a property of the run, not of the screen", () => {
  // The screen is the same one Automatic uses -- the difference shows up when
  // steps run, where Guided renders the command and waits instead of running
  // it. Carrying it on the state rather than in two screens is what keeps the
  // collect step from being written twice.
  const s = new AddClusterState();
  s.chooseAction("installGuided");
  assert.equal(s.guided, true);

  const auto = new AddClusterState();
  auto.chooseAction("install");
  assert.equal(auto.guided, false);
});

test("back returns to the landing screen and forgets the action", () => {
  const s = new AddClusterState();
  s.chooseAction("uninstall");
  s.back();
  assert.equal(s.screen, "landing");
  assert.equal(s.action, undefined);
});

// -----------------------------------------------------------------------------
// collecting
// -----------------------------------------------------------------------------

test("an install needs everything up front; a repair needs only the domain", () => {
  // Everything is collected before any work starts because a wizard that stops
  // to ask a question nine minutes in is a wizard people abandon. A repair is
  // the same graph re-run over a machine that already has these answers
  // recorded, so demanding them again would be asking for what it can see.
  assert.deepEqual(requiredFields("install"), [
    "domain",
    "ownerFirstName",
    "ownerLastName",
    "ownerEmail",
    "providerKeyFile",
  ]);
  assert.deepEqual(requiredFields("installGuided"), requiredFields("install"));
  assert.deepEqual(requiredFields("repair"), ["domain"]);
  assert.deepEqual(requiredFields("uninstall"), []);
  assert.deepEqual(requiredFields("connect"), []);
});

test("a run cannot begin while a required field is empty", () => {
  const s = new AddClusterState();
  s.chooseAction("install");
  assert.equal(s.beginRun(), false, "an incomplete form must not start an install");
  assert.equal(s.screen, "collect");
  assert.ok(s.errors.some((e) => e.field === "domain"));
});

test("a complete form begins the run", () => {
  const s = new AddClusterState();
  s.chooseAction("install");
  s.setInput("domain", "local.znas.io");
  s.setInput("ownerFirstName", "Ada");
  s.setInput("ownerLastName", "Lovelace");
  s.setInput("ownerEmail", "ada@example.com");
  s.setInput("providerKeyFile", "/home/ada/.anthropic-key");

  assert.equal(s.beginRun(), true);
  assert.equal(s.screen, "running");
  assert.deepEqual(s.errors, []);
});

test("an address that is not an email is refused by name", () => {
  const s = new AddClusterState();
  s.chooseAction("install");
  s.setInput("ownerEmail", "ada-at-example");
  assert.ok(
    s.errors.some((e) => e.field === "ownerEmail" && /email/i.test(e.message)),
    `expected an ownerEmail error, got ${JSON.stringify(s.errors)}`,
  );
});

test("editing a field clears its error without clearing the others", () => {
  const s = new AddClusterState();
  s.chooseAction("install");
  s.beginRun();
  const before = s.errors.length;
  s.setInput("domain", "local.znas.io");
  assert.ok(!s.errors.some((e) => e.field === "domain"));
  assert.equal(s.errors.length, before - 1, "only the corrected field's error may go");
});

// -----------------------------------------------------------------------------
// folding the run
// -----------------------------------------------------------------------------

test("steps appear as the run reports them, in order", () => {
  const s = new AddClusterState();
  s.chooseAction("repair");
  s.setInput("domain", "local.znas.io");
  s.beginRun();

  s.apply({ type: "stepStarted", step: step("binary", "place a tool"), params: {} } as ExecEvent);
  assert.deepEqual(
    s.steps.map((p) => `${p.id}:${p.state}`),
    ["binary:running"],
  );

  s.apply(finished("binary"));
  s.apply({ type: "stepStarted", step: step("cluster"), params: {} } as ExecEvent);
  assert.deepEqual(
    s.steps.map((p) => `${p.id}:${p.state}`),
    ["binary:done", "cluster:running"],
  );
});

test("every executor status reaches the screen, including preserved", () => {
  // Six states, not two. `preserved` in particular cannot be folded into
  // success or failure -- it is the uninstall keeping something the operator
  // already had, and it is the whole two-tier model.
  const s = new AddClusterState();
  s.chooseAction("uninstall");
  for (const [id, status] of [
    ["a", "ok"],
    ["b", "skipped"],
    ["c", "preserved"],
    ["d", "failed"],
  ] as const) {
    s.apply(finished(id, { status }));
  }
  assert.deepEqual(
    s.steps.map((p) => p.state),
    ["done", "skipped", "preserved", "failed"],
  );
});

test("an unknown event does not crash the fold", () => {
  const s = new AddClusterState();
  s.apply({ type: "somethingNewer" } as unknown as ExecEvent);
  assert.deepEqual(s.steps, []);
});

test("a failed step takes the operator to the failure screen with its evidence", () => {
  const s = new AddClusterState();
  s.chooseAction("repair");
  s.setInput("domain", "local.znas.io");
  s.beginRun();
  s.apply(
    finished("cluster", {
      status: "failed",
      exitCode: 4,
      reason: "docker is not running",
    }),
  );

  assert.equal(s.screen, "failedStep");
  assert.equal(s.failed?.id, "cluster");
  assert.equal(s.failed?.exitCode, 4);
  assert.match(s.failed?.reason ?? "", /docker/);
});

test("stderr is kept verbatim for the disclosure", () => {
  const s = new AddClusterState();
  s.chooseAction("repair");
  s.apply({ type: "stepStarted", step: step("cluster"), params: {} } as ExecEvent);
  s.apply({ type: "stepLog", step: step("cluster"), line: "E: no such host" } as ExecEvent);
  s.apply(finished("cluster", { status: "failed", exitCode: 5 }));
  assert.match(s.failed?.log ?? "", /no such host/);
});

// -----------------------------------------------------------------------------
// recovery
// -----------------------------------------------------------------------------

test("retry puts the failed step back in the queue and returns to the run", () => {
  const s = new AddClusterState();
  s.chooseAction("repair");
  s.setInput("domain", "local.znas.io");
  s.beginRun();
  s.apply(finished("cluster", { status: "failed", exitCode: 5 }));

  s.retry();
  assert.equal(s.screen, "running");
  assert.equal(s.steps.find((p) => p.id === "cluster")?.state, "pending");
  assert.equal(s.failed, undefined, "the failure is cleared once it is being retried");
});

test("switching one step to guided marks that step and nothing else", () => {
  // The escape hatch is PER STEP: an operator who wants to run the one command
  // that needs sudo by hand should not be dropped into a fully manual install.
  const s = new AddClusterState();
  s.chooseAction("repair");
  s.setInput("domain", "local.znas.io");
  s.beginRun();
  s.apply({ type: "stepStarted", step: step("binary"), params: {} } as ExecEvent);
  s.apply(finished("binary"));
  s.apply(finished("cluster", { status: "failed", exitCode: 3 }));

  s.switchToGuided();
  assert.equal(s.steps.find((p) => p.id === "cluster")?.guided, true);
  assert.equal(s.steps.find((p) => p.id === "binary")?.guided, false);
  assert.equal(s.guided, false, "one step going guided must not convert the whole run");
});

test("cancelling reaches a terminal screen and says the run was cancelled", () => {
  const s = new AddClusterState();
  s.chooseAction("repair");
  s.setInput("domain", "local.znas.io");
  s.beginRun();
  s.apply(finished("binary"));
  s.cancel();

  assert.equal(s.screen, "done");
  assert.equal(s.cancelled, true);
  // What ran, ran. The receipt records it and the uninstall can take it back;
  // a cancel that erased the progress display would tell the operator less
  // than the machine actually knows.
  assert.deepEqual(
    s.steps.map((p) => `${p.id}:${p.state}`),
    ["binary:done"],
  );
});

test("finishing a run reports whether it succeeded", () => {
  const s = new AddClusterState();
  s.chooseAction("repair");
  s.setInput("domain", "local.znas.io");
  s.beginRun();
  s.finish({ ok: true });
  assert.equal(s.screen, "done");
  assert.equal(s.succeeded, true);
  assert.equal(s.cancelled, false);
});
