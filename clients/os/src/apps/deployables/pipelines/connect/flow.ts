import type { ActionBarTone } from "../../../../kit/ActionBar";
import type { StopState } from "../../../../kit/Rail";
import type { MachineRow } from "../../../fleet/rows";
import { machinesAllowingPipelines } from "../fleet";
import type { PipelinePreview, PreviewStep, Problem } from "../calls";
import type { Compute, Delivery, PipelineRow } from "../rows";
import { computeWords, deliveryWords, shortSha } from "../words";

// WHAT THE CONNECT RAIL KNOWS, as functions over values (epic memql#5479,
// issue memql#5502; design record D14).
//
// ===========================================================================
// PURE, FOR THE REASON THE ADD-A-MACHINE FLOW IS
// ===========================================================================
// "A refusal stops Repository and offers Read again", "a step that needs a
// machine stops Compute when no machine of yours allows pipelines" and
// "Connect is offered only on Confirm, with every answer given" are statements
// about this file. Asserted through a rendered rail, each would be an
// assertion about the rail as well, and the rail has its own tests. No React
// here: ConnectPage draws what these return, and the hook holds the values
// they read.
//
// ===========================================================================
// THE ORDER IS A LAW, AND ONLY THE LAST STOP WRITES
// ===========================================================================
// Repository is a READ -- pipelinesPreview is connect's read half and writes
// nothing -- so the stages show before anything is confirmed. Compute is one
// question. Confirm is the one write. Every stop before the floor's forward
// act at Confirm can be left, cancelled or answered again for free, which is
// why Cancel here simply goes: there is nothing to take back.
//
// ===========================================================================
// THE PERSON PACES THE PAGE; THE CLUSTER PACES THE MARKS
// ===========================================================================
// When the read lands, the rail's marks move at once -- Repository done,
// Compute waiting on you -- but the page goes on showing the stages until the
// person continues. The stages are the reason the read happens before the
// questions, and a page that answered "4 stages" by replacing them with the
// next question would be a page nobody reads. A refusal is different: the
// stop it stopped at is the question, so that stop opens whoever was where.

/** The three stops, in the order the rail draws them. */
export type StepId = "repository" | "compute" | "confirm";

export const STEP_NAMES: Readonly<Record<StepId, string>> = {
  repository: "Repository",
  compute: "Compute",
  confirm: "Confirm",
};

/** What each stop is for, beside its name while it is open. */
const STEP_SENTENCES: Readonly<Record<StepId, string>> = {
  repository: "What memql-package.yaml declares at the head of the default branch.",
  compute: "Where the steps run.",
  confirm: "What the pipeline reports, and how changes reach it.",
};

/**
 * Connecting a pipeline, or changing one that is active.
 *
 * FIXED WHEN THE FLOW STARTS, so the title cannot flip under the person: the
 * pipelines feed delivers the newly connected row a beat after the write, and
 * a title read off that row would turn "Connect pipeline" into "Change
 * pipeline" on the very page that says it was connected.
 */
export type ConnectMode = "connect" | "change";

/** The preview read, as the flow holds it. */
export type PreviewRead =
  | { state: "idle" }
  | { state: "reading"; startedAt: number }
  | { state: "read"; preview: PipelinePreview }
  | { state: "failed"; problem: Problem };

/** The connect write, as the flow holds it. */
export type ConnectOutcome =
  | { state: "idle" }
  | { state: "connecting"; startedAt: number }
  | { state: "connected"; pipelineId: string; reconnected: boolean }
  | { state: "refused"; problem: Problem };

/** What the person has said. Null is "not said": a prefill or a preselection stands in. */
export interface Answers {
  compute: Compute | null;
  delivery: Delivery | null;
}

/** Whether any of the viewer's machines can take a pipeline step. */
export interface FleetReading {
  /** The machines feed has answered. Until it has, "no machine" is not known. */
  known: boolean;
  /** A machine of the viewer's, not revoked, reports `pipelines=allowed`. */
  available: boolean;
}

/** What a pipeline restates when it is connected again. */
export type Existing = Pick<PipelineRow, "status" | "compute" | "delivery">;

