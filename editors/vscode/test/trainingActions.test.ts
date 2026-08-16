// The four training actions end to end, against a stub engine that keeps a
// registry.
//
// THE REGISTRY IS WHY THIS FIXTURE IS NOT JUST A CALL LOG. The acceptance
// criterion for a dry-run is "it mutates nothing -- asserted against the
// registry, not assumed", and a call log can only say "we did not call the
// method we know about". So the stub keeps state that its mutating methods
// actually change, and the dry-run case deep-compares a snapshot of it taken
// before and after. The call log is kept as well, for the cases that are about
// ORDER -- validate before define, the closure shown before anything is
// submitted, the override never carried on a first attempt.
//
// The cases that are worth reading twice:
//
//   - THE BREAKING REFUSAL. Its failure mode is a raw error string: the engine
//     returns a classified diff AND a prose refusal, and rendering the prose is
//     both easier and useless. The assertions are on the FIELDS -- the field
//     name, the rows, the constructs that reference it -- reaching the report.
//
//   - THE OVERRIDE. Its failure mode is a promote that succeeds too easily.
//     The assertions are that the first attempt carries no flag, that the flag
//     appears only after the OVERRIDE channel answered yes, and that answering
//     the ordinary channel never unlocks it.
//
//   - THE SESSION DEFINITIONS AFTER A RECONNECT. Its failure mode is a lens
//     still claiming a construct is live on a stream that ended -- an editor
//     asserting something false about a cluster.
//
// Refs: #3763 #3745

import test from "node:test";
import assert from "node:assert/strict";

import type {
  ConceptSchemaDiff,
  DurableDemoteBundleResult,
  DurablePromoteBundleResult,
  SessionDefineBundleResult,
  StageBundleResult,
  ValidateBundleResult,
} from "@znasllc-io/memql-sdk-core/authoring";

import type { MappedDiagnostic } from "../src/run/diagnostics.js";
import type { TrainingConstruct, TrainingState } from "../src/state/training.js";
import {
  TrainingActions,
  type TrainingDeps,
  type TrainingEngine,
  type TrainingOutcome,
  type TrainingScope,
} from "../src/training/actions.js";
import {
  assembleClosure,
  assembleConstruct,
  type TrainingBundle,
  type TrainingWorkspace,
} from "../src/training/closure.js";
import { outcomeReport } from "../src/training/outcomeReport.js";
import { sessionLensPlans } from "../src/training/session.js";
import { DEFAULT_STACK_TAG } from "../src/install/stackPin.js";
import type { TrainingCluster, TrainingPrompt } from "../src/training/report.js";

// -----------------------------------------------------------------------------
// Harness
// -----------------------------------------------------------------------------

type Call =
  | { op: "validate"; sources: string }
  | { op: "define"; sources: string }
  | { op: "stage"; sources: string }
  | { op: "promote"; sources: string; allowBreaking: boolean }
  | { op: "demote"; sources: string };

/**
 * The registry a dry-run must leave alone.
 *
 * Deliberately the two things the engine actually changes: what is promoted into
 * the shared registry, and what is defined on this stream. A dry-run touches
 * neither, and the case that says so compares this whole object rather than
 * counting calls.
 */
interface StubRegistry {
  promoted: string[];
  sessionDefined: string[];
  /** The author's own durable tier -- separate from `promoted`, which is shared. */
  staged: string[];
}

class StubEngine implements TrainingEngine {
  readonly calls: Call[] = [];
  readonly registry: StubRegistry = { promoted: [], sessionDefined: [], staged: [] };

  validateResult: ValidateBundleResult = { ok: true, diagnostics: [] };
  defineResult: SessionDefineBundleResult = {
    ok: true,
    defined: [{ kind: "query", name: "spaceParticipants" }],
    diagnostics: [],
    error: "",
  };
  promoteResult: DurablePromoteBundleResult = {
    ok: true,
    promoted: [{ kind: "query", name: "spaceParticipants" }],
    diagnostics: [],
    error: "",
    conceptDiffs: [],
  };
  demoteResult: DurableDemoteBundleResult = {
    ok: true,
    demoted: [{ kind: "query", name: "spaceParticipants" }],
    outcomes: [
      { kind: "query", name: "spaceParticipants", conceptId: "", outcome: "removed", rowCount: 0 },
    ],
    diagnostics: [],
    error: "",
  };
  stageResult: StageBundleResult = {
    ok: true,
    staged: [{ kind: "query", name: "spaceParticipants" }],
    diagnostics: [],
    error: "",
  };
  throwOn: Call["op"] | undefined;
  // What `throwOn` throws. Overridable so a test can supply the SDK's
  // transport-reason error rather than a bare one (memql#4000).
  throwWith: unknown = new Error("transport died");

  async validateBundle(sources: string): Promise<ValidateBundleResult> {
    this.calls.push({ op: "validate", sources });
    if (this.throwOn === "validate") throw this.throwWith;
    return this.validateResult;
  }

