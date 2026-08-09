// The install/uninstall STEP GRAPH, read in TypeScript.
//
// This is a deliberate PORT of scripts/install/graph/graph.go, not an
// independent design. The graph documents (scripts/install/graph/install.json
// and uninstall.json) are the single description of what an install does; the
// Go loader reads them for the in-engine path and this reader reads them for
// the editor extension and the CLI harness. Two readers of one document is
// only safe while they refuse the same things, which is why every rule below
// mirrors a rule in graph.go and why installGraph.test.ts re-asserts them
// against the shipped documents.
//
// The three properties the graph exists to carry, restated because they
// constrain this file:
//
//  1. PARALLELISM IS A PROPERTY OF THE DATA. topoOrder returns WAVES. An
//     executor runs a wave concurrently; it never decides what is independent,
//     so it cannot decide wrong.
//  2. EVERY STEP PROVES ITSELF. evaluateVerify reads the capability script's
//     RESULT envelope -- what the script says about the MACHINE -- and never
//     the exit code. The one predicate that reads only the run, scriptOk, is
//     rejected at load time on any step that changed something.
//  3. UNINSTALL IS DERIVABLE. A step that leaves an artifact declares both a
//     `receipt` and a `preExistingPath`, and the second is what stops an
//     uninstall taking back something the operator already had.
//
// Deliberately free of `vscode` imports: cli.ts runs this under plain node,
// where that module does not exist. installGraph.test.ts asserts it.
//
// Refs: #3372 #3369 #3357

import * as fs from "node:fs/promises";
import * as path from "node:path";

// --------------------------------------------------------------------------
// vocabulary
// --------------------------------------------------------------------------

export type GraphKind = "install" | "uninstall";

export const VERIFY_KINDS = ["scriptOk", "resultTrue", "resultFalse", "resultNonEmpty", "resultEquals"] as const;
export type VerifyKind = (typeof VERIFY_KINDS)[number];

export const ELEVATIONS = ["none", "sudo", "user-trust"] as const;
export type Elevation = (typeof ELEVATIONS)[number];

/** The explicit declaration that an artifact cannot pre-exist as a FOREIGN one. */
export const PRE_EXISTING_NONE = "none";

const RESULT_FIELD_RE = /^result\.([A-Za-z][A-Za-z0-9_]*)$/;
const PRE_EXISTING_RE = /^(!?)result\.([A-Za-z][A-Za-z0-9_]*)$/;
const STEP_ID_RE = /^[a-z][A-Za-z0-9]*$/;

/** One step's proof obligation. */
export interface Verify {
  kind: VerifyKind;
  field?: string;
  value?: string;
}

/** One capability-script invocation, plus everything an executor needs. */
export interface Step {
  id: string;
  script: string;
  description: string;
  /** Graph-PINNED flags. Run-specific values are supplied by the executor. */
  params?: Record<string, string>;
  dependsOn?: string[];
  readOnly?: boolean;
  elevation: Elevation;
  /** The artifact class this step leaves behind. Install graphs only. */
  receipt?: string;
  /** `none` | `[!]result.<field>`. Required exactly when `receipt` is set. */
  preExistingPath?: string;
  /** Another install step whose receipt already covers this step's change. */
  containedBy?: string;
  /** The install step this step undoes. Uninstall graphs only. */
  reverses?: string;
  verify: Verify;
}

export interface Graph {
  name: string;
  kind: GraphKind;
  description: string;
  steps: Step[];
}

/** A capability-script result envelope, as far as this reader needs it. */
export interface ResultEnvelope {
  ok?: unknown;
  capability?: unknown;
  changed?: unknown;
  result?: unknown;
  error?: unknown;
}

/** Every refusal from this module. Thrown, never returned as a bare Error. */
export class GraphError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "GraphError";
  }
}

const GRAPH_KEYS = new Set(["name", "kind", "description", "steps"]);
const STEP_KEYS = new Set([
  "id",
  "script",
  "description",
  "params",
  "dependsOn",
  "readOnly",
  "elevation",
  "receipt",
  "preExistingPath",
  "containedBy",
  "reverses",
  "verify",
]);
const VERIFY_KEYS = new Set(["kind", "field", "value"]);

// --------------------------------------------------------------------------
// loading + validation
// --------------------------------------------------------------------------

