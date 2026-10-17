import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { flatten } from "../../../kit/rows";
import { formatBytes } from "../../../kit/format";
import type { LabelMap } from "../labels";

// What ONE machine runs, and what is being pulled onto it (epic memql#5103).
//
// ===========================================================================
// THE MACHINE'S OWN LABELS ARE THE SOURCE, NOT THE FLEET CATALOG
// ===========================================================================
// `fleetModels` answers "what can this caller run", folded across every
// machine. This surface asks the opposite question -- "what does THIS machine
// run" -- and the fold cannot answer it: a model both machines offer appears
// once, with the union of their capabilities, so reading a machine's row out
// of the catalog would attribute a sibling's structured-output support to a
// machine that never claimed it.
//
// The machine's own `model:<id>` labels are the machine's own claim, and they
// are already on the registration row this panel renders. So this is a
// projection, not a read: no query, no loading state, and it moves with the
// heartbeat that carries the labels.
//
// ===========================================================================
// PURE, AND SEPARATE FROM THE COMPONENT
// ===========================================================================
// The same reason rows.ts states: a projection asserted through render() is
// asserted through three layers that can each fail for unrelated reasons.
// Everything here is a function of a label map or a row.

/** One model a machine advertises. */
export interface MachineModel {
  modelId: string;
  /** Parameter count as the runtime reported it, or 0 when it did not say.
   *  Zero is "not reported" and is never rendered as a number -- printing it
   *  would make the unmeasured model look like the smallest one. */
  params: number;
  /** Quantization level (Q4_K_M, F16), or "" when unreported. Operator-facing
   *  only: nothing selects on it. */
  quant: string;
  /** Largest context window in tokens, or 0 when unreported. */
  contextWindow: number;
  structuredOutput: boolean;
  tools: boolean;
  embeddings: boolean;
  vision: boolean;
  audioIn: boolean;
  audioOut: boolean;
  imageGen: boolean;
  /** The machine's per-model concurrency ceiling; 0 means it declared none,
   *  which the load ordering reads as unlimited. */
  maxConcurrent: number;
}

const MODEL_PREFIX = "model:";
const RUNTIME_PREFIX = "runtime:";

/**
 * Parse one `model:<id>` label VALUE.
 *
 * The grammar is the engine's own (`ModelAttributes` in
 * component/worker/fleetcatalog/model.go), and the cockpit renders it
 * literally: `ctx=131072,structured=1,tools=1,params=8000000000,quant=Q4_K_M,max=2`.
 *
 * EVERY CAPABILITY DEFAULTS TO FALSE and every number to zero, which is the
 * same fail-closed direction the engine's parser takes: absent means "not
 * advertised", which is a fact, where assuming yes would be a guess that fails
 * late and elsewhere. An unparseable value yields the defaults rather than
 * dropping the model -- the machine is still offering it.
 */
function attributesFrom(value: string): Omit<MachineModel, "modelId"> {
  const out = {
    params: 0,
    quant: "",
    contextWindow: 0,
    structuredOutput: false,
    tools: false,
    embeddings: false,
    vision: false,
    audioIn: false,
    audioOut: false,
    imageGen: false,
    maxConcurrent: 0,
  };
  for (const part of value.split(",")) {
    const at = part.indexOf("=");
    if (at < 0) continue;
    const key = part.slice(0, at).trim();
    const raw = part.slice(at + 1).trim();
    switch (key) {
      case "ctx":
        out.contextWindow = positiveInt(raw);
        break;
      case "structured":
        out.structuredOutput = advertised(raw);
        break;
      case "tools":
        out.tools = advertised(raw);
        break;
      case "embeddings":
        out.embeddings = advertised(raw);
        break;
      case "vision":
        out.vision = advertised(raw);
        break;
      case "audioin":
        out.audioIn = advertised(raw);
        break;
      case "audioout":
        out.audioOut = advertised(raw);
        break;
      case "imagegen":
        out.imageGen = advertised(raw);
        break;
      case "params":
        out.params = positiveInt(raw);
        break;
      case "quant":
        out.quant = raw;
        break;
      case "max":
        out.maxConcurrent = positiveInt(raw);
        break;
    }
  }
  return out;
}

function positiveInt(raw: string): number {
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? n : 0;
}

/** The spellings a machine might send, permissive in exactly one direction:
 *  anything unrecognised is FALSE, so a novel spelling costs a capability
 *  rather than granting one. */
function advertised(raw: string): boolean {
  return ["1", "true", "yes", "y"].includes(raw.toLowerCase());
}

/**
 * The models a machine advertises, sorted by id.
 *
 * THE ID KEEPS EVERY COLON. `model:hf.co/owner/repo:Q4_K_M` has three, and a
 * split on the first would leave a model that cannot be pulled or routed under
 * the name it was advertised with -- the router selects on the exact string.
 */
export function machineModelsFrom(labels: LabelMap): MachineModel[] {
  const out: MachineModel[] = [];
  for (const [key, value] of Object.entries(labels)) {
    if (!key.startsWith(MODEL_PREFIX)) continue;
    const modelId = key.slice(MODEL_PREFIX.length).trim();
    if (modelId === "") continue;
    out.push({ modelId, ...attributesFrom(value) });
  }
  return out.sort((a, b) => (a.modelId < b.modelId ? -1 : a.modelId > b.modelId ? 1 : 0));
}