export interface ConnectFacts {
  mode: ConnectMode;
  /** The viewer holds `execute app:deployables/connect`. */
  can: boolean;
  /**
   * The flow held above the section is over THIS page's source.
   *
   * Two sections can each show a connect page -- the Deployables tab and the
   * Sources tab, both retained -- over one flow. A page whose source is not
   * the flow's must not draw another source's preview under its own title.
   */
  held: boolean;
  /** A live connection to the cluster. */
  online: boolean;
  /** The source's repository, as the source names it: what is read before the preview says. */
  repository: string;
  read: PreviewRead;
  /** The source's pipeline as the feed had it when the flow started. */
  existing: Existing | null;
  fleet: FleetReading;
  answers: Answers;
  outcome: ConnectOutcome;
  /** The person has moved past the stages: Continue, or an answer at Compute. */
  reviewed: boolean;
  /** Milliseconds since the epoch, the clock a wait is measured against. */
  now: number;
}

// ---------------------------------------------------------------------------
// Words
// ---------------------------------------------------------------------------

/** "a", "a and b", "a, b and c". */
export function andList(items: readonly string[]): string {
  if (items.length <= 1) return items[0] ?? "";
  return `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`;
}

/**
 * When a stage runs, in words.
 *
 * Each event and mode takes its own preposition -- a stage runs IN the merge
 * queue and ON pushes -- so neighbouring words that share one share it once:
 * "Runs on pull requests and pushes". A stage that names one says "only",
 * because that is the part a person is checking. An empty list is every run,
 * which needs no sentence.
 */
const ON_PHRASES: Readonly<Record<string, { prep: "on" | "in"; noun: string }>> = {
  pull_request: { prep: "on", noun: "pull requests" },
  merge_group: { prep: "in", noun: "the merge queue" },
  push: { prep: "on", noun: "pushes" },
  release: { prep: "on", noun: "releases" },
  affected: { prep: "in", noun: "affected runs" },
  full: { prep: "in", noun: "full runs" },
};

export function onWords(on: readonly string[]): string {
  const tokens = on.map((t) => t.trim()).filter((t) => t !== "");
  if (tokens.length === 0) return "";
  const groups: { prep: string; nouns: string[] }[] = [];
  for (const token of tokens) {
    // The engine refuses an event it does not know (pipeline_event_unknown),
    // so a token missing here is this build being older than the engine: its
    // own words, with the underscores read as spaces, beat a guessed name.
    const phrase = ON_PHRASES[token] ?? { prep: "on", noun: token.split("_").join(" ") };
    const last = groups[groups.length - 1];
    if (last !== undefined && last.prep === phrase.prep) last.nouns.push(phrase.noun);
    else groups.push({ prep: phrase.prep, nouns: [phrase.noun] });
  }
  const said = groups.map((g) => `${g.prep} ${andList(g.nouns)}`).join(" and ");
  return `Runs ${said}${tokens.length === 1 ? " only" : ""}`;
}

/** One manifest step as the stage list reads it: "go-tests, 4 shards", "os-checks, needs docker". */
export function stepPhrase(step: Pick<PreviewStep, "name" | "shards" | "needs">): string {
  const parts = [step.name];
  if (step.shards > 1) parts.push(`${step.shards} shards`);
  if (step.needs.length > 0) parts.push(`needs ${andList(step.needs)}`);
  return parts.join(", ");
}

/** Repository's line once it is read: "acme/shop at main, 4 stages". */
export function repositoryAnswer(p: Pick<PipelinePreview, "repository" | "defaultBranch" | "stages">): string {
  const n = p.stages.length;
  return `${p.repository} at ${p.defaultBranch}, ${n} ${n === 1 ? "stage" : "stages"}`;
}

/** Which commit was read: a person who pushed a fix and read again can see it is the new one. */
export function readFromWords(p: Pick<PipelinePreview, "sha" | "defaultBranch">): string {
  const sha = shortSha(p.sha);
  return sha === "" ? "" : `Read at ${sha}, the head of ${p.defaultBranch}.`;
}

/** Every step that names a need, as "os-checks needs docker", in plan order. */
export function needersOf(p: Pick<PipelinePreview, "stages">): string[] {
  return p.stages.flatMap((stage) =>
    stage.steps.filter((step) => step.needs.length > 0).map((step) => `${step.name} needs ${andList(step.needs)}`),
  );
}