/** The shipped document for a graph kind, under a repository root. */
export function graphDocumentPath(kind: GraphKind, repoRoot: string): string {
  return path.join(repoRoot, "scripts", "install", "graph", `${kind}.json`);
}

export async function loadGraphFile(file: string): Promise<Graph> {
  let text: string;
  try {
    text = await fs.readFile(file, "utf8");
  } catch (err) {
    throw new GraphError(`${file}: cannot be read: ${(err as Error).message}`);
  }
  return loadGraph(text, file);
}

/**
 * Parses and fully validates a graph document.
 *
 * It refuses anything an executor would otherwise have to guess about: an
 * unknown field, a step with no verify, a scriptOk stamp on a mutating step, a
 * dangling / duplicated / self-referential dependency, an undeclared
 * elevation, a receipt with no pre-existence signal, and any dependency cycle.
 * A graph that loads is a graph an executor can run without re-deriving
 * anything.
 */
export function loadGraph(data: string, source: string): Graph {
  let raw: unknown;
  try {
    raw = JSON.parse(data);
  } catch (err) {
    throw new GraphError(`${source}: ${(err as Error).message}`);
  }
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    throw new GraphError(`${source}: the graph document must be a JSON object`);
  }
  const obj = raw as Record<string, unknown>;

  // An invented or misspelled key must be an error, not a silently ignored
  // intention: "retries": 3 that nothing reads is worse than no retries.
  rejectUnknownKeys(obj, GRAPH_KEYS, `${source}: graph`);

  const name = typeof obj.name === "string" ? obj.name.trim() : "";
  if (name === "") {
    throw new GraphError(`${source}: graph has no name`);
  }
  const kind = obj.kind;
  if (kind !== "install" && kind !== "uninstall") {
    throw new GraphError(`${source}: unknown graph kind ${JSON.stringify(kind)} (want "install" or "uninstall")`);
  }
  if (!Array.isArray(obj.steps) || obj.steps.length === 0) {
    throw new GraphError(`${source}: graph has no steps`);
  }

  const graph: Graph = {
    name,
    kind,
    description: typeof obj.description === "string" ? obj.description : "",
    steps: obj.steps.map((s, i) => parseStep(s, i, source)),
  };

  const index = new Map<string, Step>();
  for (const s of graph.steps) {
    if (index.has(s.id)) {
      throw new GraphError(`${source}: duplicate step id ${JSON.stringify(s.id)}`);
    }
    index.set(s.id, s);
  }
  for (const s of graph.steps) {
    validateStep(graph, index, s, source);
  }

  // Cycles are found here rather than left for the executor to discover as a
  // hang: topoOrder is the same walk, so a graph that loads always orders.
  topoOrder(graph);
  return graph;
}

function rejectUnknownKeys(obj: Record<string, unknown>, allowed: Set<string>, where: string): void {
  for (const key of Object.keys(obj)) {
    if (!allowed.has(key)) {
      throw new GraphError(
        `${where}: unknown field ${JSON.stringify(key)} -- a key nothing reads is worse than an absent one, ` +
          `because it looks like an intention that took effect`,
      );
    }
  }
}

function parseStep(value: unknown, i: number, source: string): Step {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new GraphError(`${source}: step ${i} is not an object`);
  }
  const obj = value as Record<string, unknown>;
  rejectUnknownKeys(obj, STEP_KEYS, `${source}: step ${i}`);

  const id = typeof obj.id === "string" ? obj.id : "";
  if (!STEP_ID_RE.test(id)) {
    throw new GraphError(`${source}: step ${i} has an invalid id ${JSON.stringify(obj.id)} (want lowerCamelCase)`);
  }

  const step: Step = {
    id,
    script: typeof obj.script === "string" ? obj.script : "",
    description: typeof obj.description === "string" ? obj.description : "",
    elevation: obj.elevation as Elevation,
    verify: parseVerify(obj.verify, `${source}: step ${JSON.stringify(id)}`),
  };
  if (obj.params !== undefined) step.params = parseParams(obj.params, `${source}: step ${JSON.stringify(id)}`);
  if (obj.dependsOn !== undefined) step.dependsOn = parseStringArray(obj.dependsOn, `${source}: step ${JSON.stringify(id)}: dependsOn`);
  if (obj.readOnly !== undefined) {
    if (typeof obj.readOnly !== "boolean") {
      throw new GraphError(`${source}: step ${JSON.stringify(id)}: readOnly must be a boolean`);
    }
    step.readOnly = obj.readOnly;
  }
  for (const key of ["receipt", "preExistingPath", "containedBy", "reverses"] as const) {
    const v = obj[key];
    if (v === undefined) continue;
    if (typeof v !== "string") {
      throw new GraphError(`${source}: step ${JSON.stringify(id)}: ${key} must be a string`);
    }
    step[key] = v;
  }
  return step;
}

