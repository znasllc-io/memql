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
  return {
    id,
    script: "install.binary",
    description,
    elevation: "none",
    retained: false,
    retainedReason: "",
    shared: false,
    sharedReason: "",
    verify: { kind: "scriptOk" },
  };
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

test("an install needs everything up front; a repair needs what it can get wrong", () => {
  // Everything is collected before any work starts because a wizard that stops
  // to ask a question nine minutes in is a wizard people abandon. A repair is
  // the same graph re-run over a machine that already has these answers
  // recorded -- so it asks for the domain and for the two the RECEIPT can be
  // wrong about (memql#3544).
  //
  // It used to ask for the domain alone, reading the key path off the receipt
  // (memql#3512) so wave 2 could pass. That is right when the recorded path is
  // good and a dead end when it is not: the repair re-runs with the same bad
  // value, fails at the same step, and offers no box to fix it. The panel
  // pre-fills both from the receipt, so the common case is still no typing --
  // what changed is that the value is now reachable.
  assert.deepEqual(requiredFields("install"), [
    "domain",
    "ownerFirstName",
    "ownerLastName",
    "ownerEmail",
    "provider",
    "providerKeyFile",
  ]);
  assert.deepEqual(requiredFields("installGuided"), requiredFields("install"));
  assert.deepEqual(requiredFields("repair"), ["domain", "provider", "providerKeyFile"]);
  assert.deepEqual(requiredFields("uninstall"), []);
  assert.deepEqual(requiredFields("connect"), []);
});

test("the provider is one of the fields collected, and it is pre-answered", () => {
  // It used to be neither. `provider` was hardcoded in the panel AND pinned in
  // install.json, where graph params win -- so an operator holding an OpenAI
  // key had no route through this wizard, though verify-provider-key.sh
  // supports one. Every test enumerated the other five fields, so "collected on
  // one pass" read as satisfied at a glance.
  assert.ok(requiredFields("install").includes("provider"));
  assert.equal(
    new AddClusterState().inputs.provider,
    "anthropic",
    "a choice from a closed set gets a default; the four personal fields do not",
  );
});

test("a provider memQL cannot verify is refused here, in the operator's terms", () => {
  // The second wall. The control is a select, so the wrong answer is not
  // expressible by clicking -- but the postMessage channel is untrusted, and
  // the alternative to refusing here is exit 2 out of the script, whose
  // guidance correctly says "a fault in memQL rather than in your machine or
  // your answers". That is the wrong sentence about a value the operator chose.
  const s = new AddClusterState();
  s.chooseAction("install");
  s.setInput("provider", "gemini");
  assert.match(s.errors.find((e) => e.field === "provider")?.message ?? "", /anthropic or openai/);

  s.setInput("provider", "openai");
  assert.deepEqual(s.errors, [], "a provider the script does support is accepted");
});

test("a run cannot begin while a required field is empty", () => {
  const s = new AddClusterState();
  s.chooseAction("install");
  assert.equal(s.beginRun(), false, "an incomplete form must not start an install");
  assert.equal(s.screen, "collect");
  // `ownerEmail` rather than `domain`: the domain carries a default now
  // (#3473), so it is no longer an example of an empty required field. The
  // four fields only a person can supply are.
  assert.ok(s.errors.some((e) => e.field === "ownerEmail"));
});

test("a complete form begins the run", () => {
  const s = new AddClusterState();
  s.chooseAction("install");
  s.setInput("domain", "memql.localhost");
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
  // Corrects `ownerEmail` rather than `domain` -- the domain is pre-filled
  // (#3473) and so has no error to clear.
  s.setInput("ownerEmail", "ada@example.com");
  assert.ok(!s.errors.some((e) => e.field === "ownerEmail"));
  assert.equal(s.errors.length, before - 1, "only the corrected field's error may go");
});

// -----------------------------------------------------------------------------
// folding the run
// -----------------------------------------------------------------------------