  async sessionDefineBundle(sources: string): Promise<SessionDefineBundleResult> {
    this.calls.push({ op: "define", sources });
    if (this.throwOn === "define") throw new Error("transport died");
    if (this.defineResult.ok) {
      for (const c of this.defineResult.defined) this.registry.sessionDefined.push(c.name);
    }
    return this.defineResult;
  }

  async stageBundle(sources: string): Promise<StageBundleResult> {
    this.calls.push({ op: "stage", sources });
    if (this.throwOn === "stage") throw this.throwWith;
    if (this.stageResult.ok) {
      for (const c of this.stageResult.staged) this.registry.staged.push(c.name);
    }
    return this.stageResult;
  }

  async durablePromoteBundle(
    sources: string,
    options: { allowBreaking?: boolean },
  ): Promise<DurablePromoteBundleResult> {
    this.calls.push({ op: "promote", sources, allowBreaking: options.allowBreaking === true });
    if (this.throwOn === "promote") throw new Error("transport died");
    if (this.promoteResult.ok) {
      for (const c of this.promoteResult.promoted) this.registry.promoted.push(c.name);
    }
    return this.promoteResult;
  }

  async durableDemoteBundle(sources: string): Promise<DurableDemoteBundleResult> {
    this.calls.push({ op: "demote", sources });
    if (this.throwOn === "demote") throw new Error("transport died");
    if (this.demoteResult.ok) {
      for (const c of this.demoteResult.demoted) {
        const at = this.registry.promoted.indexOf(c.name);
        if (at >= 0) this.registry.promoted.splice(at, 1);
      }
    }
    return this.demoteResult;
  }

  ops(): string[] {
    return this.calls.map((c) => c.op);
  }
}

const LOCAL: TrainingCluster = { name: "local", label: "local", local: true };
const STAGING: TrainingCluster = { name: "staging", label: "staging", local: false };

function construct(name: string, state: TrainingState, line = 0): TrainingConstruct {
  return {
    kind: "query",
    name,
    signatureRange: { start: { line, character: 0 }, end: { line, character: 12 } },
    state,
  };
}

interface Harness {
  engine: StubEngine;
  actions: TrainingActions;
  /** Every confirmation asked through the ORDINARY channel, with the engine calls made so far. */
  confirms: { prompt: TrainingPrompt; callsSoFar: string[] }[];
  /** Every confirmation asked through the OVERRIDE channel. */
  overrides: TrainingPrompt[];
  published: MappedDiagnostic[][];
  catalogRefreshes: number;
  setCluster(cluster: TrainingCluster | undefined): void;
  setConnected(connected: boolean): void;
  answerConfirm(answer: boolean): void;
  answerOverride(answer: boolean): void;
  /** Adds a dependency file with the given per-construct states. */
  addDependency(path: string, text: string, constructs: TrainingConstruct[] | undefined): void;
}

const ACTIVE_PATH = "/w/dsl/demo/queries.memql";
const ACTIVE_TEXT = ["query spaceParticipants {", "  filter a==1", "}", ""].join("\n");

function harness(): Harness {
  const engine = new StubEngine();
  const confirms: { prompt: TrainingPrompt; callsSoFar: string[] }[] = [];
  const overrides: TrainingPrompt[] = [];
  const published: MappedDiagnostic[][] = [];
  let cluster: TrainingCluster | undefined = LOCAL;
  let connected = true;
  let confirmAnswer = true;
  let overrideAnswer = true;
  let catalogRefreshes = 0;

  const dependencies = new Map<
    string,
    { text: string; constructs: TrainingConstruct[] | undefined }
  >();
  const activeConstructs: TrainingConstruct[] = [construct("spaceParticipants", "untrained")];

  const ws = (): TrainingWorkspace => ({
    resolveImport: (dotted) => (dependencies.has(dotted) ? dotted : undefined),
    read: (p) => {
      const dep = dependencies.get(p);
      return dep === undefined ? undefined : { text: dep.text };
    },
    imports: (p) => (p === ACTIVE_PATH ? [...dependencies.keys()] : []),
    trainingStates: (p) => {
      if (p === ACTIVE_PATH) return activeConstructs;
      const dep = dependencies.get(p);
      return dep === undefined ? undefined : dep.constructs;
    },
  });

  const deps: TrainingDeps = {
    cluster: () => cluster,
    engine: () => (connected ? engine : undefined),
    assemble: async (request, scope: TrainingScope): Promise<TrainingBundle | undefined> => {
      if (scope === "construct") {
        return assembleConstruct(ACTIVE_PATH, ACTIVE_TEXT, activeConstructs, request.name);
      }
      return assembleClosure(ACTIVE_PATH, ACTIVE_TEXT, ws());
    },
    confirm: async (prompt) => {
      confirms.push({ prompt, callsSoFar: engine.ops() });
      return confirmAnswer;
    },
    confirmOverride: async (prompt) => {
      overrides.push(prompt);
      return overrideAnswer;
    },
    publishDiagnostics: (mapped) => {
      published.push(mapped);
    },
    display: (p) => p.replace("/w/", ""),
    catalogChanged: () => {
      catalogRefreshes += 1;
    },
  };

  const actions = new TrainingActions(deps);
  return {
    engine,
    actions,
    confirms,
    overrides,
    published,
    get catalogRefreshes() {
      return catalogRefreshes;
    },
    setCluster: (next) => {
      cluster = next;
    },
    setConnected: (next) => {
      connected = next;
    },
    answerConfirm: (answer) => {
      confirmAnswer = answer;
    },
    answerOverride: (answer) => {
      overrideAnswer = answer;
    },
    addDependency: (path, text, constructs) => {
      dependencies.set(path, { text, constructs });
    },
  };
}