/**
 * Why the fleet is the one place these steps can run.
 *
 * WITH THE WAY OUT. "Cluster" is not offered beside it, because the engine
 * refuses a step's need on a cluster-only pipeline at connect
 * (pipeline_fleet_not_consented) -- an answer that cannot work is not offered
 * and then taken back -- so the sentence says what keeps every step here.
 */
export function fleetReason(needers: readonly string[]): string {
  const one = needers.length === 1;
  return `Only your machines offer what ${one ? "this step needs" : "these steps need"}: ${needers.join("; ")}. ` +
    `To keep every step in this cluster, remove the ${one ? "need from the step" : "needs from the steps"}.`;
}

/** What to do when steps need a machine and none of the viewer's allows pipelines. */
export function needsRemedy(needers: readonly string[]): string {
  return "None of your machines allows pipelines. Allow pipelines in a machine's policy.yaml (pipelines.allow), " +
    `or remove the ${needers.length === 1 ? "need from the step" : "needs from the steps"}.`;
}

/** The two answers Compute can take, in the person's words. */
export const COMPUTE_CHOICES: Readonly<Record<Compute, { label: string; description: string }>> = {
  cluster: { label: "Cluster", description: "Every step runs in this cluster." },
  cluster_and_fleet: {
    label: "Cluster and your fleet",
    // THE SWITCH THE RUNNER ALSO ASKS. A machine that allows pipelines takes a
    // step only while its owner's computer use is on; the runner refuses with
    // pipeline_fleet_disabled otherwise, so the choice says so before it is
    // made rather than after the first run fails.
    description: "Steps that need Docker, a display or other tooling run on your machines that allow pipelines. Computer use must be on for your machines.",
  },
};

/** The delivery choice's own names; `deliveryWords` says what each does. */
export const DELIVERY_LABELS: Readonly<Record<Delivery, string>> = { webhook: "Webhook", poll: "Polling" };

/**
 * What the chosen delivery does, said under the field.
 *
 * TWO CONSEQUENCES A PERSON CANNOT SEE FROM THE NAME. A webhook needs GitHub to
 * reach this cluster, which is what the suggestion is read from; and a polled
 * pipeline sees branch heads only, so the merge queue and releases -- which
 * GitHub only ever delivers -- start nothing there (docs/public/operate/
 * pipelines.md, "Delivery").
 */
export function deliveryCaption(delivery: Delivery, suggested: Delivery): string {
  if (delivery === "webhook") {
    return `${deliveryWords("webhook")}.${suggested === "poll" ? " GitHub may not be able to reach this cluster, so polling is suggested." : ""}`;
  }
  return `${deliveryWords("poll")}. Merge queue and release runs need a webhook.`;
}

/** A wait, as m:ss. */
export function waitWords(ms: number): string {
  const seconds = Math.max(0, Math.floor(ms / 1000));
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")}`;
}

// ---------------------------------------------------------------------------
// The fleet
// ---------------------------------------------------------------------------

/**
 * Whether the fleet can be offered: one of the pipeline owner's machines
 * allows pipelines -- the shared rule in `../fleet.ts`, which says whose
 * machines count and why only the reported label does.
 */
export function fleetAvailable(
  machines: readonly Pick<MachineRow, "ownerUserId" | "revokedAt" | "reportedLabels">[],
  ownerUserId: string,
): boolean {
  return machinesAllowingPipelines(machines, ownerUserId) > 0;
}

// ---------------------------------------------------------------------------
// Where a refusal lands
// ---------------------------------------------------------------------------

const COMPUTE_CODES: ReadonlySet<string> = new Set([
  "pipeline_fleet_not_consented",
  "pipeline_fleet_disabled",
  "pipeline_no_machine_for_need",
]);

const CONFIRM_CODES: ReadonlySet<string> = new Set([
  "pipeline_secret_not_allowed",
  "pipeline_secret_invalid",
  "pipeline_secret_missing",
]);