test("steps appear as the run reports them, in order", () => {
  const s = new AddClusterState();
  s.chooseAction("repair");
  s.setInput("domain", "memql.localhost");
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

test("the plan is on screen before anything runs, every step pending", () => {
  // `pending` was UNREACHABLE in a forward run: a step first appeared when it
  // STARTED, so the checklist grew from empty and never said how much was left.
  // All six states rendered correctly and all six were unit-tested by feeding
  // them in directly; none of that made one visible ahead of its turn.
  const s = new AddClusterState();
  s.chooseAction("install");
  s.beginRun();
  s.apply({
    type: "runStarted",
    steps: [
      { id: "detect", description: "look at the machine" },
      { id: "binary", description: "place a tool" },
      { id: "cluster", description: "create the cluster" },
    ],
  } as ExecEvent);

  assert.deepEqual(
    s.steps.map((p) => [p.id, p.state]),
    [
      ["detect", "pending"],
      ["binary", "pending"],
      ["cluster", "pending"],
    ],
    "every step in the graph is visible, in graph order, before the first one starts",
  );

  s.apply({ type: "stepStarted", step: step("detect"), params: {} } as ExecEvent);
  assert.equal(s.steps.find((p) => p.id === "cluster")?.state, "pending", "the steps ahead stay ahead");
});

test("a re-run keeps what the previous attempt established", () => {
  // Retry re-runs the WHOLE graph, so `runStarted` arrives a second time. If it
  // reset the list, every step the operator had watched succeed would blink back
  // to pending -- a display of the event rather than of the machine.
  const s = new AddClusterState();
  s.chooseAction("install");
  s.beginRun();
  const plan = {
    type: "runStarted",
    steps: [
      { id: "binary", description: "place a tool" },
      { id: "cluster", description: "create the cluster" },
    ],
  } as ExecEvent;

  s.apply(plan);
  s.apply(finished("binary"));
  s.apply(finished("cluster", { status: "failed", exitCode: 5 }));
  s.retry();
  s.apply(plan);

  assert.equal(s.steps.find((p) => p.id === "binary")?.state, "done");
  assert.equal(s.steps.find((p) => p.id === "cluster")?.state, "pending");
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
  s.setInput("domain", "memql.localhost");
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
  s.setInput("domain", "memql.localhost");
  s.beginRun();
  s.apply(finished("cluster", { status: "failed", exitCode: 5 }));

  s.retry();
  assert.equal(s.screen, "running");
  assert.equal(s.steps.find((p) => p.id === "cluster")?.state, "pending");
  assert.equal(s.failed, undefined, "the failure is cleared once it is being retried");
});

test("a wave with two failures reports both, and leads with the first", () => {
  // The executor runs a wave under Promise.all and deliberately lets
  // independent branches finish, so several steps can fail in one wave. The
  // headline used to be whichever resolved LAST -- a scheduling accident -- and
  // the guidance shown was that one step's, out of N, though the exit codes ask
  // for genuinely different things.
  const s = new AddClusterState();
  s.chooseAction("install");
  s.beginRun();
  s.apply(finished("binary", { status: "failed", exitCode: 4 }));
  s.apply(finished("hosts", { status: "failed", exitCode: 3 }));

  assert.deepEqual(
    s.failures.map((f) => [f.id, f.exitCode]),
    [
      ["binary", 4],
      ["hosts", 3],
    ],
    "every failure is available to render, with its own exit code",
  );
  assert.equal(
    s.failed?.id,
    "binary",
    "the page leads with the EARLIEST failure -- the others may be consequences of it",
  );
});

test("retry puts every failed step back, not only the one being led with", () => {
  // The retry re-runs the whole graph, so a failure left marked `failed` would
  // be a stale verdict about a step being attempted again in front of the
  // operator.
  const s = new AddClusterState();
  s.chooseAction("install");
  s.beginRun();
  s.apply(finished("binary", { status: "failed", exitCode: 4 }));
  s.apply(finished("hosts", { status: "failed", exitCode: 3 }));

  s.retry();
  assert.deepEqual(s.failures, [], "no step may still read as failed once it is being retried");
  assert.equal(s.steps.find((p) => p.id === "binary")?.state, "pending");
  assert.equal(s.steps.find((p) => p.id === "hosts")?.state, "pending");
});

test("retry clears the previous attempt's output", () => {
  // `apply()` APPENDS each stepLog line, so a retry that kept the old log would
  // render both attempts concatenated in one disclosure with no boundary --
  // and the failure the operator reads would be the one that is no longer
  // happening. Every other trace of the last attempt is dropped; the log was
  // the one that was not.
  const s = new AddClusterState();
  s.chooseAction("repair");
  s.beginRun();
  s.apply({ type: "stepStarted", step: step("cluster"), params: {} } as ExecEvent);
  s.apply({ type: "stepLog", step: step("cluster"), line: "E: first attempt" } as ExecEvent);
  s.apply(finished("cluster", { status: "failed", exitCode: 5 }));

  s.retry();
  assert.equal(s.steps.find((p) => p.id === "cluster")?.log, "");

  s.apply({ type: "stepLog", step: step("cluster"), line: "E: second attempt" } as ExecEvent);
  const log = s.steps.find((p) => p.id === "cluster")?.log ?? "";
  assert.match(log, /second attempt/);
  assert.ok(!log.includes("first attempt"), "the retried step still carries the old output");
});

test("switching to guided clears the previous attempt's output too", () => {
  // Same reasoning as retry: the step is about to run again.
  const s = new AddClusterState();
  s.chooseAction("repair");
  s.beginRun();
  s.apply({ type: "stepStarted", step: step("cluster"), params: {} } as ExecEvent);
  s.apply({ type: "stepLog", step: step("cluster"), line: "E: needs sudo" } as ExecEvent);
  s.apply(finished("cluster", { status: "failed", exitCode: 3 }));

  s.switchToGuided();
  assert.equal(s.steps.find((p) => p.id === "cluster")?.log, "");
});

test("switching one step to guided marks that step and nothing else", () => {
  // The escape hatch is PER STEP: an operator who wants to run the one command
  // that needs sudo by hand should not be dropped into a fully manual install.
  const s = new AddClusterState();
  s.chooseAction("repair");
  s.setInput("domain", "memql.localhost");
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
  s.setInput("domain", "memql.localhost");
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
  s.setInput("domain", "memql.localhost");
  s.beginRun();
  s.finish({ ok: true });
  assert.equal(s.screen, "done");
  assert.equal(s.succeeded, true);
  assert.equal(s.cancelled, false);
});

test("a CANCELLED run does not count as a success, even though ok is true", () => {
  // THE GATE THE HAND-OFF HANGS OFF (#3477). `ok` means nothing FAILED, and a
  // cancelled run normally satisfies that -- `executor.ts` sets `cancelled`
  // separately for exactly this reason. Reading `ok` alone would have the
  // wizard register the cluster and offer "Sign in as owner" for an install
  // the operator deliberately stopped: worse than doing nothing, because it
  // looks like success.
  const s = new AddClusterState();
  s.chooseAction("repair");
  s.beginRun();
  s.finish({ ok: true, cancelled: true });

  assert.equal(s.screen, "done");
  assert.equal(s.cancelled, true);
  assert.equal(s.succeeded, false, "a cancelled run must never report success");
});

test("a failed run is neither successful nor cancelled", () => {
  const s = new AddClusterState();
  s.chooseAction("repair");
  s.beginRun();
  s.finish({ ok: false });
  assert.equal(s.succeeded, false);
  assert.equal(s.cancelled, false);
});

// -----------------------------------------------------------------------------
// registering an existing cluster (memql#3475)
//
// What these cover is the property the old input-box sequence could not have:
// the form is a single object the operator revises. A sequence of prompts had
// to validate one answer at a time, could not be walked backwards, and threw
// everything away on Escape -- so "all the problems at once", "revise after a
// refusal" and "discard writes nothing" are the cases, not extras.
// -----------------------------------------------------------------------------

import * as fs from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";

import { addCluster, readClustersFile } from "../src/clusters/file.js";
import type { ClustersFile } from "../src/clusters/model.js";
import { DEFAULT_LOCAL_DOMAIN } from "../src/install/stackPin.js";

// A stand-in for the pasted access token, deliberately NOT shaped like a JWT.
//
// The field really does hold one in production, and an earlier version of these
// fixtures used a structurally valid but empty JWT for that realism. It tripped
// gitleaks, which reads a `header.payload.signature` triple next to the word
// `token` as a credential and cannot know the payload is `{}`. Realism bought
// nothing here: `validateConnect` only refuses a `mql_pat_` prefix and embedded
// whitespace, so nothing in the code under test parses this value. A fixture
// that trains the secret scanner to expect false positives in this file is a
// worse trade than a fixture that looks less like the real thing.
const FAKE_TOKEN = "not-a-real-access-token";

function registry(...names: string[]): ClustersFile {
  return {
    clusters: names.map((name) => ({ name, endpoint: `cockpit.${name}.example.com:443` })),
    selectedCluster: "",
  };
}

/** A state parked on the connect screen with the whole form filled in. */
function connectForm(values: Partial<Record<"name" | "domain" | "endpoint" | "token", string>>) {
  const s = new AddClusterState();
  s.chooseAction("connect");
  for (const [field, text] of Object.entries(values)) {
    s.setConnectInput(field as "name" | "domain" | "endpoint" | "token", text);
  }
  return s;
}

function messageFor(s: AddClusterState, field: string): string | undefined {
  return s.connectErrors.find((e) => e.field === field)?.message;
}

test("a complete form produces the entry to write", () => {
  const s = connectForm({
    name: "staging",
    endpoint: "cockpit.staging.example.com:443",
    token: FAKE_TOKEN,
  });
  assert.deepEqual(s.connectDraft(), {
    name: "staging",
    endpoint: "cockpit.staging.example.com:443",
    token: FAKE_TOKEN,
  });
});

test("a registered cluster carries NO local key", () => {
  // `local: true` means "this cluster's data is disposable" -- it disables the
  // mutation confirmation and, since memql#3466, decides whether the tree row
  // offers an uninstall. Nothing about registering someone else's cluster
  // knows that, so the field is absent rather than false: absent is what the
  // reader means by "not local", and `local: false` is a key the Cockpit drops
  // on its next write anyway.
  const draft = connectForm({ name: "prod", endpoint: "cockpit.prod.example.com:443" }).connectDraft();
  if (draft === undefined) throw new assert.AssertionError({ message: "the form was valid" });
  assert.equal("local" in draft, false);
});

test("an empty optional field is omitted from the entry, not written as a clear", () => {
  // "" and absent are the same file for a NEW entry but opposite instructions
  // to upsertCluster, where "" DELETES the key.
  const draft = connectForm({ name: "prod", endpoint: "cockpit.prod.example.com:443" }).connectDraft();
  assert.deepEqual(draft, { name: "prod", endpoint: "cockpit.prod.example.com:443" });
});

test("the endpoint is composed from the domain when the box is left empty", () => {
  const draft = connectForm({ name: "staging", domain: " staging.example.com. " }).connectDraft();
  assert.deepEqual(draft, {
    name: "staging",
    domain: "staging.example.com",
    endpoint: "cockpit.staging.example.com:443",
  });
});

test("a typed endpoint wins over the one the domain would compose", () => {
  const draft = connectForm({
    name: "staging",
    domain: "staging.example.com",
    endpoint: "10.0.0.5:50051",
  }).connectDraft();
  assert.equal(draft?.endpoint, "10.0.0.5:50051");
});

// --- per-field validation ----------------------------------------------------

test("a missing name is refused", () => {
  const s = connectForm({ name: "   ", endpoint: "cockpit.x.example.com:443" });
  assert.equal(s.connectDraft(), undefined);
  assert.equal(messageFor(s, "name"), "A cluster name is required.");
});

test("a name already in the registry is refused, and the message names the conflict", () => {
  const s = connectForm({ name: "staging", endpoint: "cockpit.staging.example.com:443" });
  s.setRegistry(registry("local", "staging"));
  assert.equal(s.connectDraft(), undefined);
  assert.equal(
    messageFor(s, "name"),
    'a cluster named "staging" already exists; edit it instead of adding it again',
  );
});

test("the duplicate check needs a registry, and without one the write-time wall is the only one", () => {
  // clusters.yaml is shared with the Cockpit, so no read here stays
  // authoritative -- which is why this check never became the only one. A
  // clusters.yaml that would not parse simply leaves it silent.
  const s = connectForm({ name: "staging", endpoint: "cockpit.staging.example.com:443" });
  assert.notEqual(s.connectDraft(), undefined);
});

test("an endpoint that names nothing at all is refused", () => {
  const s = connectForm({ name: "staging" });
  assert.equal(s.connectDraft(), undefined);
  assert.match(messageFor(s, "endpoint") ?? "", /An endpoint is required/);
});

test("the endpoint is judged by the dialer, and reports what the dialer said", () => {
  // webSocketUrlFor is the function the connection layer actually calls, so
  // its refusal is the field error -- minus the "cluster \"x\": " prefix it
  // carries for callers with no field to attach a sentence to.
  const s = connectForm({ name: "staging", endpoint: "https://cockpit.staging.example.com" });
  assert.equal(s.connectDraft(), undefined);
  assert.equal(
    messageFor(s, "endpoint"),
    'endpoint scheme must be ws:// or wss://, got "https://" -- store the gRPC host:port (or an explicit ws(s):// bridge URL), not a general-purpose URL',
  );
});

test("a PAT in the token box is refused by name rather than left to fail at the handshake", () => {
  const s = connectForm({
    name: "staging",
    endpoint: "cockpit.staging.example.com:443",
    token: "mql_pat_abcdef",
  });
  assert.equal(s.connectDraft(), undefined);
  assert.match(messageFor(s, "token") ?? "", /Personal Access Token/);
});

test("a token that picked up a line break is refused", () => {
  const s = connectForm({
    name: "staging",
    endpoint: "cockpit.staging.example.com:443",
    token: "eyJhbGciOi\nJSUzI1NiJ9",
  });
  assert.equal(s.connectDraft(), undefined);
  assert.match(messageFor(s, "token") ?? "", /whitespace/);
});

test("a domain given as a URL is refused, and so is one with a space in it", () => {
  const scheme = connectForm({ name: "s", domain: "https://staging.example.com" });
  assert.equal(scheme.connectDraft(), undefined);
  assert.match(messageFor(scheme, "domain") ?? "", /drop the scheme/);

  const spaced = connectForm({ name: "s", domain: "staging example.com" });
  assert.equal(spaced.connectDraft(), undefined);
  assert.equal(messageFor(spaced, "domain"), "A domain cannot contain spaces.");
});

test("every field is checked, so all the problems arrive at once", () => {
  // The sequence of input boxes this replaces could only ever report the box
  // in front of the operator: four wrong answers meant four separate attempts.
  const s = connectForm({
    name: "staging",
    domain: "https://staging.example.com",
    endpoint: "https://cockpit.staging.example.com",
    token: "mql_pat_abcdef",
  });
  s.setRegistry(registry("staging"));
  assert.equal(s.connectDraft(), undefined);
  assert.deepEqual(
    s.connectErrors.map((e) => e.field).sort(),
    ["domain", "endpoint", "name", "token"],
  );
});

// --- revision ----------------------------------------------------------------

test("a field can be revised after it was refused, and the rest of the form survives", () => {
  const s = connectForm({
    name: "staging",
    endpoint: "https://cockpit.staging.example.com",
    token: FAKE_TOKEN,
  });
  s.setRegistry(registry("staging"));
  assert.equal(s.connectDraft(), undefined);

  // Fix ONLY the two that were wrong. Everything else is still there -- which
  // is the whole point of a form over a sequence of prompts.
  s.setConnectInput("name", "staging-2");
  s.setConnectInput("endpoint", "cockpit.staging.example.com:443");
  assert.deepEqual(s.connectDraft(), {
    name: "staging-2",
    endpoint: "cockpit.staging.example.com:443",
    token: FAKE_TOKEN,
  });
});

test("editing a field drops the complaint that field was carrying", () => {
  const s = connectForm({ name: "", endpoint: "cockpit.x.example.com:443" });
  assert.equal(s.connectDraft(), undefined);
  s.setConnectInput("name", "x");
  assert.equal(messageFor(s, "name"), undefined);
});

test("going back to the cards clears the complaints but keeps what was typed", () => {
  const s = connectForm({ name: "", endpoint: "cockpit.x.example.com:443" });
  s.connectDraft();
  s.back();
  assert.equal(s.screen, "landing");
  assert.deepEqual(s.connectErrors, []);
  assert.equal(s.connectInputs.endpoint, "cockpit.x.example.com:443");
});

// --- discarding ---------------------------------------------------------------

test("discarding empties the form and returns to the cards", () => {
  const s = connectForm({ name: "staging", endpoint: "cockpit.staging.example.com:443" });
  s.discardConnect();
  assert.equal(s.screen, "landing");
  assert.deepEqual(s.connectInputs, { name: "", domain: "", endpoint: "", token: "" });
});

test("a write refused after the fact is reported without losing the form", () => {
  const s = connectForm({ name: "staging", endpoint: "cockpit.staging.example.com:443" });
  s.failConnect('a cluster named "staging" already exists; edit it instead of adding it again');
  assert.match(s.connectFailure, /already exists/);
  assert.equal(s.connectInputs.name, "staging");
  // ...and it goes away on the next attempt rather than lingering over a form
  // that has since been corrected.
  s.setConnectInput("name", "staging-2");
  s.connectDraft();
  assert.equal(s.connectFailure, "");
});

// --- what actually lands on disk ----------------------------------------------

test("saving writes the entry, and the written YAML carries no local key", async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-connect-"));
  const file = path.join(dir, "clusters.yaml");

  const draft = connectForm({
    name: "staging",
    domain: "staging.example.com",
    endpoint: "cockpit.staging.example.com:443",
  }).connectDraft();
  if (draft === undefined) throw new assert.AssertionError({ message: "the form was valid" });
  await addCluster(file, draft);

  const written = await readClustersFile(file);
  assert.deepEqual(written.clusters, [
    { name: "staging", domain: "staging.example.com", endpoint: "cockpit.staging.example.com:443" },
  ]);
  // Not merely absent from the parse -- absent from the bytes, since `local`
  // is what keeps the tree row `memqlCluster` rather than `memqlLocalCluster`.
  assert.equal((await fs.readFile(file, "utf8")).includes("local"), false);
});

test("discarding writes nothing, not even a partial entry", async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-connect-"));
  const file = path.join(dir, "clusters.yaml");

  const s = connectForm({ name: "staging" });
  s.discardConnect();
  // Nothing writes except an explicit save, and there is nothing left to save.
  assert.equal(s.connectDraft(), undefined);
  await assert.rejects(() => fs.stat(file));
});

