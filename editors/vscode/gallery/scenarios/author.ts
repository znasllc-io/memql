// The authoring pages: construct, concept, run form and result, automation
// form and trace, and the language reference -- each in its populated,
// loading or empty, and failed states.
//
// BUILT THE WAY THE PANELS BUILD THEM: the same screen renderer, the same
// panel-local styles, the same pageDocument, so a capture is the page the
// editor shows. The data is REAL-SHAPED -- construct names, concept ids, step
// ids and engine messages of the lengths a person will actually read.

import { viewKitStyles, type ConceptLike } from "@znasllc-io/memql-view-kit";

import type { AutomationRunStep } from "@znasllc-io/memql-sdk-core/automation";

import { bodyThemeAttr } from "../../src/webview/appearance.js";
import { automationFormPlan } from "../../src/state/automationForm.js";
import { buildFields } from "../../src/state/argForm.js";
import type { CatalogConstruct } from "../../src/state/constructCatalog.js";
import {
  clusterLanguage,
  grammarFromRows,
  languageIdentity,
  vocabularyFromRows,
  type LanguagePin,
} from "../../src/state/languageReference.js";
import type { TraceRefusal, TraceStatus } from "../../src/state/stepTrace.js";
import {
  AUTOMATION_PAGE_STYLES,
  automationFormParts,
  traceParts,
  type AutomationFormInput,
  type TraceView,
} from "../../src/webview/automationScreens.js";
import { CONCEPT_PAGE_STYLES, conceptPageParts, type ConceptPageInput } from "../../src/webview/conceptScreens.js";
import {
  CONSTRUCT_PAGE_STYLES,
  constructFailedParts,
  constructLoadingParts,
  constructPageParts,
} from "../../src/webview/constructScreens.js";
import {
  LANGUAGE_REFERENCE_STYLES,
  languageReferenceParts,
  type LanguageReferenceInput,
} from "../../src/webview/languageReferenceScreens.js";
import { RUN_PAGE_STYLES, resultParts, runFormParts, type ResultInput } from "../../src/webview/runScreens.js";
import { pageDocument } from "../../src/webview/ui/document.js";
import type { RegionParts } from "../../src/webview/ui/liveView.js";
import { GALLERY_NONCE, type GalleryTheme, type Scenario } from "../harness.js";

const GROUP = "Authoring";

function scenario(id: string, title: string, styles: string, build: () => RegionParts): Scenario {
  return {
    id,
    group: GROUP,
    title,
    render: (theme: GalleryTheme) =>
      pageDocument({
        nonce: GALLERY_NONCE,
        title,
        themeAttr: bodyThemeAttr(theme),
        screen: id,
        styles,
        ...build(),
      }),
  };
}

const RUN_STYLES = `${viewKitStyles}\n${RUN_PAGE_STYLES}`;
const CONCEPT_STYLES = `${viewKitStyles}\n${CONCEPT_PAGE_STYLES}`;
const AUTOMATION_STYLES = `${viewKitStyles}\n${AUTOMATION_PAGE_STYLES}`;

// ---------------------------------------------------------------------------
// Data
// ---------------------------------------------------------------------------

const QUERY: CatalogConstruct = {
  name: "spaceParticipants",
  kind: "query",
  namespace: "cognition",
  origin: "bundle",
  originPath: "cognition/queries.memql",
  description: "The people and agents in a space, newest first.",
  runnable: true,
  runnableKind: "query",
  args: [
    { name: "spaceId", type: "string", required: true, description: "The space to list." },
    { name: "limit", type: "number", required: false },
    { name: "role", type: "string", required: false, enum: ["human", "agent", "guest"] },
  ],
  boundConcept: "v1:cognition:participant",
  sourceHash: "9f2c41d8a07be3c5",
  source: "",
};

const AUTOMATION: CatalogConstruct = {
  name: "autoJoinSI",
  kind: "automation",
  namespace: "cognition",
  origin: "core",
  originPath: "cognition/automations.memql",
  description: "When a participant joins, bring the space's assistant in.",
  runnable: true,
  runnableKind: "automation",
  args: [],
  boundConcept: "",
  sourceHash: "",
  source: "",
  trigger: { event: "node.created", concept: "v1:cognition:participant" },
};

