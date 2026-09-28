// Installing a MemQL cluster involves no AI provider key, because there is no
// such thing (epic memql#4440, task memql#4441; widened by epic memql#5088).
//
// WHAT THE CLAIM USED TO BE, AND WHAT IT IS NOW. memql#4440's sentence was "no
// lifecycle verb REQUIRES a vendor credential" -- the fields stayed, demoted to
// a collapsed disclosure, for the operator who happened to have a key.
// memql#5088's sentence is stronger and shorter: no lifecycle verb COLLECTS,
// CARRIES OR PASSES one, because both cloud vendors are now reached by workload
// identity federation and no vendor API key exists anywhere in the product.
//
// AND THIS WIZARD CANNOT OFFER FEDERATION EITHER, which is why the end state is
// "no AI credential at all" rather than "a different AI credential". Federation
// works by having the vendor verify a token against the cluster's OIDC issuer.
// A k3d cluster's issuer is not publicly reachable, so nothing a local cluster
// mints can be verified by anyone. An ids form here would collect answers that
// could never work (design D6).
//
// WHY THIS FILE EXISTS RATHER THAN A FEW LINES IN THE NEIGHBOURING SUITES.
// The claim is a single sentence spread across four modules that otherwise have
// nothing to do with each other: the required-field tables, the validator, the
// install plan, and the graph executor's skip semantics. Split across four
// suites it reads as four unrelated assertions, and the one that actually
// protects an operator reads as a test about dependency edges.
//
// THE LOAD-BEARING TEST IS `a keyless install still runs every mutating step`.
// The others would all pass against a change that silently removes the entire
// install: every mutating step declares `dependsOn: [..., providerFederation]`,
// and `runStep` blocks a step whose dependency was "skipped without satisfying
// what it was there to establish". A `providerFederation` skip that forgot
// `satisfied: true` would cascade through the whole graph, and the run would
// report a tidy list of skips having touched nothing. That is not hypothetical
// -- install-e2e.yml's header records the gate doing exactly this when it was
// introduced.

import assert from "node:assert/strict";
import test from "node:test";
import fs from "node:fs/promises";
import path from "node:path";

import { AddClusterState, DEFAULT_INPUTS, requiredFields } from "../src/state/addCluster.js";
import { renderCollectScreen } from "../src/webview/installScreens.js";
import type { CollectScreenInput } from "../src/webview/installScreens.js";
import { installPlan } from "../src/install/session.js";
import type { SessionOptions } from "../src/install/session.js";
import { executeGraph } from "../src/install/executor.js";
import type { ScriptOutcome, ScriptRun } from "../src/install/runner.js";
import { graphDocumentPath, loadGraphFile } from "../src/install/graph.js";
import type { Graph, Step } from "../src/install/graph.js";

const REPO_ROOT = path.resolve(__dirname, "..", "..", "..", "..");

/** Anything that reads as an AI credential, whatever it is called. */
const CREDENTIAL_SHAPED = /provider|vendor|apikey|api_key|secret|token|credential|key/i;

function opts(over: Partial<SessionOptions> = {}): SessionOptions {
  return {
    root: "/nonexistent",
    receiptFile: "/nonexistent/receipt.json",
    skip: new Set<string>(),
    stepParams: {},
    domain: "memql.localhost",
    ownerEmail: "ada@example.com",
    ownerFirstName: "Ada",
    ownerLastName: "Lovelace",
    ...over,
  };
}

// ---------------------------------------------------------------------------
// the field tables
// ---------------------------------------------------------------------------

test("no action collects an AI credential, under any name", () => {
  // WIDENED FROM AN ABSENCE CHECK TO A SHAPE CHECK (epic memql#5088). It used
  // to name the two fields it wanted gone (`provider`, `providerKeyFile`),
  // which is a test that passes the moment somebody re-adds one under a third
  // name. `DEFAULT_INPUTS` has a key for every field the wizard collects, so
  // asserting over its keys catches a credential whatever it is called.
  const fields = Object.keys(DEFAULT_INPUTS);
  assert.ok(
    !fields.some((f) => CREDENTIAL_SHAPED.test(f)),
    `a collected field reads as an AI credential: ${fields.join(", ")}`,
  );
  for (const action of ["install", "installGuided", "repair"] as const) {
    assert.ok(
      !requiredFields(action).some((f) => CREDENTIAL_SHAPED.test(f)),
      `${action} requires a field that reads as an AI credential`,
    );
  }
});