test("the second wall still refuses a name the first one could not see", async () => {
  // The registry snapshot is only true of the moment it was read, so the
  // duplicate that arrives between the read and the write has to be caught by
  // addCluster -- and it is caught with the same sentence the form uses.
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "memql-connect-"));
  const file = path.join(dir, "clusters.yaml");
  await addCluster(file, { name: "staging", endpoint: "cockpit.staging.example.com:443" });

  const draft = connectForm({
    name: "staging",
    endpoint: "cockpit.staging.example.com:443",
  }).connectDraft();
  if (draft === undefined) throw new assert.AssertionError({ message: "no registry, no conflict" });
  await assert.rejects(
    () => addCluster(file, draft),
    /a cluster named "staging" already exists; edit it instead of adding it again/,
  );
});

// -----------------------------------------------------------------------------
// the domain is validated, not constrained (memql#3590, memql#3593)
// -----------------------------------------------------------------------------
//
// The field is editable, and until memql#3590 anything typed into it was
// accepted. It reached `seedBootstrap` (which bootstrapped identity for it)
// while the hosts block, the certificate and the front-door probe used their own
// defaults -- and underneath all three, the release's local overlay pinned its
// Ingress hosts and identity issuer to one domain as well. So a custom domain
// could not work no matter how carefully the installer threaded it, and the
// operator learned that at `frontDoor` as a failure against hostnames they had
// typed themselves.
//
// memql#3593 removed the constraint rather than the field: the overlay is
// parameterised, so ANY well-formed domain now reaches the cluster. What is
// checked here is only what cannot become a hostname at all. Whether a domain
// RESOLVES is not knowable in a form -- `hostsBlock` probes it and `frontDoor`
// verifies the whole path.