const CONCEPT_CONSTRUCT: CatalogConstruct = {
  name: "v1:cognition:space",
  kind: "concept",
  namespace: "cognition",
  origin: "bundle",
  originPath: "cognition/concepts.memql",
  description: "A place where people and agents talk.",
  runnable: false,
  args: [],
  boundConcept: "",
  sourceHash: "",
  source: "",
};

const PROMOTED: CatalogConstruct = {
  name: "recentSpaces",
  kind: "query",
  namespace: "",
  origin: "staged",
  originPath: "",
  description: "",
  runnable: true,
  runnableKind: "query",
  args: [],
  boundConcept: "v1:cognition:space",
  sourceHash: "",
  source: `query space recentSpaces {\n  filter  status == "active"\n  sort    "row.createdAt", "desc"\n  shape   spaceCard\n}`,
};

const SPACE: ConceptLike = {
  id: "v1:cognition:space",
  entity: "space",
  displayCard: { primary: "name", secondary: "purpose", status: "status" },
};

const PARTICIPANT: ConceptLike = {
  id: "v1:cognition:participant",
  entity: "participant",
  displayCard: { primary: "displayName", secondary: "role" },
};

const SPACE_ROWS: Record<string, unknown>[] = [
  ["01J8Z2QK5M", "Design review", "Weekly review of the onboarding flow", "active"],
  ["01J8Z2QK6N", "Launch plan", "Everything that has to happen before Friday", "active"],
  ["01J8Z2QK7P", "Customer calls", "Notes from the calls this week", "active"],
  ["01J8Z2QK8Q", "Hiring", "Open roles and the people talking to us", "archived"],
  ["01J8Z2QK9R", "Research", "Papers worth reading", "active"],
  ["01J8Z2QKAS", "Standup", "", "active"],
].map(([id, name, purpose, status]) => ({ id, concept: "v1:cognition:space", name, purpose, status }));

const PARTICIPANT_ROWS: Record<string, unknown>[] = [
  { id: "01J8Z3A1B2", concept: "v1:cognition:participant", displayName: "Alex Kim", role: "human" },
  { id: "01J8Z3A1C3", concept: "v1:cognition:participant", displayName: "Faye", role: "agent" },
  { id: "01J8Z3A1D4", concept: "v1:cognition:participant", displayName: "Sofia", role: "agent" },
];

const CONCEPTS = new Map<string, ConceptLike>([
  [SPACE.id, SPACE],
  [PARTICIPANT.id, PARTICIPANT],
]);

// ---------------------------------------------------------------------------
// Construct page
// ---------------------------------------------------------------------------

const constructScenarios: Scenario[] = [
  scenario("author-construct-query", "Construct: a query with arguments", CONSTRUCT_PAGE_STYLES, () =>
    constructPageParts({ construct: QUERY, cluster: "local", source: "workspace" }),
  ),
  scenario("author-construct-automation", "Construct: an automation", CONSTRUCT_PAGE_STYLES, () =>
    constructPageParts({ construct: AUTOMATION, cluster: "local", source: "cluster", detailsOpen: true }),
  ),
  scenario("author-construct-concept", "Construct: a concept (no run)", CONSTRUCT_PAGE_STYLES, () =>
    constructPageParts({ construct: CONCEPT_CONSTRUCT, cluster: "local", source: "workspace" }),
  ),
  scenario("author-construct-staged", "Construct: staged, with its source on the page", CONSTRUCT_PAGE_STYLES, () =>
    constructPageParts({ construct: PROMOTED, cluster: "local", source: "none" }),
  ),
  scenario("author-construct-loading", "Construct: loading", CONSTRUCT_PAGE_STYLES, () => constructLoadingParts()),
  scenario("author-construct-error", "Construct: the read failed", CONSTRUCT_PAGE_STYLES, () =>
    constructFailedParts("spaceParticipants", "Couldn't read this cluster's constructs."),
  ),
];

// ---------------------------------------------------------------------------
// Concept page
// ---------------------------------------------------------------------------