/**
 * The stop a connect refusal belongs to (README, "A STOP OWNS ITS OWN
 * REFUSAL"): the fleet's consent at Compute, the secrets at Confirm where they
 * are listed, and everything about the repository, its manifest or the grant
 * at Repository, where reading again is the way forward.
 *
 * A FAILURE WITH NO CODE stays at Confirm, where the act was taken: it says
 * nothing about which stop is wrong, and a guess would send somebody to repair
 * the wrong thing.
 */
export function stopForProblem(problem: Problem): StepId {
  if (COMPUTE_CODES.has(problem.code)) return "compute";
  if (problem.code === "" || CONFIRM_CODES.has(problem.code)) return "confirm";
  return "repository";
}

// ---------------------------------------------------------------------------
// The reading
// ---------------------------------------------------------------------------

export type Phase =
  /** The flow held above the section is over another source, or none. */
  | "unheld"
  /** Nothing read yet, and no connection to read over. */
  | "offline"
  | "reading"
  /** The read failed and named no code: a fault, not a refusal. */
  | "failed"
  /** The preview refused, or the viewer does not hold the connect part. */
  | "refused"
  /** Steps need a machine, and none of the viewer's allows pipelines. */
  | "needsFleet"
  /** Compute is the person's to answer. */
  | "compute"
  /** Every answer is given. */
  | "confirm"
  | "connecting"
  | "connected"
  /** The connect refused; the problem names the stop it belongs to. */
  | "notConnected";

export interface ComputeReading {
  /** The answers offered, in order. Empty when no answer can work; null while the fleet is not known. */
  options: Compute[] | null;
  /** What the choice shows as chosen: the answer, or else the preselection. */
  value: Compute;
  /** Answered by choosing, by a reconnect's prefill, or by being the only possibility. */
  answered: boolean;
  /** "<step> needs <need>", for every step that names one. */
  needers: string[];
}

export interface Reading {
  phase: Phase;
  /** The manifest as read, once the read answered one without a refusal. */
  preview: PipelinePreview | null;
  /** A problem, and the stop it renders at. */
  problem: { at: StepId; problem: Problem } | null;
  compute: ComputeReading | null;
  delivery: Delivery;
}

/** What a viewer without the connect part is told: the copy is keyed by the code, and there is no server sentence to add. */
export const PART_NOT_HELD: Problem = { code: "capability_not_held", message: "", scope: "" };

/** The preview answered no row at all. */
export const NO_PREVIEW: Problem = { code: "", message: "The cluster answered the preview with nothing.", scope: "" };

/**
 * Compute's answers.
 *
 * THE CHOICES ARE WHAT CAN WORK. A step that names a need runs only on a fleet
 * machine, so with needs "Cluster" is not offered (connect would refuse it),
 * and with needs and no machine that allows pipelines NOTHING is -- the stop
 * is stopped, with the remedy. Without needs, "Cluster" is always there and the
 * fleet joins it once a machine allows pipelines.
 *
 * A PRESELECTION IS NOT AN ANSWER. Steps going to somebody's own machines is a
 * consent ("nothing about a laptop is a default", connect.go), so the fleet is
 * checked and explained but the step waits for the person to choose. Only a
 * stop with one possible answer that consents to nothing -- the cluster, when
 * there is no fleet, nothing needs one and no other answer was held -- answers
 * itself.
 */
export function computeOf(f: Pick<ConnectFacts, "fleet" | "answers" | "existing">, preview: PipelinePreview): ComputeReading {
  const needers = needersOf(preview);
  const needs = needers.length > 0 || preview.needs.length > 0;
  const options: Compute[] | null = !f.fleet.known
    ? null
    : needs
      ? f.fleet.available ? ["cluster_and_fleet"] : []
      : f.fleet.available ? ["cluster", "cluster_and_fleet"] : ["cluster"];
  // A RECONNECT IS PREFILLED, the server's reading of the pipeline first and
  // the feed's after it. An answer that is no longer offered (a fleet
  // pipeline whose machines stopped allowing pipelines) is not kept, and is
  // not quietly replaced either -- not even by the one answer left: Compute
  // asks again, so the person sees their setting could not stand before
  // anything is saved without it.
  const held = f.answers.compute ?? (preview.existing ?? f.existing)?.compute ?? null;
  const preselected: Compute = needs ? "cluster_and_fleet" : "cluster";
  if (options === null) {
    return { options, value: held ?? preselected, answered: !needs && held === "cluster", needers };
  }
  if (held !== null && options.includes(held)) return { options, value: held, answered: true, needers };
  if (held === null && options.length === 1 && options[0] === "cluster") return { options, value: "cluster", answered: true, needers };
  return { options, value: preselected, answered: false, needers };
}

