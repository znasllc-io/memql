// Installing a MemQL cluster does not require an AI provider key
// (epic memql#4440, task memql#4441).
//
// WHY THIS FILE EXISTS RATHER THAN A FEW LINES IN THE NEIGHBOURING SUITES.
// The claim being defended is a single sentence -- "no lifecycle verb needs a
// vendor credential" -- and it is spread across four modules that otherwise
// have nothing to do with each other: the required-field tables, the
// validator, the install plan, and the graph executor's skip semantics. Split
// across four suites it reads as four unrelated assertions, and the one that
// actually protects an operator (the graph-level one, below) reads as a test
// about dependency edges.
//
// THE LOAD-BEARING TEST IS `a keyless install still runs every mutating step`.
// The others would all pass against a change that silently removes the entire
// install: every mutating step declares `dependsOn: [..., providerKey]`, and
// `runStep` blocks a step whose dependency was "skipped without satisfying
// what it was there to establish". A `providerKey` skip that forgot
// `satisfied: true` would cascade through the whole graph, and the run would
// report a tidy list of skips having touched nothing. That is not
// hypothetical -- install-e2e.yml's header records the gate doing exactly this
// when it was introduced.

import assert from "node:assert/strict";
import test from "node:test";
import fs from "node:fs/promises";
import path from "node:path";

import {
  AddClusterState,
  DEFAULT_INPUTS,
  optionalFields,
  requiredFields,
} from "../src/state/addCluster.js";
import { renderCollectScreen } from "../src/webview/installScreens.js";
import type { CollectScreenInput } from "../src/webview/installScreens.js";
import { installPlan } from "../src/install/session.js";
import type { SessionOptions } from "../src/install/session.js";
import { executeGraph } from "../src/install/executor.js";
import type { ScriptOutcome, ScriptRun } from "../src/install/runner.js";
import { graphDocumentPath, loadGraphFile } from "../src/install/graph.js";
import type { Graph, Step } from "../src/install/graph.js";

const REPO_ROOT = path.resolve(__dirname, "..", "..", "..", "..");