function concept(over: Partial<ConceptPageInput>): RegionParts {
  return conceptPageParts({
    concept: SPACE,
    connection: "connected",
    rows: SPACE_ROWS,
    settled: true,
    loading: false,
    listError: "",
    more: true,
    selectedRowId: "01J8Z2QK6N",
    detail: {
      state: "found",
      value: {
        id: "01J8Z2QK6N",
        concept: "v1:cognition:space",
        createdAt: "2026-09-27T16:02:11Z",
        createdBy: "01J7USER00",
        payload: { name: "Launch plan", purpose: "Everything that has to happen before Friday", status: "active", memberCount: 4 },
      },
    },
    liveOff: "",
    ...over,
  });
}

const conceptScenarios: Scenario[] = [
  scenario("author-concept-rows", "Concept: rows and the selected row", CONCEPT_STYLES, () => concept({})),
  scenario("author-concept-loading", "Concept: first page loading", CONCEPT_STYLES, () =>
    concept({ rows: [], settled: false, selectedRowId: undefined, detail: { state: "none" } }),
  ),
  scenario("author-concept-empty", "Concept: no rows", CONCEPT_STYLES, () =>
    concept({ rows: [], more: false, selectedRowId: undefined, detail: { state: "none" } }),
  ),
  scenario("author-concept-error", "Concept: the read failed", CONCEPT_STYLES, () =>
    concept({ rows: [], more: false, selectedRowId: undefined, detail: { state: "none" }, listError: "PERMISSION_DENIED: reading v1:cognition:space needs the reader role" }),
  ),
  scenario("author-concept-notlive", "Concept: live updates off", CONCEPT_STYLES, () =>
    concept({ liveOff: "subscriptions are not enabled on this node", selectedRowId: undefined, detail: { state: "none" } }),
  ),
  scenario("author-concept-signin", "Concept: sign-in needed", CONCEPT_STYLES, () =>
    concept({ connection: "signIn", rows: [], settled: false }),
  ),
];

// ---------------------------------------------------------------------------
// Run form and result
// ---------------------------------------------------------------------------

const FORM_ARGS = [
  { name: "spaceId", type: "string" as const, required: true, description: "The space to list." },
  { name: "limit", type: "number" as const, required: false },
  { name: "role", type: "string" as const, required: false, enum: ["human", "agent", "guest"] },
  { name: "includeArchived", type: "boolean" as const, required: true },
  { name: "filter", type: "object" as const, required: false },
];

const runScenarios: Scenario[] = [
  scenario("author-run-form", "Run form", RUN_STYLES, () =>
    runFormParts({
      target: { kind: "query", name: "spaceParticipants" },
      fields: buildFields(FORM_ARGS, { spaceId: "01J8Z2QK6N", includeArchived: false, filter: { role: "agent" } }),
      errors: {},
      orphans: [],
      busy: false,
    }),
  ),
  scenario("author-run-form-errors", "Run form: fields to fix", RUN_STYLES, () =>
    runFormParts({
      target: { kind: "query", name: "spaceParticipants" },
      // As typed: the form holds the text, and these did not parse.
      fields: buildFields(FORM_ARGS, { limit: "ten" }).map((f) => (f.name === "filter" ? { ...f, text: '{ "role": ' } : f)),
      errors: { spaceId: "Required", limit: "Enter a number", filter: "Invalid JSON (line 1)" },
      orphans: ["cursor"],
      busy: false,
    }),
  ),
  scenario("author-run-form-saved", "Run form: running, after a save", RUN_STYLES, () =>
    runFormParts({
      target: { kind: "query", name: "spaceParticipants" },
      fields: buildFields(FORM_ARGS, { spaceId: "01J8Z2QK6N", includeArchived: true }),
      errors: {},
      orphans: [],
      busy: true,
      note: { tone: "info", line: 'Saved as "participants of launch".', openRuns: true },
    }),
  ),
];

function result(input: ResultInput): () => RegionParts {
  return () => resultParts(input);
}

const TARGET = { uri: "file:///w/cognition/queries.memql", kind: "query" as const, name: "spaceParticipants", args: [] };