function parseVerify(value: unknown, where: string): Verify {
  if (value === undefined) {
    throw new GraphError(
      `${where} declares no verify -- a step that proves nothing reports success on an exit code, ` +
        `and the next step builds on that`,
    );
  }
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new GraphError(`${where}: verify must be an object`);
  }
  const obj = value as Record<string, unknown>;
  rejectUnknownKeys(obj, VERIFY_KEYS, `${where}: verify`);
  const verify: Verify = { kind: obj.kind as VerifyKind };
  if (obj.field !== undefined) {
    if (typeof obj.field !== "string") throw new GraphError(`${where}: verify field must be a string`);
    verify.field = obj.field;
  }
  if (obj.value !== undefined) {
    if (typeof obj.value !== "string") throw new GraphError(`${where}: verify value must be a string`);
    verify.value = obj.value;
  }
  return verify;
}

function parseParams(value: unknown, where: string): Record<string, string> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new GraphError(`${where}: params must be an object of string values`);
  }
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
    if (typeof v !== "string") {
      throw new GraphError(`${where}: param ${JSON.stringify(k)} must be a string -- it becomes an argv flag value`);
    }
    out[k] = v;
  }
  return out;
}

function parseStringArray(value: unknown, where: string): string[] {
  if (!Array.isArray(value) || value.some((v) => typeof v !== "string")) {
    throw new GraphError(`${where} must be an array of step ids`);
  }
  return value as string[];
}

function validateStep(graph: Graph, index: Map<string, Step>, s: Step, source: string): void {
  const where = `${source}: step ${JSON.stringify(s.id)}`;

  if (s.script.trim() === "") {
    throw new GraphError(`${where} names no script`);
  }
  if (s.description.trim() === "") {
    throw new GraphError(`${where} has no description -- an operator has to be told what is about to happen`);
  }
  if (!(ELEVATIONS as readonly string[]).includes(s.elevation)) {
    const what = s.elevation === undefined ? "declares no elevation" : `declares an unknown elevation ${JSON.stringify(s.elevation)}`;
    throw new GraphError(
      `${where} ${what} -- privilege is declared, never discovered ` +
        `(want ${ELEVATIONS.map((e) => JSON.stringify(e)).join(", ")})`,
    );
  }

  for (const d of s.dependsOn ?? []) {
    if (d === s.id) throw new GraphError(`${where} depends on itself`);
    if (!index.has(d)) throw new GraphError(`${where} depends on ${JSON.stringify(d)}, which is not a step in this graph`);
  }

  // -- receipt / pre-existence ------------------------------------------
  if (graph.kind === "install") {
    if (s.reverses) {
      throw new GraphError(`${where} sets reverses -- that belongs on an uninstall graph, an install step undoes nothing`);
    }
  } else {
    if (s.receipt) {
      throw new GraphError(`${where} sets receipt -- an uninstall step removes artifacts, it does not leave them behind`);
    }
    if (s.containedBy) {
      throw new GraphError(`${where} sets containedBy -- that records where an INSTALL step's change gets taken back`);
    }
  }
  if (s.containedBy) {
    if (s.receipt) {
      throw new GraphError(
        `${where} sets both receipt and containedBy -- a change is taken back by its own artifact or by another's, not both`,
      );
    }
    if (s.containedBy === s.id) throw new GraphError(`${where} is containedBy itself`);
    if (!index.has(s.containedBy)) {
      throw new GraphError(`${where} is containedBy ${JSON.stringify(s.containedBy)}, which is not a step in this graph`);
    }
    if (s.readOnly) {
      throw new GraphError(
        `${where} is readOnly and containedBy ${JSON.stringify(s.containedBy)} -- ` +
          `a step that changes nothing needs nothing taken back`,
      );
    }
  }
  if (s.receipt && !s.preExistingPath) {
    throw new GraphError(
      `${where} writes the ${JSON.stringify(s.receipt)} receipt but declares no preExistingPath -- ` +
        `uninstall could not tell an artifact we created from one that was already here`,
    );
  }
  if (!s.receipt && s.preExistingPath) {
    throw new GraphError(`${where} declares a preExistingPath but writes no receipt -- nothing would ever read it`);
  }
  if (s.preExistingPath && s.preExistingPath !== PRE_EXISTING_NONE && !PRE_EXISTING_RE.test(s.preExistingPath)) {
    throw new GraphError(
      `${where} has a malformed preExistingPath ${JSON.stringify(s.preExistingPath)} ` +
        `(want "${PRE_EXISTING_NONE}" or [!]result.<field>)`,
    );
  }

  // -- verify ------------------------------------------------------------
  const v = s.verify;
  if (v.kind === "scriptOk") {
    if (!s.readOnly) {
      throw new GraphError(
        `${where} verifies with scriptOk but is not readOnly -- scriptOk reads only the exit code, ` +
          `which is a rubber stamp on a step that changed the machine; verify a result field instead`,
      );
    }
    if (v.field !== undefined || v.value !== undefined) {
      throw new GraphError(`${where}: scriptOk takes no field and no value`);
    }
    return;
  }
  if (!(VERIFY_KINDS as readonly string[]).includes(v.kind)) {
    throw new GraphError(`${where}: unknown verify kind ${JSON.stringify(v.kind)} (want ${VERIFY_KINDS.join(", ")})`);
  }
  if (!v.field) {
    throw new GraphError(`${where}: ${v.kind} requires a field`);
  }
  if (!RESULT_FIELD_RE.test(v.field)) {
    throw new GraphError(
      `${where}: verify field ${JSON.stringify(v.field)} must be spelled result.<name> -- ` +
        `the envelope's own bookkeeping (ok/changed/capability) describes the run, result{} describes the machine`,
    );
  }
  if (v.kind === "resultEquals") {
    if (v.value === undefined || v.value === "") {
      throw new GraphError(`${where}: resultEquals requires a value`);
    }
  } else if (v.value !== undefined && v.value !== "") {
    throw new GraphError(`${where}: ${v.kind} takes no value (got ${JSON.stringify(v.value)})`);
  }
}