test("an answer that is not a hostname is refused before anything runs", () => {
  const s = new AddClusterState();
  s.chooseAction("install");
  s.setInput("domain", "https://memql.localhost");

  const message = s.errors.find((e) => e.field === "domain")?.message ?? "";
  assert.notEqual(message, "", "a pasted URL was accepted as a domain");
  // `includes`, not a regex built from the hostname. Escaping dots by hand is
  // incomplete escaping (CodeQL is right: it leaves backslashes alone), and a
  // hostname pattern with an unescaped `.` matches more hosts than intended --
  // neither of which a substring check can get wrong.
  assert.ok(
    message.includes("URL"),
    `the refusal must say what is wrong with the answer: ${message}`,
  );
});

test("the operator's own domain is accepted", () => {
  const s = new AddClusterState();
  s.chooseAction("install");

  for (const domain of [DEFAULT_LOCAL_DOMAIN, "lab.example.com", "sub.memql.localhost"]) {
    s.setInput("domain", domain);
    assert.deepEqual(
      s.errors.filter((e) => e.field === "domain"),
      [],
      `${domain} must be accepted -- the overlay is parameterised now`,
    );
  }
});

test("a run cannot begin on an answer that is not a hostname", () => {
  const s = new AddClusterState();
  s.chooseAction("install");
  s.setInput("domain", "memql.localhost:443");
  s.setInput("ownerFirstName", "Ada");
  s.setInput("ownerLastName", "Lovelace");
  s.setInput("ownerEmail", "ada@example.com");
  s.setInput("providerKeyFile", "/home/ada/.anthropic-key");

  assert.equal(
    s.beginRun(),
    false,
    "an install that cannot produce a working front door must not spend ten minutes proving it",
  );
  assert.equal(s.screen, "collect");
});