const REQUEST = { uri: `file://${ACTIVE_PATH}`, name: "spaceParticipants" };

function breakingDiff(overrides: Partial<ConceptSchemaDiff> = {}): ConceptSchemaDiff {
  return {
    concept: "v1:demo:widget",
    breaking: true,
    overridden: false,
    changes: [
      {
        concept: "v1:demo:widget",
        field: "colour",
        kind: "field_removed",
        breaking: true,
        was: "string",
        now: "",
        rowsAffected: 1204,
        rowCountKnown: true,
        referencedBy: ["query:widgetsByColour", "mutation:setWidgetColour"],
        detail: "removing colour strands 1204 rows",
      },
    ],
    summary: "v1:demo:widget: field_removed colour",
    ...overrides,
  };
}

// -----------------------------------------------------------------------------
// Dry-run
// -----------------------------------------------------------------------------

test("a dry-run leaves the registry byte-identical", async () => {
  // THE ACCEPTANCE CRITERION, asserted against state rather than against a call
  // log. "We did not call the mutating method" is a claim about the code under
  // test; "nothing in the registry moved" is a claim about the outcome.
  const h = harness();
  const before = JSON.stringify(h.engine.registry);

  const outcome = await h.actions.dryRun(REQUEST);

  assert.equal(outcome.status, "ok");
  assert.equal(JSON.stringify(h.engine.registry), before);
  assert.deepEqual(h.engine.ops(), ["validate"]);
});

test("a dry-run asks no confirmation -- there is nothing to confirm", async () => {
  const h = harness();
  h.setCluster(STAGING);
  await h.actions.dryRun(REQUEST);
  assert.deepEqual(h.confirms, []);
});

test("a dry-run's failures land as diagnostics, and the next clean one clears them", async () => {
  const h = harness();
  h.engine.validateResult = {
    ok: false,
    diagnostics: [
      {
        name: "spaceParticipants",
        kind: "query",
        ok: false,
        skipped: false,
        error: "unknown spec isActive",
        line: 1,
        column: 3,
        endLine: 0,
        endColumn: 0,
      },
    ],
  };

  const bad = await h.actions.dryRun(REQUEST);
  assert.equal(bad.status, "invalid");
  assert.equal(h.published.at(-1)?.length, 1);
  assert.equal(h.published.at(-1)?.[0]?.path, ACTIVE_PATH);
  // 1-based line 1 -> editor line 0, and the file's own coordinates rather than
  // the bundle's.
  assert.equal(h.published.at(-1)?.[0]?.start.line, 0);

  h.engine.validateResult = { ok: true, diagnostics: [] };
  const good = await h.actions.dryRun(REQUEST);
  assert.equal(good.status, "ok");
  assert.deepEqual(h.published.at(-1), [], "an empty publish is what CLEARS the panel");
});

test("a skipped construct is not a compile error", async () => {
  // The single most likely bug in a consumer: a bundle routinely carries a
  // concept or a shape this pass does not compile, each reporting ok=false with
  // skipped=true. Reading `!ok` alone turns a perfectly valid bundle into a
  // screen of errors.
  const h = harness();
  h.engine.validateResult = {
    ok: true,
    diagnostics: [
      {
        name: "widget",
        kind: "concept",
        ok: false,
        skipped: true,
        error: "this pass does not compile concepts",
        line: 0,
        column: 0,
        endLine: 0,
        endColumn: 0,
      },
    ],
  };

  const outcome = await h.actions.dryRun(REQUEST);
  assert.equal(outcome.status, "ok");
  assert.deepEqual(h.published.at(-1), []);
});

test("a positionless diagnostic becomes file-level rather than line 0", async () => {
  // The zero rule: every position field is 0 when the engine could not compute
  // one, and reading that as line 0 parks the diagnostic on the first line of
  // the bundle -- a dependency far more often than the file being edited.
  const h = harness();
  h.engine.validateResult = {
    ok: false,
    diagnostics: [
      {
        name: "spaceParticipants",
        kind: "query",
        ok: false,
        skipped: false,
        error: "refused",
        line: 0,
        column: 0,
        endLine: 0,
        endColumn: 0,
      },
    ],
  };

  await h.actions.dryRun(REQUEST);
  const diagnostic = h.published.at(-1)?.[0];
  assert.equal(diagnostic?.fileLevel, true);
  assert.equal(diagnostic?.path, ACTIVE_PATH, "file-level lands on the ACTIVE file");
  assert.ok(diagnostic?.message.includes("no source position"));
});