/** How changes arrive: the person's choice, else a reconnect's, else the cluster's suggestion. */
export function deliveryOf(f: Pick<ConnectFacts, "answers" | "existing">, preview: PipelinePreview): Delivery {
  return f.answers.delivery ?? (preview.existing ?? f.existing)?.delivery ?? preview.suggestedDelivery;
}

export function readingOf(f: ConnectFacts): Reading {
  const none = { preview: null, problem: null, compute: null, delivery: "webhook" as Delivery };
  // THE OS HIDES; THE ENGINE REFUSES (parts.ts). A page reached without the
  // part says so in place and offers nothing that would only be refused --
  // not even starting a read over this source.
  if (!f.can) return { ...none, phase: "refused", problem: { at: "repository", problem: PART_NOT_HELD } };
  if (!f.held) return { ...none, phase: "unheld" };
  const read = f.read;
  if (read.state === "idle") return { ...none, phase: f.online ? "reading" : "offline" };
  if (read.state === "reading") return { ...none, phase: "reading" };
  if (read.state === "failed") {
    // A READ THAT FAILED WITH A CODE IS A REFUSAL by another road: the part's
    // gate and ErrNotOwner answer as errors before the preview can answer a
    // refusal as data, and the person needs the same copy either way.
    return { ...none, phase: read.problem.code === "" ? "failed" : "refused", problem: { at: "repository", problem: read.problem } };
  }
  const preview = read.preview;
  if (preview.refusal !== null) return { ...none, phase: "refused", problem: { at: "repository", problem: preview.refusal } };

  const base = { preview, compute: computeOf(f, preview), delivery: deliveryOf(f, preview) };
  switch (f.outcome.state) {
    case "connected":
      return { ...base, phase: "connected", problem: null };
    case "connecting":
      return { ...base, phase: "connecting", problem: null };
    case "refused":
      return { ...base, phase: "notConnected", problem: { at: stopForProblem(f.outcome.problem), problem: f.outcome.problem } };
  }
  if (base.compute.options !== null && base.compute.options.length === 0) return { ...base, phase: "needsFleet", problem: null };
  if (!base.compute.answered) return { ...base, phase: "compute", problem: null };
  return { ...base, phase: "confirm", problem: null };
}

/** The person has moved past the stages. Pressing Connect is moving past them. */
function hasReviewed(f: ConnectFacts): boolean {
  return f.reviewed || f.outcome.state !== "idle";
}

// ---------------------------------------------------------------------------
// The stops
// ---------------------------------------------------------------------------

export interface ConnectStep {
  id: StepId;
  name: string;
  state: StopState;
  sentence: string;
  /** The stop's line once it has something to say back. */
  answer: string;
  /** Whether its line opens anything. */
  openable: boolean;
}

function reachable(state: StopState): boolean {
  return state !== "ahead" && state !== "pending" && state !== "unknown";
}

/**
 * The three stops.
 *
 * A STOPPED STOP DIMS EVERY STOP AFTER IT (README, "A STOP OWNS ITS OWN
 * REFUSAL"): a refusal at Repository leaves nothing later answerable, because
 * the next read may name other stages, other needs and other secrets.
 */