function opts(over: Partial<SessionOptions> = {}): SessionOptions {
  return {
    root: "/nonexistent",
    receiptFile: "/nonexistent/receipt.json",
    skip: new Set<string>(),
    stepParams: {},
    // Pre-answered by the wizard exactly as it is today: `provider` has a
    // house default and is offered in the disclosure. What decides whether a
    // vendor is contacted is the KEY FILE, never this.
    provider: "anthropic",
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

test("no action requires an AI provider key", () => {
  for (const action of ["install", "installGuided", "repair"] as const) {
    const required = requiredFields(action);
    assert.ok(
      !required.includes("provider"),
      `${action} still requires a provider -- installing spends no inference`,
    );
    assert.ok(
      !required.includes("providerKeyFile"),
      `${action} still requires a key file -- installing spends no inference`,
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

test("the key fields are still COLLECTED, just never waited for", () => {
  // Demoted, not deleted. An operator who has a key must still have somewhere
  // to put it, or this epic would have removed a capability rather than a
  // requirement.
  for (const action of ["install", "installGuided", "repair"] as const) {
    assert.deepEqual(optionalFields(action), ["provider", "providerKeyFile"]);
  }
  for (const action of ["uninstall", "connect", "reconnect"] as const) {
    assert.deepEqual(optionalFields(action), []);
  }
});

test("no field is both required and optional", () => {
  // The two lists answer different questions ("may this run start" versus
  // "what else is worth offering"), and a field in both would make the answer
  // depend on which loop ran last.
  for (const action of ["install", "installGuided", "repair", "uninstall", "connect", "reconnect"] as const) {
    const required = new Set(requiredFields(action));
    for (const field of optionalFields(action)) {
      assert.ok(!required.has(field), `${field} is in both tables for ${action}`);
    }
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

test("a supplied key still validates exactly as before", () => {
  const s = new AddClusterState();
  s.chooseAction("install");
  s.setInput("domain", "memql.localhost");
  s.setInput("ownerFirstName", "Ada");
  s.setInput("ownerLastName", "Lovelace");
  s.setInput("ownerEmail", "ada@example.com");
  s.setInput("version", "v1.2.3");
  s.setInput("provider", "anthropic");
  s.setInput("providerKeyFile", "/home/ada/.anthropic-key");
  assert.deepEqual(s.validate(), []);
});

test("the paste-the-key refusal survives the demotion to optional", () => {
  // THE TRAP IN MAKING A REQUIRED FIELD OPTIONAL. memql#3545's refusal ran
  // inside `validate()`'s loop over requiredFields, so demoting the field
  // would have silently stopped running it -- and the value it catches goes
  // on to a command line every process on the machine can read, and is then
  // written verbatim into the install receipt.
  const s = new AddClusterState();
  s.chooseAction("install");
  s.setInput("domain", "memql.localhost");
  s.setInput("ownerFirstName", "Ada");
  s.setInput("ownerLastName", "Lovelace");
  s.setInput("ownerEmail", "ada@example.com");
  s.setInput("version", "v1.2.3");
  s.setInput("providerKeyFile", "sk-ant-api03-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa");
  const errors = s.validate();
  const problem = errors.find((e) => e.field === "providerKeyFile");
  assert.ok(problem !== undefined, "a pasted key is accepted now that the field is optional");
  assert.match(problem!.message, /PATH/);
  assert.ok(
    !problem!.message.includes("sk-ant-api03"),
    "the message quotes the secret back, and validation messages end up in screenshots",
  );
  assert.equal(s.beginRun(), false, "the run started with a key on the command line");
});

test("an unverifiable vendor is still refused, optional or not", () => {
  const s = new AddClusterState();
  s.chooseAction("install");
  s.setInput("domain", "memql.localhost");
  s.setInput("ownerFirstName", "Ada");
  s.setInput("ownerLastName", "Lovelace");
  s.setInput("ownerEmail", "ada@example.com");
  s.setInput("version", "v1.2.3");
  s.setInput("provider", "gemini");
  assert.match(
    s.validate().find((e) => e.field === "provider")?.message ?? "",
    /anthropic or openai/,
  );
});

// ---------------------------------------------------------------------------
// the install plan
// ---------------------------------------------------------------------------

function step(id: string, script = "install.verifyProviderKey"): Step {
  return {
    id,
    script,
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

test("with no key, providerKey is skipped -- with a stated reason", () => {
  const decision = installPlan(opts())(step("providerKey"));
  assert.equal(decision.action, "skip");
  if (decision.action !== "skip") return;
  assert.equal(decision.reason, "no key supplied -- configure AI providers in the portal");
});

test("with no key, the providerKey skip is SATISFIED", () => {
  // The single most consequential boolean in this epic. See the file header.
  const decision = installPlan(opts())(step("providerKey"));
  assert.equal(decision.action, "skip");
  if (decision.action !== "skip") return;
  assert.equal(
    decision.satisfied,
    true,
    "an unsatisfied skip cascades through every mutating step and installs nothing",
  );
});

test("a whitespace-only key file is treated as no key, not as a key", () => {
  const decision = installPlan(opts({ providerKeyFile: "   " }))(step("providerKey"));
  assert.equal(decision.action, "skip");
});

test("with a key, providerKey runs and carries both params", () => {
  const decision = installPlan(
    opts({ provider: "anthropic", providerKeyFile: "/tmp/key" }),
  )(step("providerKey"));
  assert.equal(decision.action, "run");
  if (decision.action !== "run") return;
  assert.equal(decision.params["key-file"], "/tmp/key");
  assert.equal(decision.params["provider"], "anthropic");
});

test("with no key, seedBootstrap is handed no provider arguments at all", () => {
  // `stage_provider_key` returns cleanly when given neither, so what must be
  // true is that neither ARRIVES -- a `--provider=anthropic` with no key file
  // is the half-supplied shape nothing downstream expects.
  const decision = installPlan(opts({ provider: "anthropic" }))(
    step("seedBootstrap", "install.seedBootstrap"),
  );
  assert.equal(decision.action, "run");
  if (decision.action !== "run") return;
  assert.equal(decision.params["provider"], undefined);
  assert.equal(decision.params["provider-key-file"], undefined);
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

  const providerKey = report.outcomes.find((o) => o.id === "providerKey");
  assert.equal(providerKey?.status, "skipped");
  assert.equal(providerKey?.satisfied, true);

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
        "the providerKey skip cascaded, and the install would have touched nothing",
    );
  }
  assert.ok(
    !ran.includes("install.verifyProviderKey"),
    "the vendor was called on an install that supplied no key",
  );
});

test("every step that depends on providerKey is one the skip must not block", async () => {
  // Reads the shipped document rather than restating the list: the dependency
  // set has grown before (memql#3473 added it to every mutating step) and a
  // test carrying its own copy would go quietly stale.
  const graph = await loadGraphFile(graphDocumentPath("install", REPO_ROOT));
  const dependents = graph.steps
    .filter((s) => (s.dependsOn ?? []).includes("providerKey"))
    .map((s) => s.id);
  assert.ok(
    dependents.length > 0,
    "nothing depends on providerKey any more -- the gate memql#3473 built is gone",
  );
});

// ---------------------------------------------------------------------------
// the done screen's hand-off (the DECISION, which the panel only renders)
// ---------------------------------------------------------------------------

function handedOff(state: AddClusterState, domain: string): void {
  state.setHandoff({
    ok: true,
    cluster: { name: "memql", endpoint: `https://api.${domain}:443`, domain, local: true },
    canSignIn: true,
  });
}

test("a keyless install is handed the portal's provider page", () => {
  const s = new AddClusterState();
  s.chooseAction("install");
  handedOff(s, "memql.localhost");
  assert.equal(s.providerSetupUrl, "https://portal.memql.localhost/settings/providers");
});

test("the address follows the domain the operator actually gave", () => {
  // Composed rather than pinned: `portal.<domain>` is the front door's own
  // single-label-under-the-domain rule, and a hardcoded memql.localhost would
  // send every custom-domain operator to a host that does not exist.
  const s = new AddClusterState();
  s.chooseAction("install");
  handedOff(s, "lab.example.com");
  assert.equal(s.providerSetupUrl, "https://portal.lab.example.com/settings/providers");
});

test("an install that DID seed a key is offered nothing", () => {
  // Inviting an operator to go and configure the providers they just
  // configured reads as though the key had not taken.
  const s = new AddClusterState();
  s.chooseAction("install");
  s.setInput("providerKeyFile", "/home/ada/.anthropic-key");
  handedOff(s, "memql.localhost");
  assert.equal(s.providerSetupUrl, "");
});

test("no hand-off, no link", () => {
  // A run that registered no cluster has no domain worth linking into, and the
  // failed-write screen is already saying something more urgent.
  const s = new AddClusterState();
  s.chooseAction("install");
  assert.equal(s.providerSetupUrl, "");
  s.setHandoff({ ok: false, reachableAt: "https://api.memql.localhost:443", message: "nope" });
  assert.equal(s.providerSetupUrl, "");
});

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

test("the provider fields are inside a collapsed disclosure, not on the form", () => {
  const html = collect();
  assert.match(html, /<details class="optional-section">/);
  assert.match(html, /AI provider \(optional -- configure later in the portal\)/);
  // Still rendered -- demoted, not deleted.
  assert.match(html, /data-field="provider"/);
  assert.match(html, /data-field="providerKeyFile"/);
});

test("the disclosure is closed by default", () => {
  // An operator with no key should be able to read the form top to bottom and
  // never learn that LLM vendors exist.
  assert.doesNotMatch(collect(), /<details class="optional-section" open>/);
});

test("the disclosure OPENS when one of its fields is in error", () => {
  // The failure mode a disclosure introduces: a validation message inside a
  // closed <details> is a form that refuses to start and will not say why.
  const html = collect({
    errors: [{ field: "providerKeyFile", message: "That is the key itself." }],
  });
  assert.match(html, /<details class="optional-section" open>/);
  assert.match(html, /That is the key itself\./);
});

test("an error on a REQUIRED field leaves the disclosure closed", () => {
  const html = collect({ errors: [{ field: "ownerEmail", message: "An email is required." }] });
  assert.doesNotMatch(html, /<details class="optional-section" open>/);
});

test("the disclosure says installing needs none of it", () => {
  const html = collect();
  assert.match(html, /Nothing here is needed to install/);
  assert.match(html, /makes no call to any AI vendor/);
  assert.match(html, /federation is the recommended path/);
});

test("a repair gets the same disclosure; uninstall gets none", () => {
  assert.match(collect({ action: "repair" }), /<details class="optional-section"/);
  assert.doesNotMatch(collect({ action: "uninstall" }), /<details class="optional-section"/);
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
  // The extraction (renderField) exists precisely to prevent that, and this
  // epic's own rebase is how it nearly came back: memql#4430 rewrote the same
  // function from the other end, and a merge that took both sides verbatim
  // would have produced two field renderers that compile, pass, and disagree
  // six months later.
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
      "(renderField). A second one is how the required list and the optional disclosure start " +
      "disagreeing about what a field looks like.",
  );

  // The reachable positive: the marker is actually present, so a rename that
  // made this count zero cannot pass as "no duplicates".
  assert.ok(source.includes("function renderField("), "renderField has been renamed or removed");
});