// -----------------------------------------------------------------------------
// Try in session
// -----------------------------------------------------------------------------

test("try in session validates BEFORE defining, and says it is temporary", async () => {
  const h = harness();
  const outcome = await h.actions.tryInSession(REQUEST);

  assert.equal(outcome.status, "ok");
  assert.deepEqual(h.engine.ops(), ["validate", "define"]);
  const prompt = h.confirms[0]?.prompt;
  assert.ok(prompt !== undefined);
  assert.ok(
    prompt.detail.includes("TEMPORARY"),
    "the confirmation is where temporariness is said, and it must say it",
  );
  assert.ok(prompt.detail.includes("local"), "and it names the cluster");
  assert.ok(prompt.confirmLabel.toLowerCase().includes("session"));
});

test("declining try in session defines nothing", async () => {
  const h = harness();
  h.answerConfirm(false);
  const outcome = await h.actions.tryInSession(REQUEST);
  assert.equal(outcome.status, "declined");
  assert.deepEqual(h.engine.ops(), []);
  assert.deepEqual(h.engine.registry.sessionDefined, []);
});

test("a session-defined construct is live for the session and GONE after a reconnect", async () => {
  // The acceptance criterion, in the half this process owns: the engine drops
  // the definitions when the stream ends and does not say so, so what must not
  // survive is this editor's claim about them.
  const h = harness();
  await h.actions.tryInSession(REQUEST);

  assert.equal(h.actions.sessionDefinitions.isDefined("local", "spaceParticipants"), true);
  const live = sessionLensPlans([construct("spaceParticipants", "untrained")], (name) =>
    h.actions.sessionDefinitions.isDefined("local", name),
  );
  assert.equal(live.length, 1);
  assert.ok(live[0]?.title.includes("session"));

  h.actions.noteStreamReset();

  assert.equal(h.actions.sessionDefinitions.isDefined("local", "spaceParticipants"), false);
  const afterReconnect = sessionLensPlans([construct("spaceParticipants", "untrained")], (name) =>
    h.actions.sessionDefinitions.isDefined("local", name),
  );
  assert.deepEqual(afterReconnect, [], "no lens may claim a definition on a stream that ended");
});

test("a session-define that fails on the transport claims nothing", async () => {
  const h = harness();
  h.engine.throwOn = "define";
  const outcome = await h.actions.tryInSession(REQUEST);
  assert.equal(outcome.status, "error");
  // The registration state is unknown, so the safe direction is to claim
  // nothing -- a lens that stops saying something true costs nothing.
  assert.equal(h.actions.sessionDefinitions.isDefined("local", "spaceParticipants"), false);
});

// -----------------------------------------------------------------------------
// The version-skew hint on a severed session (memql#4000)
// -----------------------------------------------------------------------------
//
// This is the path the motivating incident travelled: a plugin newer than its
// cluster sends a field that cluster refuses, the refusal ENDS THE SESSION
// rather than failing the request, and the operator reads
// `ERROR (validate): stream closed` with nothing anywhere naming the skew.

const transportClose = (): Error =>
  Object.assign(new Error("stream closed"), { reason: "transport" });

test("a severed session on a cluster BEHIND the plugin names the possible skew", async () => {
  const h = harness();
  h.setCluster({ ...LOCAL, version: "v0.17.0" });
  h.engine.throwOn = "validate";
  h.engine.throwWith = transportClose();

  const outcome = await h.actions.dryRun(REQUEST);
  assert.equal(outcome.status, "error");
  assert.ok(outcome.status === "error" && outcome.message.startsWith("stream closed"));
  assert.match(outcome.status === "error" ? outcome.message : "", /v0\.17\.0/);
  assert.match(outcome.status === "error" ? outcome.message : "", /upgrad/i);
});

test("a severed session on a CURRENT cluster says nothing extra", async () => {
  // The failure is real but version skew cannot explain it, and a hint that
  // fires on every dropped socket is one an operator learns to skip.
  const h = harness();
  h.setCluster({ ...LOCAL, version: DEFAULT_STACK_TAG });
  h.engine.throwOn = "validate";
  h.engine.throwWith = transportClose();

  const outcome = await h.actions.dryRun(REQUEST);
  assert.equal(outcome.status === "error" && outcome.message, "stream closed");
});

test("a cluster with no recorded version says nothing extra", async () => {
  const h = harness();
  h.engine.throwOn = "validate";
  h.engine.throwWith = transportClose();
  const outcome = await h.actions.dryRun(REQUEST);
  assert.equal(outcome.status === "error" && outcome.message, "stream closed");
});