export function stepsFor(f: ConnectFacts, r: Reading = readingOf(f)): ConnectStep[] {
  const { phase } = r;
  const at = r.problem?.at ?? null;
  const locked = phase === "connecting" || phase === "connected";
  const notDone = f.mode === "change" ? "Not saved" : "Not connected";

  let repository: Pick<ConnectStep, "state" | "answer">;
  switch (phase) {
    case "unheld":
      repository = { state: "open", answer: "" };
      break;
    case "offline":
      repository = { state: "unknown", answer: "" };
      break;
    case "reading":
      repository = { state: "current", answer: "" };
      break;
    case "failed":
      repository = { state: "stopped", answer: "Could not be read" };
      break;
    case "refused":
      repository = { state: "stopped", answer: "Cannot be connected" };
      break;
    default:
      repository = at === "repository" || r.preview === null
        ? { state: "stopped", answer: "Cannot be connected" }
        : { state: "done", answer: repositoryAnswer(r.preview) };
  }

  let compute: Pick<ConnectStep, "state" | "answer">;
  if (r.preview === null || r.compute === null || at === "repository") compute = { state: "ahead", answer: "" };
  else if (phase === "needsFleet") compute = { state: "stopped", answer: "None of your machines allows pipelines" };
  else if (at === "compute") compute = { state: "stopped", answer: computeWords(r.compute.value) };
  else if (phase === "compute") compute = { state: "open", answer: "" };
  else compute = { state: "done", answer: computeWords(r.compute.value) };

  let confirm: Pick<ConnectStep, "state" | "answer">;
  if (phase === "confirm") confirm = { state: "open", answer: DELIVERY_LABELS[r.delivery] };
  else if (phase === "connecting") confirm = { state: "current", answer: DELIVERY_LABELS[r.delivery] };
  else if (phase === "connected") confirm = { state: "done", answer: f.mode === "change" ? "Saved" : "Connected" };
  else if (at === "confirm") confirm = { state: "stopped", answer: notDone };
  else confirm = { state: "ahead", answer: "" };

  return [
    { id: "repository", name: STEP_NAMES.repository, sentence: STEP_SENTENCES.repository, ...repository, openable: reachable(repository.state) },
    // A choice in flight or made is a fact, not a question: once Connect is
    // pressed, Compute's answer is on its line and its line opens nothing.
    { id: "compute", name: STEP_NAMES.compute, sentence: STEP_SENTENCES.compute, ...compute, openable: reachable(compute.state) && !locked },
    { id: "confirm", name: STEP_NAMES.confirm, sentence: STEP_SENTENCES.confirm, ...confirm, openable: reachable(confirm.state) },
  ];
}

/**
 * The stop the page shows when nobody has chosen one.
 *
 * A stopped stop first: the refusal is the question. Then, until the person
 * moves past them, the stages -- the read is the cluster's, and its answer is
 * what the person came to see. Then the first stop waiting on the person,
 * the one the cluster is working on, and at the end the last.
 */
export function openStepFor(steps: readonly ConnectStep[], f: ConnectFacts): StepId {
  const stopped = steps.find((s) => s.state === "stopped");
  if (stopped) return stopped.id;
  if (!hasReviewed(f)) return "repository";
  const open = steps.find((s) => s.state === "open");
  if (open) return open.id;
  const current = steps.find((s) => s.state === "current");
  if (current) return current.id;
  return steps[steps.length - 1]?.id ?? "repository";
}

/**
 * What a person's own choice of stop is pinned to. A choice holds until the
 * flow moves -- a read landing, an answer given, a refusal -- and the stop the
 * flow is at is shown again, the add-a-machine flow's rule for its override.
 */
export function movementOf(r: Reading, f: ConnectFacts): string {
  return `${r.phase}:${r.problem?.at ?? ""}:${hasReviewed(f) ? "reviewed" : ""}`;
}

// ---------------------------------------------------------------------------
// The floor (interface rule 12)
// ---------------------------------------------------------------------------

export type ActId = "cancel" | "leave" | "read" | "readAgain" | "continue" | "connect" | "done";

export interface FlowAct {
  id: ActId;
  label: string;
  tone?: "quiet" | "primary";
  /** A text action rather than a button: the way out, beside the one act. */
  text?: boolean;
  busy?: boolean;
}

export interface FlowBar {
  word: string;
  detail: string;
  tone: ActionBarTone;
  /** The measure of a wait, or "". */
  meta: string;
  acts: FlowAct[];
}

/** The forward act keeps its name through the flow, so a refusal can say what was not done. */
export function forwardLabel(mode: ConnectMode): string {
  return mode === "change" ? "Save changes" : "Connect pipeline";
}

/**
 * The bar.
 *
 * NOTHING IS WRITTEN UNTIL THE FORWARD ACT, so Cancel is a text act that
 * simply goes, at every stop before it. While the cluster reads there is no
 * forward act, and Leave is the button beside Cancel (DESIGN.md, "the floor
 * has two verbs and one button"); once Connect is pressed nothing on this page
 * takes it back, so there is no Cancel at all -- only the act, busy, until the
 * cluster answers. Leave is the Head's Back the whole way through.
 */