const resultScenarios: Scenario[] = [
  scenario("author-result-running", "Result: running", RUN_STYLES, result({ state: "running", target: TARGET })),
  scenario(
    "author-result-rows",
    "Result: rows of two concepts",
    RUN_STYLES,
    result({
      state: "settled",
      concepts: CONCEPTS,
      jsonOpen: false,
      outcome: {
        status: "ok",
        target: TARGET,
        rows: [...PARTICIPANT_ROWS, ...SPACE_ROWS.slice(0, 2)],
        raw: {},
        ranDeployedDefinition: false,
        injected: true,
      },
    }),
  ),
  scenario(
    "author-result-empty",
    "Result: no rows",
    RUN_STYLES,
    result({
      state: "settled",
      concepts: CONCEPTS,
      jsonOpen: false,
      outcome: { status: "ok", target: TARGET, rows: [], raw: [], ranDeployedDefinition: true, injected: false },
    }),
  ),
  scenario(
    "author-result-tool",
    "Result: a tool",
    RUN_STYLES,
    result({
      state: "settled",
      concepts: CONCEPTS,
      jsonOpen: false,
      outcome: {
        status: "ok",
        target: { ...TARGET, kind: "tool", name: "searchUsers" },
        rows: [],
        raw: [],
        toolContent: [
          { type: "text", text: "Found 2 users:\n  alex@example.com  (owner)\n  sam@example.com   (writer)", mimeType: "", data: "", uri: "" },
        ],
        ranDeployedDefinition: true,
        injected: false,
      },
    }),
  ),
  scenario(
    "author-result-error",
    "Result: the run failed",
    RUN_STYLES,
    result({
      state: "settled",
      concepts: CONCEPTS,
      jsonOpen: false,
      outcome: {
        status: "error",
        target: TARGET,
        phase: "invoke",
        message: "query spaceParticipants: argument spaceId: no space 01J8Z2QK6X is visible to you",
        errorId: "ERR-7c41a2",
      },
    }),
  ),
  scenario(
    "author-result-invalid",
    "Result: didn't compile",
    RUN_STYLES,
    result({
      state: "settled",
      concepts: CONCEPTS,
      jsonOpen: false,
      outcome: {
        status: "invalid",
        target: TARGET,
        phase: "validate",
        diagnostics: [
          {
            message: "unknown field \"rolee\" on concept participant",
            path: "/Users/alex/src/app/dsl/cognition/queries.memql",
            fileLevel: false,
            start: { line: 41, character: 10 },
            end: { line: 41, character: 15 },
            code: "lower_unknown_field",
            constructName: "spaceParticipants",
            constructKind: "query",
          },
          {
            message: "spec isActiveRecord is not imported",
            path: "/Users/alex/src/app/dsl/cognition/queries.memql",
            fileLevel: false,
            start: { line: 44, character: 2 },
            end: { line: 44, character: 16 },
            code: "",
            constructName: "spaceParticipants",
            constructKind: "query",
          },
        ],
      },
    }),
  ),
];

// ---------------------------------------------------------------------------
// Automation form and trace
// ---------------------------------------------------------------------------

const PLAN = automationFormPlan(AUTOMATION.name, AUTOMATION.trigger);

function automation(over: Partial<AutomationFormInput>): RegionParts {
  return automationFormParts({
    name: AUTOMATION.name,
    trigger: AUTOMATION.trigger,
    plan: PLAN,
    picker: {
      open: true,
      settled: true,
      loading: false,
      rows: PARTICIPANT_ROWS,
      concept: PARTICIPANT,
      selectedRowId: "01J8Z3A1B2",
      more: false,
      error: "",
    },
    payloadText: JSON.stringify(PARTICIPANT_ROWS[0], null, 2),
    payloadError: "",
    targetNodeType: "",
    includeStepOutput: false,
    optionsOpen: false,
    busy: false,
    ...over,
  });
}