test("the required tables are otherwise exactly what they were", () => {
  // Pinned in full rather than as a subtraction, so a future edit that drops
  // the owner fields (which seedBootstrap genuinely refuses without,
  // znasllc-io#3888) fails here rather than at `exit 2` on an operator's
  // machine.
  assert.deepEqual(requiredFields("install"), [
    "domain",
    "ownerFirstName",
    "ownerLastName",
    "ownerEmail",
    "version",
  ]);
  assert.deepEqual(requiredFields("installGuided"), requiredFields("install"));
  assert.deepEqual(requiredFields("repair"), [
    "domain",
    "ownerFirstName",
    "ownerLastName",
    "ownerEmail",
  ]);
  assert.deepEqual(requiredFields("uninstall"), []);
  assert.deepEqual(requiredFields("connect"), []);
  assert.deepEqual(requiredFields("reconnect"), []);
});

test("nothing is collected but never waited for", () => {
  // THE REPLACEMENT FOR `optionalFields` (epic memql#5088). That list existed
  // to hold exactly the two AI-provider fields -- "what else is worth offering
  // while we are here" -- and went with them, along with `validate()`'s second
  // pass and the disclosure that rendered it.
  //
  // What must stay true is the reason it was safe to delete: every field the
  // wizard holds is now required by SOME action, so there is no field being
  // collected that nothing waits for. A field re-added as optional would leave
  // a key in `DEFAULT_INPUTS` that appears in no required table, and that is
  // what this catches.
  const everRequired = new Set(
    (["install", "installGuided", "repair", "uninstall", "connect", "reconnect"] as const).flatMap(
      (action) => requiredFields(action) as string[],
    ),
  );
  for (const field of Object.keys(DEFAULT_INPUTS)) {
    assert.ok(
      everRequired.has(field),
      `${field} is collected but no action requires it -- either it is dead, or the ` +
        "optional tier is back and its validation is not running",
    );
  }
});

// ---------------------------------------------------------------------------
// the validator
// ---------------------------------------------------------------------------

test("a keyless install validates", () => {
  const s = new AddClusterState();
  s.chooseAction("install");
  s.setInput("domain", "memql.localhost");
  s.setInput("ownerFirstName", "Ada");
  s.setInput("ownerLastName", "Lovelace");
  s.setInput("ownerEmail", "ada@example.com");
  s.setInput("version", "v1.2.3");
  assert.deepEqual(s.validate(), [], "an install with no provider answers is refused");
  assert.equal(s.beginRun(), true);
});

test("a keyless repair validates", () => {
  const s = new AddClusterState();
  s.chooseAction("repair");
  s.setInput("domain", "memql.localhost");
  s.setInput("ownerFirstName", "Ada");
  s.setInput("ownerLastName", "Lovelace");
  s.setInput("ownerEmail", "ada@example.com");
  assert.deepEqual(s.validate(), []);
});

// THREE VALIDATOR CASES ARE DELETED HERE (epic memql#5088), all three about a
// field that no longer exists:
//
//   - `a supplied key still validates exactly as before`
//   - `the paste-the-key refusal survives the demotion to optional`
//   - `an unverifiable vendor is still refused, optional or not`
//
// The middle one is worth an extra sentence, because it guarded a real trap.
// memql#3545's refusal ran inside `validate()`'s loop over `requiredFields`, so
// memql#4440's demotion would have stopped it running silently -- and the value
// it caught goes on to a command line every process on the machine can read.
// That trap needed a field to have; the wall BEHIND it did not. `redactSecrets`
// still runs on the receipt write and the run-log write, covering every other
// way a param can reach a file, and `receiptSecrets.test.ts` and
// `runLogSecrets.test.ts` still hold it.