test("an ordinary error on an old cluster is not dressed up as version skew", async () => {
  // `throwWith` defaults to a bare Error with no transport reason. The socket
  // was fine, so a severed-session explanation does not apply.
  const h = harness();
  h.setCluster({ ...LOCAL, version: "v0.17.0" });
  h.engine.throwOn = "validate";
  const outcome = await h.actions.dryRun(REQUEST);
  assert.equal(outcome.status === "error" && outcome.message, "transport died");
});

// -----------------------------------------------------------------------------
// Promote
// -----------------------------------------------------------------------------

test("a promote shows the closure BEFORE anything is submitted", async () => {
  // The acceptance criterion. The developer clicked a lens above one construct;
  // what actually goes is that construct plus every dependency the cluster does
  // not have, and the modal is the only place that is visible.
  const h = harness();
  h.addDependency("/w/dsl/demo/specs.memql", "spec isActive { }\n", [
    construct("isActive", "untrained"),
  ]);
  h.addDependency("/w/dsl/common/traits.memql", "trait onCluster { }\n", [
    construct("onCluster", "trained"),
  ]);

  const outcome = await h.actions.promote(REQUEST);
  assert.equal(outcome.status, "ok");

  const first = h.confirms[0];
  assert.ok(first !== undefined, "a promote always confirms");
  assert.deepEqual(first.callsSoFar, [], "and confirms before touching the engine at all");
  assert.ok(first.prompt.detail.includes("dsl/demo/specs.memql"), "the untrained dependency is named");
  assert.ok(first.prompt.detail.includes("isActive"), "and so is the construct that put it there");
  assert.equal(
    first.prompt.detail.includes("dsl/common/traits.memql"),
    false,
    "a dependency the cluster already has is not in what is being committed",
  );
  assert.ok(first.prompt.detail.includes("dependency file(s) the cluster does not have"));
});

test("a promote validates before promoting, and refreshes the catalog after", async () => {
  const h = harness();
  const outcome = await h.actions.promote(REQUEST);
  assert.equal(outcome.status, "ok");
  assert.deepEqual(h.engine.ops(), ["validate", "promote"]);
  assert.deepEqual(h.engine.calls.filter((c) => c.op === "promote"), [
    { op: "promote", sources: h.engine.calls[0]!.sources, allowBreaking: false },
  ]);
  assert.equal(h.catalogRefreshes, 1);
});

test("a promote against a non-local cluster says so in the confirmation", async () => {
  // Browsing a remote cluster is not a quieter way to write to it. memql#3309
  // set that for runs; a promote is persisted, shared and replayed on restart.
  const h = harness();
  h.setCluster(STAGING);
  await h.actions.promote(REQUEST);
  assert.ok(h.confirms[0]?.prompt.detail.includes("not marked local"));
  assert.ok(h.confirms[0]?.prompt.message.includes("staging"));
});

test("declining a promote promotes nothing", async () => {
  const h = harness();
  h.answerConfirm(false);
  const outcome = await h.actions.promote(REQUEST);
  assert.equal(outcome.status, "declined");
  assert.deepEqual(h.engine.ops(), []);
  assert.deepEqual(h.engine.registry.promoted, []);
  assert.equal(h.catalogRefreshes, 0);
});

test("the engine's owner-only refusal is surfaced, not paraphrased", async () => {
  // Nothing here checks a role. The engine enforces and names what it required;
  // the editor's whole job is to put that sentence in front of the developer.
  const h = harness();
  h.engine.promoteResult = {
    ok: false,
    promoted: [],
    diagnostics: [],
    error: 'authoring: durable promote requires the cluster owner role; caller holds "writer"',
    conceptDiffs: [],
  };

  const outcome = await h.actions.promote(REQUEST);
  assert.equal(outcome.status, "error");
  assert.ok(
    outcome.status === "error" && outcome.message.includes('caller holds "writer"'),
    "the engine's own words reach the developer",
  );
  assert.equal(h.catalogRefreshes, 0, "a refusal changed nothing, so nothing is refreshed");
});

// -----------------------------------------------------------------------------
// The breaking-change override
// -----------------------------------------------------------------------------

test("a breaking concept change comes back as a diff, not as an error", async () => {
  const h = harness();
  h.engine.promoteResult = {
    ok: false,
    promoted: [],
    diagnostics: [],
    error: "authoring: refusing a breaking concept schema change",
    conceptDiffs: [breakingDiff()],
  };

  const outcome = await h.actions.promote(REQUEST);
  assert.equal(outcome.status, "breaking");
  assert.equal(
    h.engine.calls.find((c) => c.op === "promote")?.op === "promote" &&
      (h.engine.calls.find((c) => c.op === "promote") as { allowBreaking: boolean }).allowBreaking,
    false,
    "the first attempt never carries the override",
  );
});