/** A text-chat check must not call a specialized image, audio or embedding
 * endpoint. Vision is an input capability of chat models and remains eligible.
 * The engine still checks ownership, connectivity and required context. */
export function isChatModel(model: MachineModel): boolean {
  return !model.embeddings && !model.imageGen && !model.audioIn && !model.audioOut;
}

export function modelKindLabel(model: MachineModel): string {
  const kinds = [
    model.embeddings ? "Embeddings" : "",
    model.imageGen ? "Image generation" : "",
    model.audioIn ? "Transcription" : "",
    model.audioOut ? "Speech" : "",
  ].filter(Boolean);
  return kinds.length > 0 ? kinds.join(" · ") : model.tools ? "Chat & tools" : "Chat";
}

/**
 * The runtimes a machine advertises (ollama, openai-compatible).
 *
 * Read separately from the models because the two answer different questions,
 * and the pair of them separates two states a models list alone collapses: a
 * runtime installed and serving nothing is one fix ("pull a model"), and no
 * runtime at all is another ("set the machine up").
 */
export function machineRuntimesFrom(labels: LabelMap): string[] {
  const out: string[] = [];
  for (const key of Object.keys(labels)) {
    if (!key.startsWith(RUNTIME_PREFIX)) continue;
    const name = key.slice(RUNTIME_PREFIX.length).trim();
    if (name !== "") out.push(name);
  }
  return out.sort();
}

// ---------------------------------------------------------------------------
// A pull
// ---------------------------------------------------------------------------

/** One `v1:worker:modelPull` row, as this surface reads it. */
export interface ModelPull {
  pullId: string;
  workerId: string;
  model: string;
  status: string;
  /** The runtime's own most recent line, verbatim. */
  statusLine: string;
  layer: string;
  /** Bytes fetched FOR THE CURRENT LAYER. Resets on every new layer. */
  completedBytes: number;
  /** Size of the CURRENT LAYER, or 0 when the runtime did not say. */
  totalBytes: number;
  readvertised: boolean;
  errorMessage: string;
  requestedAt: string;
  updatedAt: string;
  endedAt: string;
}

function num(row: Record<string, unknown>, key: string): number {
  const raw = row[key];
  if (typeof raw === "number" && Number.isFinite(raw)) return raw;
  if (typeof raw === "string" && raw.trim() !== "") {
    const parsed = Number(raw);
    if (Number.isFinite(parsed)) return parsed;
  }
  return 0;
}

function str(row: Record<string, unknown>, key: string): string {
  const raw = row[key];
  return typeof raw === "string" ? raw : "";
}

export function pullFromRow(raw: Row | Record<string, unknown>): ModelPull {
  const row = flatten(raw as Row) as unknown as Record<string, unknown>;
  return {
    pullId: str(row, "id"),
    workerId: str(row, "workerId"),
    model: str(row, "model"),
    status: str(row, "status"),
    statusLine: str(row, "statusLine"),
    layer: str(row, "layer"),
    completedBytes: num(row, "completedBytes"),
    totalBytes: num(row, "totalBytes"),
    readvertised: row["readvertised"] === true,
    errorMessage: str(row, "errorMessage"),
    requestedAt: str(row, "requestedAt"),
    updatedAt: str(row, "updatedAt"),
    endedAt: str(row, "endedAt"),
  };
}

/** Whether a pull is still open, and therefore worth watching. */
export function pullIsLive(pull: ModelPull): boolean {
  return pull.status === "requested" || pull.status === "running";
}

/**
 * How far through the CURRENT STEP the pull is, or null when there is no
 * honest fraction to draw.
 *
 * ===========================================================================
 * NULL, NEVER ZERO, WHEN THE RUNTIME DID NOT STATE A TOTAL
 * ===========================================================================
 * A bar is a claim about a denominator. Returning 0 for an unstated total
 * paints an empty track, which says "this download has moved nothing" -- when
 * what happened is that nobody said how big the step is. The caller renders
 * the status line alone instead, which is the true and useful thing.
 *
 * IT IS THE STEP AND NOT THE PULL. Ollama fetches a model as a set of blobs
 * and counts each from zero, and there is no whole-pull denominator anywhere in
 * the protocol. A fraction across layers would be a number this page invented.
 */
export function pullProgressFraction(pull: ModelPull): number | null {
  if (!pullIsLive(pull)) return null;
  if (pull.totalBytes <= 0) return null;
  const fraction = pull.completedBytes / pull.totalBytes;
  if (!Number.isFinite(fraction) || fraction < 0) return null;
  // Clamped: the two counters are separate observations of a moving target,
  // and a runtime that overshoots its own stated total would draw past the end
  // of the track.
  return Math.min(1, fraction);
}

/**
 * The bytes line beneath the bar.
 *
 * IT ALWAYS SAYS "in this step". Without those words the numbers read as the
 * whole download, and the first layer boundary makes them jump backwards --
 * which reads as a bug in this page rather than as the next blob starting.
 */
export function pullStepLabel(pull: ModelPull): string {
  if (pull.completedBytes <= 0 && pull.totalBytes <= 0) return "";
  if (pull.totalBytes <= 0) {
    return `${formatBytes(pull.completedBytes)} fetched in this step`;
  }
  return `${formatBytes(pull.completedBytes)} of ${formatBytes(pull.totalBytes)} in this step`;
}