// ---------------------------------------------------------------------------
// the install plan
// ---------------------------------------------------------------------------

function step(id: string, script = "install.verifyProviderKey"): Step {
  return {
    id,
    script,
    label: "Working",
    description: "",
    elevation: "none",
    retained: false,
    retainedReason: "",
    shared: false,
    sharedReason: "",
    verify: { kind: "scriptOk" },
  };
}

/**
 * A runner that satisfies EVERY step's verify predicate.
 *
 * The leaf names are read off the graph rather than listed here, so a step
 * whose verify field is renamed does not quietly turn this into a test of the
 * verifier. `true` satisfies `resultTrue` and is non-empty for
 * `resultNonEmpty`, which are the only two kinds the install graph uses.
 */
function satisfyingResult(graph: Graph): Record<string, unknown> {
  const result: Record<string, unknown> = {};
  for (const s of graph.steps) {
    const field = s.verify?.field ?? "";
    const leaf = field.startsWith("result.") ? field.slice("result.".length) : "";
    if (leaf !== "") result[leaf] = true;
  }
  return result;
}

test("providerFederation is skipped -- with a stated reason", () => {
  const decision = installPlan(opts())(step("providerFederation"));
  assert.equal(decision.action, "skip");
  if (decision.action !== "skip") return;
  assert.equal(decision.reason, "no federation ids supplied -- installing spends no AI credit");
});

test("the providerFederation skip is SATISFIED", () => {
  // The single most consequential boolean in this epic. See the file header.
  const decision = installPlan(opts())(step("providerFederation"));
  assert.equal(decision.action, "skip");
  if (decision.action !== "skip") return;
  assert.equal(
    decision.satisfied,
    true,
    "an unsatisfied skip cascades through every mutating step and installs nothing",
  );
});

test("the skip is UNCONDITIONAL -- no caller can make this lane call a vendor", () => {
  // THE REPLACEMENT FOR `with a key, providerKey runs and carries both params`
  // (epic memql#5088), and it asserts the opposite thing on purpose.
  //
  // Under memql#4440 the skip was CONDITIONAL: supplying a key file made the
  // step run, so the plan had two branches and the old case pinned the second.
  // There is no key to supply, and the reason this lane never verifies is not
  // "nothing was supplied" -- it is that a local cluster cannot federate at all
  // (design D6). A conditional skip would be a way to send a k3d cluster's
  // unverifiable token to a vendor and fail the install on the answer.
  //
  // So: every route a caller has into the plan is tried, and none of them
  // reaches `run`. `stepParams` is the escape hatch the CLI exposes as
  // `--param=<step>.<flag>=<value>`, which is the one that could plausibly
  // resurrect the call.
  const routes: Partial<SessionOptions>[] = [
    {},
    { stepParams: { providerFederation: { provider: "anthropic" } } },
    { stepParams: { providerFederation: { "federation-deploy": "agent" } } },
    { domain: "lab.example.com" },
  ];
  for (const over of routes) {
    const decision = installPlan(opts(over))(step("providerFederation"));
    assert.equal(
      decision.action,
      "skip",
      `installPlan ran the vendor check for ${JSON.stringify(over)}`,
    );
  }
});

test("seedBootstrap is handed no provider arguments at all", () => {
  // seed-bootstrap.sh no longer DECLARES `--provider` or `--provider-key-file`,
  // and a capability script exits 2 on an undeclared flag -- so what must be
  // true is that neither ARRIVES. Under memql#4440 this was a correctness
  // point about a half-supplied shape; it is now a hard failure if it regresses.
  const decision = installPlan(opts())(step("seedBootstrap", "install.seedBootstrap"));
  assert.equal(decision.action, "run");
  if (decision.action !== "run") return;
  assert.equal(decision.params["provider"], undefined);
  assert.equal(decision.params["provider-key-file"], undefined);
  assert.ok(
    !Object.keys(decision.params).some((p) => CREDENTIAL_SHAPED.test(p)),
    `seedBootstrap was handed a credential-shaped param: ${Object.keys(decision.params).join(", ")}`,
  );
  assert.equal(decision.params["owner-email"], "ada@example.com", "the bootstrap set still arrives");
});