test("the breaking report renders the classified diff, not the raw error string", async () => {
  // The acceptance criterion. The engine returns both a prose refusal and a
  // classification; rendering the prose is easier and useless, because the field
  // name, the row count and the referencing constructs are what a developer acts
  // on.
  const h = harness();
  h.engine.promoteResult = {
    ok: false,
    promoted: [],
    diagnostics: [],
    error: "authoring: refusing a breaking concept schema change",
    conceptDiffs: [breakingDiff()],
  };

  const outcome = await h.actions.promote(REQUEST);
  const report = outcomeReport(outcome);
  assert.ok(report !== undefined);
  assert.equal(report.severity, "warning", "a refusal is not an error -- it is an answer");
  assert.ok(report.body.includes("v1:demo:widget"), "the concept");
  assert.ok(report.body.includes("field_removed colour"), "the change and the field");
  assert.ok(report.body.includes("string -> (absent)"), "both sides of it");
  assert.ok(report.body.includes("rows affected: 1204"), "the count that decided it");
  assert.ok(report.body.includes("query:widgetsByColour"), "what still references it");
  assert.ok(report.headline.toLowerCase().includes("override"), "and that an override exists");
});

test("an unknown row count is never rendered as zero", async () => {
  // A node with no database cannot count, and its zero would be a claim it is
  // not entitled to make -- one that reads as "this is safe" at exactly the
  // moment nothing is known.
  const diff = breakingDiff();
  diff.changes[0]!.rowsAffected = 0;
  diff.changes[0]!.rowCountKnown = false;

  const h = harness();
  h.engine.promoteResult = {
    ok: false,
    promoted: [],
    diagnostics: [],
    error: "refused",
    conceptDiffs: [diff],
  };

  const report = outcomeReport(await h.actions.promote(REQUEST));
  assert.ok(report!.body.includes("not counted"));
  assert.equal(report!.body.includes("rows affected: 0"), false);
});

test("the override reaches the engine only after the OVERRIDE channel says yes", async () => {
  const h = harness();
  h.engine.promoteResult = {
    ok: false,
    promoted: [],
    diagnostics: [],
    error: "refused",
    conceptDiffs: [breakingDiff()],
  };

  const refused = await h.actions.promote(REQUEST);
  assert.equal(refused.status, "breaking");
  if (refused.status !== "breaking") return;

  h.engine.promoteResult = {
    ok: true,
    promoted: [{ kind: "concept", name: "widget" }],
    diagnostics: [],
    error: "",
    conceptDiffs: [breakingDiff({ overridden: true })],
  };
  const confirmsBefore = h.confirms.length;
  const outcome = await h.actions.promoteWithOverride(REQUEST, refused.cluster, refused.bundle, refused.diffs);

  assert.equal(outcome.status, "ok");
  assert.equal(h.overrides.length, 1, "asked through the override channel");
  assert.equal(h.confirms.length, confirmsBefore, "and never through the ordinary one");
  const second = h.engine.calls.filter((c) => c.op === "promote").at(-1);
  assert.equal(second?.op === "promote" && second.allowBreaking, true);
  assert.equal(
    second?.op === "promote" && second.sources,
    refused.bundle.sources,
    "the bytes promoted are the bytes the diff described, not a re-read of the buffer",
  );
});

test("declining the override promotes nothing", async () => {
  const h = harness();
  h.engine.promoteResult = {
    ok: false,
    promoted: [],
    diagnostics: [],
    error: "refused",
    conceptDiffs: [breakingDiff()],
  };
  const refused = await h.actions.promote(REQUEST);
  if (refused.status !== "breaking") throw new Error("expected a breaking refusal");

  h.answerOverride(false);
  const outcome = await h.actions.promoteWithOverride(REQUEST, refused.cluster, refused.bundle, refused.diffs);

  assert.equal(outcome.status, "declined");
  assert.equal(
    h.engine.calls.filter((c) => c.op === "promote").length,
    1,
    "no second attempt was made",
  );
});

test("an override cannot be applied to a cluster other than the one that refused", async () => {
  // The one thing the supersession guard cannot cover, because the override is a
  // new action rather than a continuation of the refused one. Everything else on
  // this path exists to make the override apply to exactly what was shown; a
  // cluster switch in between would leave the bytes right and the destination
  // wrong.
  const h = harness();
  h.engine.promoteResult = {
    ok: false,
    promoted: [],
    diagnostics: [],
    error: "refused",
    conceptDiffs: [breakingDiff()],
  };
  const refused = await h.actions.promote(REQUEST);
  if (refused.status !== "breaking") throw new Error("expected a breaking refusal");

  h.setCluster(STAGING);
  const outcome = await h.actions.promoteWithOverride(
    REQUEST,
    refused.cluster,
    refused.bundle,
    refused.diffs,
  );

  assert.equal(outcome.status, "error");
  assert.ok(outcome.status === "error" && outcome.message.includes("staging"));
  assert.deepEqual(h.overrides, [], "and it is refused before anybody is asked to override");
  assert.equal(h.engine.calls.filter((c) => c.op === "promote").length, 1);
});