// --------------------------------------------------------------------------
// accessors
// --------------------------------------------------------------------------

export function stepById(graph: Graph, id: string | undefined): Step | undefined {
  if (!id) return undefined;
  return graph.steps.find((s) => s.id === id);
}

export function stepIds(graph: Graph): string[] {
  return graph.steps.map((s) => s.id);
}

/**
 * Decodes a step's pre-existence declaration into the result field to read and
 * whether its meaning is inverted. null when the step declares "none" or
 * nothing at all -- there is no field to consult.
 */
export function stepPreExisting(step: Step): { field: string; negate: boolean } | null {
  const p = step?.preExistingPath;
  if (!p || p === PRE_EXISTING_NONE) return null;
  const m = PRE_EXISTING_RE.exec(p);
  if (!m) return null;
  return { field: m[2]!, negate: m[1] === "!" };
}

/**
 * Resolves "did this artifact already exist before we ran" against a result
 * envelope.
 *
 * DELIBERATELY FAIL-SAFE. When the step declares a field and the envelope does
 * not carry it, this returns TRUE -- "it was already here". The two mistakes
 * are not symmetric: claiming an artifact pre-existed leaves a stale binary
 * behind, while claiming we created one lets uninstall delete a k3d cluster
 * the developer set up themselves. A signal we cannot read is not a licence.
 */
export function resolvePreExisting(step: Step, envelope: ResultEnvelope): boolean {
  const decl = stepPreExisting(step);
  if (!decl) return false;
  const result = resultObject(envelope);
  if (!(decl.field in result)) return true;
  const value = truthy(result[decl.field]);
  return decl.negate ? !value : value;
}

/** The bare result-envelope field this step's predicate reads, or "" for scriptOk. */
export function verifyField(step: Step): string {
  const m = RESULT_FIELD_RE.exec(step?.verify?.field ?? "");
  return m ? m[1]! : "";
}

// --------------------------------------------------------------------------
// evaluation
// --------------------------------------------------------------------------