// ---------------------------------------------------------------------------
// the graph, executed
// ---------------------------------------------------------------------------

test("a keyless install still runs every mutating step", async () => {
  // THE ONE THAT MATTERS. Runs the REAL shipped graph document through the
  // REAL executor with the REAL install plan, stubbing only the script runner
  // -- so the assertion is about the executor's skip-blocks-dependents
  // semantics meeting this plan's decision, which is precisely what no
  // document-shaped test can see.
  const graph = await loadGraphFile(graphDocumentPath("install", REPO_ROOT));
  const ran: string[] = [];
  const report = await executeGraph({
    graph,
    plan: installPlan(opts()),
    scriptPath: (s: Step) => `/nonexistent/${s.script}`,
    run: async (invocation: ScriptRun): Promise<ScriptOutcome> => {
      ran.push(invocation.capability ?? "");
      return {
        argv: [],
        exitCode: 0,
        signal: null,
        stdout: "",
        stderr: "",
        envelope: {
          ok: true,
          capability: invocation.capability ?? "",
          changed: true,
          result: satisfyingResult(graph),
          error: null,
        },
      };
    },
  });

  const federation = report.outcomes.find((o) => o.id === "providerFederation");
  assert.equal(federation?.status, "skipped");
  assert.equal(federation?.satisfied, true);

  // Every OTHER step must have been invoked. Named individually rather than
  // as a count, so a step deleted from the graph cannot make this pass.
  for (const id of [
    "detect",
    "dockerAccess",
    "toolK3d",
    "toolKubectl",
    "toolMkcert",
    "hostsBlock",
    "browserTrust",
    "localCA",
    "stackCheckout",
    "clusterUp",
    "seedBootstrap",
    "frontDoor",
    "magicLink",
    "enrolmentLink",
    "recoveryKey",
  ]) {
    const outcome = report.outcomes.find((o) => o.id === id);
    assert.equal(
      outcome?.status,
      "ok",
      `${id} did not run on a keyless install (status ${outcome?.status ?? "absent"}) -- ` +
        "the providerFederation skip cascaded, and the install would have touched nothing",
    );
  }
  assert.ok(
    !ran.includes("install.verifyProviderKey"),
    "the vendor was called on an install of a cluster that cannot federate",
  );
});

test("every step that depends on providerFederation is one the skip must not block", async () => {
  // Reads the shipped document rather than restating the list: the dependency
  // set has grown before (memql#3473 added it to every mutating step) and a
  // test carrying its own copy would go quietly stale.
  const graph = await loadGraphFile(graphDocumentPath("install", REPO_ROOT));
  const dependents = graph.steps
    .filter((s) => (s.dependsOn ?? []).includes("providerFederation"))
    .map((s) => s.id);
  assert.ok(
    dependents.length > 0,
    "nothing depends on providerFederation any more -- the gate memql#3473 built is gone",
  );
});

// FOUR HAND-OFF CASES ARE DELETED HERE (epic memql#5088). They covered
// `AddClusterState.providerSetupUrl`, the done screen's link to the portal's
// AI-providers page: that a keyless install was offered it, that the address
// followed the operator's own domain, that an install which HAD seeded a key
// was offered nothing, and that a failed hand-off was offered nothing.
//
// The getter is gone, and each half of it was wrong by the time it went. The
// gate read `providerKeyFile`, a field that no longer exists, so it was true
// on every install there can now be; and the address was `portal.<domain>`,
// which epic memql#4984 retired -- `clusters/consoleUrl.ts` records that host
// as one nothing serves.
//
// NOTHING REPLACES THEM AT THIS LAYER, and that is a coverage loss worth
// naming. What the done screen says now is a fixed sentence in
// `addClusterPanel.ts` (`providerSettingsBlock`), and that module imports
// `vscode`, which this lane excludes by design -- the decision it used to hold
// was moved into the state machine for exactly that reason (memql#3884). There
// is no decision left to hold: the sentence is unconditional.