test("the override prompt names the field it breaks and says the engine audits it", async () => {
  const h = harness();
  h.engine.promoteResult = {
    ok: false,
    promoted: [],
    diagnostics: [],
    error: "refused",
    conceptDiffs: [breakingDiff()],
  };
  const refused = await h.actions.promote(REQUEST);
  if (refused.status !== "breaking") throw new Error("expected a breaking refusal");

  await h.actions.promoteWithOverride(REQUEST, refused.cluster, refused.bundle, refused.diffs);

  const prompt = h.overrides[0];
  assert.ok(prompt !== undefined);
  assert.ok(prompt.confirmLabel.includes("colour"), "the button names the consequence");
  assert.ok(prompt.detail.includes("audits"), "and the modal says the override is recorded");
});

test("a successful override is reported as one", async () => {
  const h = harness();
  h.engine.promoteResult = {
    ok: false,
    promoted: [],
    diagnostics: [],
    error: "refused",
    conceptDiffs: [breakingDiff()],
  };
  const refused = await h.actions.promote(REQUEST);
  if (refused.status !== "breaking") throw new Error("expected a breaking refusal");

  h.engine.promoteResult = {
    ok: true,
    promoted: [{ kind: "concept", name: "widget" }],
    diagnostics: [],
    error: "",
    conceptDiffs: [breakingDiff({ overridden: true })],
  };
  const report = outcomeReport(
    await h.actions.promoteWithOverride(REQUEST, refused.cluster, refused.bundle, refused.diffs),
  );

  assert.equal(report?.severity, "warning", "a promote that broke something is not routine");
  assert.ok(report!.headline.includes("override"));
  assert.ok(report!.body.includes("auditEvent"));
});

// -----------------------------------------------------------------------------
// Demote
// -----------------------------------------------------------------------------

// -----------------------------------------------------------------------------
// Stage (epic memql#3928)
// -----------------------------------------------------------------------------

test("a stage validates first, submits the CLOSURE, and refreshes the catalog", async () => {
  // The closure, not the construct alone: a staged construct still has to bind,
  // and a dependency the cluster does not serve to this author would leave it
  // compiling and then failing to resolve.
  const h = harness();
  const outcome = await h.actions.stage(REQUEST);
  assert.equal(outcome.status, "ok");
  assert.deepEqual(h.engine.ops(), ["validate", "stage"]);
  assert.equal(h.catalogRefreshes, 1);
});

test("a stage puts the construct in the author's tier and NOT in the shared one", async () => {
  // Asserted against state rather than a call log, for the dry-run test's
  // reason. The whole difference between this tier and a promote is which of
  // these two lists moves.
  const h = harness();
  await h.actions.stage(REQUEST);
  assert.deepEqual(h.engine.registry.staged, ["spaceParticipants"]);
  assert.deepEqual(h.engine.registry.promoted, [], "staging must not make anything shared");
  assert.deepEqual(h.engine.registry.sessionDefined, []);
});

test("the stage confirmation says who can call it, which is the whole distinction", () => {
  // Durability is most of what a developer reads "this is real now" from, and
  // staging has it. The sentence that has to land is the other half.
  return (async () => {
    const h = harness();
    await h.actions.stage(REQUEST);
    const detail = h.confirms[0]?.prompt.detail ?? "";
    assert.match(detail, /callable BY YOU AND BY NOBODY ELSE/);
    assert.match(detail, /concept cannot be staged/);
    assert.equal(
      /owner-only/.test(detail),
      false,
      "staging takes the authoring bar, so promising an owner-only refusal would be wrong",
    );
  })();
});

test("declining a stage stages nothing", async () => {
  const h = harness();
  h.answerConfirm(false);
  const outcome = await h.actions.stage(REQUEST);
  assert.equal(outcome.status, "declined");
  assert.deepEqual(h.engine.ops(), []);
  assert.deepEqual(h.engine.registry.staged, []);
  assert.equal(h.catalogRefreshes, 0);
});

test("the engine's concept refusal is surfaced, not paraphrased", async () => {
  // The engine names the concept and says to train it instead. That is a better
  // sentence than anything this layer could reconstruct from a status code.
  const h = harness();
  h.engine.stageResult = {
    ok: false,
    staged: [],
    diagnostics: [],
    error:
      'authoring: durable stage of concept "order" is not supported: ... train the concept (durable promote) and stage the constructs bound to it',
  };
  const outcome = await h.actions.stage(REQUEST);
  assert.equal(outcome.status, "error");
  if (outcome.status !== "error") return;
  assert.match(outcome.message, /concept "order"/);
  assert.deepEqual(h.engine.registry.staged, []);
});