const automationScenarios: Scenario[] = [
  scenario("author-automation-form", "Automation: a picked row", AUTOMATION_STYLES, () => automation({ optionsOpen: true })),
  scenario("author-automation-loading", "Automation: rows loading", AUTOMATION_STYLES, () =>
    automation({ picker: { open: true, settled: false, loading: true, rows: [], concept: PARTICIPANT, more: false, error: "" }, payloadText: "" }),
  ),
  scenario("author-automation-empty", "Automation: no rows to pick", AUTOMATION_STYLES, () =>
    automation({ picker: { open: true, settled: true, loading: false, rows: [], concept: PARTICIPANT, more: false, error: "" }, payloadText: "" }),
  ),
  scenario("author-automation-invalid", "Automation: a payload to fix", AUTOMATION_STYLES, () =>
    automation({
      picker: { open: false, settled: true, loading: false, rows: PARTICIPANT_ROWS, concept: PARTICIPANT, more: false, error: "" },
      payloadText: '{ "id": "01J8Z3A1B2",',
      payloadError: "Invalid JSON (line 1)",
    }),
  ),
  scenario("author-automation-schedule", "Automation: on a schedule", AUTOMATION_STYLES, () =>
    automationFormParts({
      name: "reapStaleSessions",
      trigger: { schedule: "0 */10 * * * *" },
      plan: automationFormPlan("reapStaleSessions", { schedule: "0 */10 * * * *" }),
      picker: { open: false, settled: false, loading: false, rows: [], concept: { id: "", entity: "" }, more: false, error: "" },
      payloadText: "",
      payloadError: "",
      targetNodeType: "cognition",
      includeStepOutput: true,
      optionsOpen: true,
      busy: false,
    }),
  ),
];

const ACCEPTED = {
  automation: "autoJoinSI",
  ranDeployedDefinition: true,
  definitionNote: "The deployed version ran. Edits in your editor apply once they are deployed.",
  triggerKind: "event",
  triggerTopic: "graph.node.created.v1:cognition:participant",
  requestedOnNodeId: "bff-7d9f8c6b5-x2kqp",
  requestedOnNodeType: "bff",
  targetNodeType: "",
};

function step(sequence: number, stepId: string, status: string, durationMs: number, extra: Partial<AutomationRunStep> = {}): AutomationRunStep {
  return { sequence, stepId, status, durationMs, error: "", ...extra };
}

function trace(status: TraceStatus, steps: AutomationRunStep[], over: Partial<TraceView> = {}): TraceView {
  return {
    accepted: ACCEPTED,
    complete:
      status === "running" || status === "refused"
        ? undefined
        : {
            status,
            durationMs: 1240,
            stepCount: steps.length,
            error: status === "failed" ? "notifyAssistant: agent Faye is not in space 01J8Z2QK6N" : "",
            executedOnNodeId: "cognition-5c7d9-4hj2m",
            executedOnNodeType: "cognition",
          },
    refusal: undefined,
    error: "",
    runId: "run_01J8Z4C0TR9Q",
    steps,
    status,
    settled: status !== "running",
    ...over,
  };
}

const STEPS = [
  step(1, "loadSpace", "success", 38, { output: { spaceId: "01J8Z2QK6N", name: "Launch plan" } }),
  step(2, "pickAssistant", "success", 412),
  step(3, "notifyAssistant", "failed", 790, { error: "agent Faye is not in space 01J8Z2QK6N" }),
];

const refusal: TraceRefusal = {
  code: 7,
  codeName: "PERMISSION_DENIED",
  message: "operator run requires owner or admin; you are a writer on this cluster",
  runId: "run_01J8Z4C0TR9Q",
};

const traceScenarios: Scenario[] = [
  scenario("author-trace-running", "Trace: running", AUTOMATION_STYLES, () =>
    traceParts({ name: "autoJoinSI", trace: trace("running", STEPS.slice(0, 2)), detailsOpen: false, jsonOpen: false }),
  ),
  scenario("author-trace-succeeded", "Trace: succeeded", AUTOMATION_STYLES, () =>
    traceParts({
      name: "autoJoinSI",
      trace: trace("completed", [...STEPS.slice(0, 2), step(3, "notifyAssistant", "success", 790)]),
      detailsOpen: true,
      jsonOpen: false,
    }),
  ),
  scenario("author-trace-failed", "Trace: failed at a step", AUTOMATION_STYLES, () =>
    traceParts({ name: "autoJoinSI", trace: trace("failed", STEPS), detailsOpen: false, jsonOpen: false }),
  ),
  scenario("author-trace-refused", "Trace: refused", AUTOMATION_STYLES, () =>
    traceParts({ name: "autoJoinSI", trace: trace("refused", [], { refusal }), detailsOpen: false, jsonOpen: false }),
  ),
];