// ---------------------------------------------------------------------------
// the collect screen
// ---------------------------------------------------------------------------

function collect(over: Partial<CollectScreenInput> = {}): string {
  return renderCollectScreen({
    action: "install",
    values: { ...DEFAULT_INPUTS },
    errors: [],
    ...over,
  });
}

test("the form offers no AI credential control at all", () => {
  // THE INVERSE OF THE DISCLOSURE CASES memql#4440 WROTE. Those asserted the
  // vendor fields were present but collapsed; this asserts they are absent.
  //
  // Both the CONTROL and the CONTAINER are named, because they would fail
  // independently: a `<details class="optional-section">` left behind with
  // nothing in it renders an empty expander, and a `data-field` left behind
  // renders a box the state machine cannot store.
  for (const action of ["install", "installGuided", "repair"] as const) {
    const html = collect({ action });
    assert.doesNotMatch(html, /<details class="optional-section"/, `${action} kept the disclosure`);
    assert.doesNotMatch(html, /data-field="provider/, `${action} kept a vendor field`);
    assert.doesNotMatch(html, /data-act="browseKeyFile"/, `${action} kept the key-file picker`);
    assert.doesNotMatch(html, /AI provider key/i, `${action} still says "AI provider key"`);
  }
});

test("the form says where a local cluster's models come from instead", () => {
  // A DELETION THAT SAYS NOTHING IS ITS OWN DEFECT. An operator who installs a
  // cluster and finds its agents cannot think is owed the reason, and the
  // reason is not "you forgot to enter a key" -- it is that this cluster cannot
  // hold one. The sentence replaces the disclosure's, in the same place.
  const html = collect();
  assert.match(html, /No AI credential is collected/);
  assert.match(html, /makes no call to any AI vendor/);
  assert.match(html, /fleet machine/);
  assert.match(html, /model running locally/);
  // The reachable positive for the two doesNotMatch assertions above: this
  // renderer does produce text in that slot, so their silence is evidence.
  assert.match(html, /class="hint"/);
});

// ---------------------------------------------------------------------------
// the extraction, as a countable invariant
// ---------------------------------------------------------------------------

test("exactly ONE producer of a field's markup", async () => {
  // WHY A SOURCE-LEVEL COUNT RATHER THAN A RENDER ASSERTION. What can go wrong
  // here is not a wrong page -- it is a SECOND implementation that renders the
  // same thing, after which the two drift and only one of them gets the next
  // fix. That is invisible to every behavioural test, because both copies work
  // on the day they are written.
  //
  // The extraction (renderField) exists precisely to prevent that, and
  // memql#4440's own rebase is how it nearly came back: memql#4430 rewrote the
  // same function from the other end, and a merge that took both sides verbatim
  // would have produced two field renderers that compile, pass, and disagree
  // six months later.
  //
  // IT SURVIVES THE DISCLOSURE IT WAS WRITTEN FOR (epic memql#5088). The second
  // caller `renderField` was extracted for is gone, so there is one caller
  // again -- which is precisely when a future edit is most likely to inline it
  // back and re-open the seam.
  //
  // `data-invalid=` is the marker because it opens a field's wrapper element
  // and appears nowhere else in the module -- so counting it counts producers.
  //
  // If a legitimate second renderer is ever added, this test should be UPDATED
  // with the reason rather than deleted; the count is the point, not the 1.
  const source = await fs.readFile(
    path.join(REPO_ROOT, "editors", "vscode", "src", "webview", "installScreens.ts"),
    "utf8",
  );
  const producers = source.split("data-invalid=").length - 1;
  assert.equal(
    producers,
    1,
    `installScreens.ts has ${producers} places emitting a field wrapper; there must be exactly one ` +
      "(renderField). A second one is how two callers start disagreeing about what a field " +
      "looks like.",
  );

  // The reachable positive: the marker is actually present, so a rename that
  // made this count zero cannot pass as "no duplicates".
  assert.ok(source.includes("function renderField("), "renderField has been renamed or removed");
});
