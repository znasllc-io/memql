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
  // `ownerEmail` rather than `domain`: the domain carries a default now
  // (#3473), so it is no longer an example of an empty required field. The
  // four fields only a person can supply are.
  assert.ok(s.errors.some((e) => e.field === "ownerEmail"));
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
    token: "eyJhbGciOiJSUzI1NiJ9.e30.sig",
  });
  assert.deepEqual(s.connectDraft(), {
    name: "staging",
    endpoint: "cockpit.staging.example.com:443",
    token: "eyJhbGciOiJSUzI1NiJ9.e30.sig",
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
    token: "eyJhbGciOiJSUzI1NiJ9.e30.sig",
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
    token: "eyJhbGciOiJSUzI1NiJ9.e30.sig",
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