// ---------------------------------------------------------------------------
// Language reference
// ---------------------------------------------------------------------------

const PIN: LanguagePin = { edition: "2026", status: "frozen", grammarVersion: "2026.09-authoring-4f1c2e9a", version: "0.23.5" };

const GRAMMAR = grammarFromRows([
  {
    format: "ebnf",
    edition: "2026",
    grammarVersion: PIN.grammarVersion,
    content: `(* MemQL authoring grammar. Edition 2026, grammar version ${PIN.grammarVersion}.
   Generated from the parser -- do not edit. *)

(* ---- A file ---- *)
<file>                ::= <use>* <declaration>*
<use>                 ::= "use" <path> "." "{" <name> { "," <name> } "}"
<declaration>         ::= <automation> | <concept> | <logic>
                        | <mutation> | <query> | <shape> | <spec>

(* ---- Queries ---- *)
<query>               ::= "query" <concept-name> <name> "{" <query-body> "}"
<query-body>          ::= [ <args> ] "filter" <expr> [ <sort> ] "shape" <name>

(* ---- Expressions ---- *)
<expr-5>              ::= <expr-4> { "??" <expr-4> }
<expr-7>              ::= <expr-6> { "&&" <expr-6> }
<expr-2>              ::= ( "!" | "-" ) <expr-2> | <expr-1>
`,
  },
]);

const VOCABULARY = vocabularyFromRows([
  {
    edition: "2026",
    grammarVersion: PIN.grammarVersion,
    kinds: ["construct", "operator", "function"],
    entries: [
      { kind: "construct", name: "query", signature: "query <Concept> <name> { ... }", description: "Read function: a bound concept, a filter and a projection." },
      { kind: "construct", name: "mutate", signature: "mutate <Concept> <name> { ... }", description: "Write function: one insert or update block." },
      { kind: "operator", name: "??", signature: "a ?? b", description: "Falls through on an absent or whitespace-only value." },
      { kind: "operator", name: "in", signature: "x in list", description: "Membership." },
      { kind: "function", name: "lower", signature: "lower(s)", description: "Lowercases a string.", tier: "P" },
      { kind: "function", name: "coalesceDeep", signature: "coalesceDeep(a, b)", description: "Merges two objects, the first winning.", tier: "M" },
    ],
  },
]);

function language(over: Partial<LanguageReferenceInput>): RegionParts {
  return languageReferenceParts({
    identity: languageIdentity({ pin: PIN, cluster: clusterLanguage("local", { edition: "2026", grammarVersion: PIN.grammarVersion }, GRAMMAR) }),
    pin: PIN,
    grammar: GRAMMAR,
    vocabulary: VOCABULARY,
    loading: false,
    error: "",
    search: "",
    offerSelectCluster: true,
    ...over,
  });
}

const languageScenarios: Scenario[] = [
  scenario("author-language-populated", "Language reference", LANGUAGE_REFERENCE_STYLES, () => language({})),
  scenario("author-language-search", "Language reference: a search", LANGUAGE_REFERENCE_STYLES, () => language({ search: "query" })),
  scenario("author-language-loading", "Language reference: reading", LANGUAGE_REFERENCE_STYLES, () =>
    language({ grammar: undefined, vocabulary: undefined, loading: true }),
  ),
  scenario("author-language-disconnected", "Language reference: no cluster", LANGUAGE_REFERENCE_STYLES, () =>
    language({ identity: languageIdentity({ pin: PIN }), grammar: undefined, vocabulary: undefined }),
  ),
  scenario("author-language-error", "Language reference: nothing came back", LANGUAGE_REFERENCE_STYLES, () =>
    language({ grammar: undefined, vocabulary: undefined, error: "It didn't answer within 20s." }),
  ),
];

export const scenarios: Scenario[] = [
  ...constructScenarios,
  ...conceptScenarios,
  ...runScenarios,
  ...resultScenarios,
  ...automationScenarios,
  ...traceScenarios,
  ...languageScenarios,
];