export function barFor(f: ConnectFacts, r: Reading = readingOf(f)): FlowBar {
  const cancel: FlowAct = { id: "cancel", label: "Cancel", text: true };
  const readAgain: FlowAct = { id: "readAgain", label: "Read again", text: true };
  const forward: FlowAct = { id: "connect", label: forwardLabel(f.mode), tone: "primary" };
  const resting = { tone: "none" as ActionBarTone, meta: "" };
  switch (r.phase) {
    case "unheld":
      return {
        ...resting,
        word: "Not read yet",
        detail: "connecting starts by reading this source's memql-package.yaml",
        acts: [cancel, { id: "read", label: "Read memql-package.yaml", tone: "primary" }],
      };
    case "offline":
      return { word: "Offline", detail: "memql-package.yaml is read when the connection to the cluster returns", tone: "paused", meta: "", acts: [cancel] };
    case "reading":
      return {
        word: "Reading memql-package.yaml",
        detail: `at the head of ${f.repository}'s default branch`,
        tone: "busy",
        meta: f.read.state === "reading" ? waitWords(f.now - f.read.startedAt) : "",
        acts: [cancel, { id: "leave", label: "Leave" }],
      };
    case "failed":
      return { word: "Not read", detail: "nothing was written", tone: "paused", meta: "", acts: [cancel, readAgain] };
    case "refused":
      // Reading again cannot help a viewer the part's gate refuses.
      return { word: "Cannot be connected", detail: "the Repository step says why", tone: "paused", meta: "", acts: f.can ? [cancel, readAgain] : [cancel] };
    case "needsFleet":
      // Allowing pipelines on a machine reaches this page by itself, through
      // the machines feed. Removing the need is a push, and reading again is
      // how the page learns of it.
      return { word: "A step needs one of your machines", detail: "none of yours allows pipelines yet", tone: "paused", meta: "", acts: [cancel, readAgain] };
    case "compute":
      return hasReviewed(f)
        ? { ...resting, word: "Choose where steps run", detail: "", acts: [cancel] }
        : { ...resting, word: "Review the stages", detail: "then choose where steps run", acts: [cancel, { id: "continue", label: "Continue", tone: "primary" }] };
    case "confirm":
      if (!hasReviewed(f)) {
        return { ...resting, word: "Review the stages", detail: "then confirm", acts: [cancel, { id: "continue", label: "Continue", tone: "primary" }] };
      }
      if (!f.online) return { word: "Offline", detail: "connecting needs a live connection to the cluster", tone: "paused", meta: "", acts: [cancel] };
      return {
        ...resting,
        word: f.mode === "change" ? "Ready to save" : "Ready to connect",
        detail: f.mode === "change" ? "the next run uses these settings" : `checks report on GitHub as ${r.preview?.checkName ?? ""}`,
        acts: [cancel, forward],
      };
    case "connecting":
      return {
        word: f.mode === "change" ? "Saving the pipeline" : "Connecting the pipeline",
        detail: "",
        tone: "busy",
        meta: f.outcome.state === "connecting" ? waitWords(f.now - f.outcome.startedAt) : "",
        acts: [{ ...forward, busy: true }],
      };
    case "connected":
      return {
        word: f.mode === "change" ? "Saved" : "Connected",
        detail: f.mode === "change" ? "the next run uses these settings" : "runs start with the next push or pull request",
        tone: "live",
        meta: "",
        acts: [{ id: "done", label: "Done", tone: "primary" }],
      };
    case "notConnected": {
      const at = r.problem?.at ?? "confirm";
      // Each stop's way forward: read the repository again, choose again (an
      // answer at Compute moves on by itself), or try the act again.
      const acts = at === "repository" ? [cancel, readAgain] : at === "compute" || !f.online ? [cancel] : [cancel, forward];
      return {
        word: f.mode === "change" ? "Not saved" : "Not connected",
        detail: `the ${STEP_NAMES[at]} step says why`,
        tone: "paused",
        meta: "",
        acts,
      };
    }
  }
}