/**
 * Applies a predicate to a parsed capability-script result envelope.
 *
 * THE ASSERTION THIS FUNCTION IS: it reads the RESULT, never the exit code. A
 * capability script that exits 0 without doing its job is the failure the
 * verify vocabulary exists to catch, so the envelope's own `ok` is a
 * precondition and the evidence is always a field of `result{}`. The single
 * exception, scriptOk, is rejected at load time on any step that changed the
 * machine.
 *
 * Keeping the vocabulary executable HERE is what stops each executor from
 * inventing its own reading of the same graph.
 */
export function evaluateVerify(verify: Verify, envelope: ResultEnvelope): boolean {
  if (!truthy(envelope?.ok)) return false;
  if (verify.kind === "scriptOk") return true;

  const m = RESULT_FIELD_RE.exec(verify.field ?? "");
  if (!m) {
    throw new GraphError(`verify field ${JSON.stringify(verify.field)} is not spelled result.<name>`);
  }
  const result = resultObject(envelope);
  const field = m[1]!;
  if (!(field in result)) return false;
  const value = result[field];

  switch (verify.kind) {
    case "resultTrue":
      return truthy(value);
    case "resultFalse":
      return !truthy(value);
    case "resultNonEmpty":
      return nonEmpty(value);
    case "resultEquals":
      return stringify(value) === (verify.value ?? "");
    default:
      throw new GraphError(`unknown verify kind ${JSON.stringify(verify.kind)}`);
  }
}

function resultObject(envelope: ResultEnvelope): Record<string, unknown> {
  const r = envelope?.result;
  if (r !== null && typeof r === "object" && !Array.isArray(r)) return r as Record<string, unknown>;
  return {};
}

/**
 * Reads a JSON value as a boolean the way the capability scripts write them:
 * real booleans, and the strings a shell emits for them. The accepted spellings
 * are exactly Go's strconv.ParseBool, because graph.go reads the same envelopes
 * and a value one reader calls true must not be false to the other.
 */
export function truthy(v: unknown): boolean {
  if (typeof v === "boolean") return v;
  if (typeof v === "number") return v !== 0;
  if (typeof v === "string") {
    switch (v.trim()) {
      case "1":
      case "t":
      case "T":
      case "TRUE":
      case "true":
      case "True":
        return true;
      default:
        return false;
    }
  }
  return false;
}

/** Whether a value carries content. */
function nonEmpty(v: unknown): boolean {
  if (v === null || v === undefined) return false;
  if (typeof v === "string") return v.trim() !== "";
  if (typeof v === "number") return v !== 0;
  if (typeof v === "boolean") return v;
  if (Array.isArray(v)) return v.length > 0;
  if (typeof v === "object") return Object.keys(v as object).length > 0;
  return true;
}

/** Renders a JSON scalar the way resultEquals compares it. */
function stringify(v: unknown): string {
  if (v === null || v === undefined) return "";
  if (typeof v === "string") return v;
  if (typeof v === "boolean") return v ? "true" : "false";
  if (typeof v === "number") return String(v);
  return JSON.stringify(v);
}

// --------------------------------------------------------------------------
// ordering
// --------------------------------------------------------------------------

/**
 * Groups the graph's steps into dependency WAVES: every id in waves[0] has no
 * dependencies, and every id in waves[i] depends only on steps in earlier
 * waves. An executor runs a wave concurrently, waits for all of it, and moves
 * on.
 *
 * Returning waves rather than a flat topological list is the whole design
 * choice: it makes "these three tool installs are independent" a fact stated
 * by the graph document instead of a judgement each executor re-derives (and
 * one of them gets wrong, serialising a two-minute install into six).
 *
 * Ids within a wave are sorted so the order is deterministic across runs.
 */
export function topoOrder(graph: Graph): string[][] {
  const remaining = new Map<string, Set<string>>();
  for (const s of graph.steps) {
    remaining.set(s.id, new Set(s.dependsOn ?? []));
  }

  const waves: string[][] = [];
  const done = new Set<string>();
  while (remaining.size > 0) {
    const wave: string[] = [];
    for (const [id, deps] of remaining) {
      let ready = true;
      for (const d of deps) {
        if (!done.has(d)) {
          ready = false;
          break;
        }
      }
      if (ready) wave.push(id);
    }
    if (wave.length === 0) {
      const stuck = [...remaining.keys()].sort();
      throw new GraphError(`dependency cycle among steps: ${stuck.join(", ")}`);
    }
    wave.sort();
    for (const id of wave) {
      remaining.delete(id);
      done.add(id);
    }
    waves.push(wave);
  }
  return waves;
}