test("a staged dependency joins the closure, because the cluster serves it to nobody else", async () => {
  // The asymmetry with `trained` is the point: a promote landing a shared
  // construct bound to a private one compiles on its author's session and
  // resolves on no other.
  const h = harness();
  h.addDependency("/w/dsl/demo/specs.memql", "spec isActive { }\n", [
    construct("isActive", "staged"),
  ]);
  await h.actions.stage(REQUEST);
  assert.ok(
    h.confirms[0]?.prompt.detail.includes("dsl/demo/specs.memql"),
    "a staged dependency is carried, not assumed present",
  );
});

test("a demote submits the construct alone and never compiles it", async () => {
  const h = harness();
  const outcome = await h.actions.demote(REQUEST);

  assert.equal(outcome.status, "ok");
  // No validate. The engine demotes by name, so a construct whose source no
  // longer compiles can still be withdrawn -- which is exactly the case a demote
  // exists to clean up.
  assert.deepEqual(h.engine.ops(), ["demote"]);
  const sources = h.engine.calls[0]!.sources;
  assert.ok(sources.includes("query spaceParticipants"));
  assert.equal(h.catalogRefreshes, 1);
});

test("a demote confirmation states the retire-or-remove rule without predicting it", async () => {
  // Only the engine can count the rows. A prediction made here would be a claim
  // about data this process has not read.
  const h = harness();
  await h.actions.demote(REQUEST);
  const detail = h.confirms[0]?.prompt.detail ?? "";
  assert.ok(detail.includes("RETIRED"));
  assert.ok(detail.includes("Only this construct"));
});

test("a retired concept is reported with the row count that decided it", async () => {
  const h = harness();
  h.engine.demoteResult = {
    ok: true,
    demoted: [{ kind: "concept", name: "widget" }],
    outcomes: [
      {
        kind: "concept",
        name: "widget",
        conceptId: "v1:demo:widget",
        outcome: "retired",
        rowCount: 1204,
      },
    ],
    diagnostics: [],
    error: "",
  };

  const report = outcomeReport(await h.actions.demote(REQUEST));
  assert.ok(report!.body.includes("retired"));
  assert.ok(report!.body.includes("1204 row(s)"));
  assert.ok(report!.body.includes("v1:demo:widget"));
  assert.ok(report!.headline.includes("retired"), "which outcome happened is the headline");
});

test("a removed construct is reported as its name being free again", async () => {
  const h = harness();
  const report = outcomeReport(await h.actions.demote(REQUEST));
  assert.ok(report!.body.includes("removed"));
  assert.ok(report!.body.includes("claimable again"));
});

test("an outcome value this build has never heard of is printed, not rewritten", async () => {
  const h = harness();
  h.engine.demoteResult = {
    ok: true,
    demoted: [{ kind: "query", name: "spaceParticipants" }],
    outcomes: [
      {
        kind: "query",
        name: "spaceParticipants",
        conceptId: "",
        outcome: "quarantined",
        rowCount: 0,
      },
    ],
    diagnostics: [],
    error: "",
  };

  const report = outcomeReport(await h.actions.demote(REQUEST));
  assert.ok(report!.body.includes("quarantined"), "a fact about the cluster is not ours to hide");
});

// -----------------------------------------------------------------------------
// Preflight and supersession
// -----------------------------------------------------------------------------

test("every action refuses before touching the engine when nothing is connected", async () => {
  const h = harness();
  h.setConnected(false);
  for (const outcome of [
    await h.actions.dryRun(REQUEST),
    await h.actions.tryInSession(REQUEST),
    await h.actions.promote(REQUEST),
    await h.actions.demote(REQUEST),
  ]) {
    assert.equal(outcome.status, "error");
    assert.ok(outcome.status === "error" && outcome.message.includes("Not connected"));
  }
  assert.deepEqual(h.engine.ops(), []);
});

test("a second action supersedes the first, which then publishes nothing", async () => {
  // The modal is the longest window in the extension for the world to move on.
  // A superseded action landing its diagnostics over a newer one's is the defect
  // the one guard exists to prevent.
  const h = harness();
  h.engine.validateResult = {
    ok: false,
    diagnostics: [
      {
        name: "spaceParticipants",
        kind: "query",
        ok: false,
        skipped: false,
        error: "stale",
        line: 1,
        column: 1,
        endLine: 0,
        endColumn: 0,
      },
    ],
  };

  const first = h.actions.dryRun(REQUEST);
  // Begun while the first is still in flight: starting an action IS what makes
  // every earlier one stale.
  const second = h.actions.dryRun(REQUEST);
  const outcomes: TrainingOutcome[] = [await first, await second];

  assert.equal(outcomes[0]?.status, "superseded");
  assert.equal(outcomes[1]?.status, "invalid");
  assert.equal(h.published.length, 1, "only the winner published");
});

test("a construct the buffer no longer declares reports the rename rather than crashing", async () => {
  const h = harness();
  const outcome = await h.actions.demote({ uri: REQUEST.uri, name: "renamedSince" });
  assert.equal(outcome.status, "error");
  assert.ok(outcome.status === "error" && outcome.message.includes("no longer declares"));
});
