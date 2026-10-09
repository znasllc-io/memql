import { rowNumber, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { boolOr, flatten, stringsOf } from "../../../kit/rows";

// Pipelines, as this app reads them (epic memql#5479).
//
// A pipeline is a FACT ABOUT A SOURCE (design record D12): one per source,
// owned by the source's owner, connected from the source's page. A run is one
// attempt at one commit, and its steps are v1:work:step rows -- the run
// compiles into a work goal, so "where each ran and for how long" is read off
// the step rows the work spine already writes (D13), never off a second copy.
//
// PROJECTED ON THE READ SIDE, like every feed in this app: a collection holds
// raw wire rows, because an event's payload is folded in AS the row type, so
// a projection kept in the collection would be right until the first update.
//
// IDS ARE COMPARED BY THEIR TAIL. The engine answers a seed with bare ids and
// an event with whatever its envelope carries; `idTail` reads both spellings
// the same, and cannot invent a match -- two rows of one concept never share a
// short id (the Nexus rule, `apps/nexus/rows.ts`).

export const PIPELINE_CONCEPT = "v1:pipelines:pipeline";
export const RUN_CONCEPT = "v1:pipelines:run";
export const WORK_STEP_CONCEPT = "v1:work:step";
export const LIBRARY_ARTIFACT_CONCEPT = "v1:library:artifact";

/** The short id of a bare or canonical id. */
export function idTail(id: string): string {
  const trimmed = id.trim();
  if (trimmed === "") return "";
  return trimmed.slice(trimmed.lastIndexOf(":") + 1);
}

/** Whether two ids name the same row, under either spelling. */
export function sameId(a: string, b: string): boolean {
  const left = idTail(a);
  return left !== "" && left === idTail(b);
}

// ---------------------------------------------------------------------------
// The pipeline
// ---------------------------------------------------------------------------

export type Delivery = "webhook" | "poll";
export type Compute = "cluster" | "cluster_and_fleet";

export interface PipelineRow {
  id: string;
  ownerUserId: string;
  packageId: string;
  /** The manifest's name when it was connected: the check run is `MemQL / <name>`. */
  name: string;
  /** Lower-cased owner/name. */
  repository: string;
  defaultBranch: string;
  installationId: string;
  delivery: Delivery;
  compute: Compute;
  status: "active" | "disconnected";
  /** globalSecret NAMES the steps may resolve -- the owner's allowlist. Never values. */
  secretNames: string[];
  connectedAt: string;
}

export function pipelineFromRow(raw: Row): PipelineRow {
  const row = flatten(raw);
  return {
    id: idTail(rowString(row, "id")),
    ownerUserId: rowString(row, "ownerUserId"),
    packageId: idTail(rowString(row, "packageId")),
    name: rowString(row, "name"),
    repository: rowString(row, "repository"),
    defaultBranch: rowString(row, "defaultBranch"),
    installationId: rowString(row, "installationId"),
    delivery: rowString(row, "delivery") === "poll" ? "poll" : "webhook",
    compute: rowString(row, "compute") === "cluster_and_fleet" ? "cluster_and_fleet" : "cluster",
    status: rowString(row, "status") === "disconnected" ? "disconnected" : "active",
    secretNames: stringsOf(row, "secretNames"),
    connectedAt: rowString(row, "connectedAt"),
  };
}

/** What changing a pipeline is: a person's connect, reconnect or disconnect. */
export function pipelineFingerprint(p: PipelineRow): string {
  return [p.status, p.delivery, p.compute, p.secretNames.join(","), p.connectedAt].join("|");
}

// ---------------------------------------------------------------------------
// The run
// ---------------------------------------------------------------------------

export type RunStatus = "queued" | "in_progress" | "completed";
export type RunConclusion = "" | "success" | "failure" | "cancelled" | "refused";
export type StageStatus = "waiting" | "running" | "passed" | "failed" | "cancelled" | "skipped" | "blocked";

export interface StageRow {
  name: string;
  status: StageStatus;
  durationMs: number;
  steps: number;
  failed: number;
}

export interface RunNote {
  code: string;
  message: string;
}

export interface RunRow {
  id: string;
  ownerUserId: string;
  pipelineId: string;
  repository: string;
  sha: string;
  mode: "affected" | "full";
  event: "pull_request" | "merge_group" | "push" | "release" | "schedule" | "manual";
  runKey: string;
  attempt: number;
  trigger: "webhook" | "poll" | "schedule" | "manual" | "rerun";
  rerunOf: string;
  rerunFailedOnly: boolean;
  pullRequest: number;
  headBranch: string;
  title: string;
  version: string;
  status: RunStatus;
  conclusion: RunConclusion;
  refusalCode: string;
  refusalMessage: string;
  refusalScope: string;
  checkRunId: string;
  checkRunState: "" | "written" | "refused" | "unavailable";
  notes: RunNote[];
  workRunId: string;
  cancelRequested: boolean;
  stages: StageRow[];
  queuedAt: string;
  startedAt: string;
  finishedAt: string;
  durationMs: number;
}

const STAGE_STATUSES: ReadonlySet<string> = new Set(["waiting", "running", "passed", "failed", "cancelled", "skipped", "blocked"]);
const EVENTS: ReadonlySet<string> = new Set(["pull_request", "merge_group", "push", "release", "schedule", "manual"]);
const CONCLUSIONS: ReadonlySet<string> = new Set(["success", "failure", "cancelled", "refused"]);

function objectsOf(row: Row, key: string): Row[] {
  const v = row[key];
  if (!Array.isArray(v)) return [];
  return v.filter((m): m is Row => m !== null && typeof m === "object" && !Array.isArray(m));
}

function notesOf(list: readonly Row[]): RunNote[] {
  return list.map((n) => ({ code: rowString(n, "code"), message: rowString(n, "message") })).filter((n) => n.code !== "");
}

export function runFromRow(raw: Row): RunRow {
  const row = flatten(raw);
  const status = rowString(row, "status");
  const conclusion = rowString(row, "conclusion");
  const event = rowString(row, "event");
  const trigger = rowString(row, "trigger");
  const checkRunState = rowString(row, "checkRunState");
  return {
    id: idTail(rowString(row, "id")),
    ownerUserId: rowString(row, "ownerUserId"),
    pipelineId: idTail(rowString(row, "pipelineId")),
    repository: rowString(row, "repository"),
    sha: rowString(row, "sha"),
    mode: rowString(row, "mode") === "full" ? "full" : "affected",
    event: (EVENTS.has(event) ? event : "push") as RunRow["event"],
    runKey: rowString(row, "runKey"),
    attempt: Math.max(1, rowNumber(row, "attempt")),
    trigger: (["poll", "schedule", "manual", "rerun"].includes(trigger) ? trigger : "webhook") as RunRow["trigger"],
    rerunOf: idTail(rowString(row, "rerunOf")),
    rerunFailedOnly: boolOr(row, "rerunFailedOnly", false),
    pullRequest: rowNumber(row, "pullRequest"),
    headBranch: rowString(row, "headBranch"),
    title: rowString(row, "title"),
    version: rowString(row, "version"),
    status: status === "in_progress" || status === "completed" ? status : "queued",
    conclusion: (CONCLUSIONS.has(conclusion) ? conclusion : "") as RunConclusion,
    refusalCode: rowString(row, "refusalCode"),
    refusalMessage: rowString(row, "refusalMessage"),
    refusalScope: rowString(row, "refusalScope"),
    checkRunId: rowString(row, "checkRunId"),
    checkRunState: (["written", "refused", "unavailable"].includes(checkRunState) ? checkRunState : "") as RunRow["checkRunState"],
    notes: notesOf(objectsOf(row, "notes")),
    workRunId: idTail(rowString(row, "workRunId")),
    cancelRequested: boolOr(row, "cancelRequested", false),
    stages: objectsOf(row, "stages").map((s) => {
      const st = rowString(s, "status");
      return {
        name: rowString(s, "name"),
        status: (STAGE_STATUSES.has(st) ? st : "waiting") as StageStatus,
        durationMs: rowNumber(s, "durationMs"),
        steps: rowNumber(s, "steps"),
        failed: rowNumber(s, "failed"),
      };
    }),
    queuedAt: rowString(row, "queuedAt"),
    startedAt: rowString(row, "startedAt"),
    finishedAt: rowString(row, "finishedAt"),
    durationMs: rowNumber(row, "durationMs"),
  };
}

/**
 * What a person would call a change to a run.
 *
 * A HEARTBEAT IS NOT NEWS (clients/os/README.md, the arrival cue): the run row
 * is rewritten every 30 seconds by its driver's lease (`driverHeartbeatAt`),
 * and naming it here would ring every running row on a timer. The stage table
 * moves at stage boundaries, which is progress a person watches -- but the
 * Runs list rings only on a TERMINAL state (`runRingFingerprint`), so a long
 * run is quiet until it has an answer.
 */
export function runFingerprint(run: RunRow): string {
  return [run.status, run.conclusion, run.cancelRequested ? "c" : "", run.stages.map((s) => `${s.name}:${s.status}`).join(",")].join("|");
}

/**
 * The Runs list's fingerprint: the arrival cue on a terminal state only
 * (issue memql#5499). A queued row arriving and a stage finishing change
 * nothing it announces; the conclusion does.
 */
export function runRingFingerprint(run: RunRow): string {
  return run.status === "completed" ? `done|${run.conclusion}` : "open";
}

export function isFinished(run: RunRow): boolean {
  return run.status === "completed";
}

// ---------------------------------------------------------------------------
// A step of a run (a v1:work:step of the run's work run)
// ---------------------------------------------------------------------------

export type StepStatus = "pending" | "ready" | "running" | "waiting" | "done" | "failed" | "skipped" | "cancelled";

export interface StepWhere {
  /** "cluster", "fleet", or "" when no runner answered (a skip, a driver-side failure). */
  surface: "cluster" | "fleet" | "";
  nodeId: string;
  jobName: string;
  workerId: string;
}

export interface StepRow {
  id: string;
  runId: string;
  key: string;
  seq: number;
  stage: string;
  /** The manifest step's name, without the shard suffix. */
  name: string;
  /** 1-based shard index from a `#<i>` key, or 0 for a step that is not a shard. */
  shard: number;
  status: StepStatus;
  /** The runner would not start it (an executor refusal, recorded as failed). */
  refused: boolean;
  errorCode: string;
  errorMessage: string;
  /** A skipped or cancelled step's reason, in the server's words. */
  reason: string;
  durationMs: number;
  startedAt: string;
  finishedAt: string;
  where: StepWhere;
  logFileId: string;
  artifactFileIds: string[];
  notes: RunNote[];
}

const STEP_STATUSES: ReadonlySet<string> = new Set(["pending", "ready", "running", "waiting", "done", "failed", "skipped", "cancelled"]);

export function stepFromRow(raw: Row): StepRow {
  const row = flatten(raw);
  const key = rowString(row, "key");
  const call = (row["call"] && typeof row["call"] === "object" ? row["call"] : {}) as Row;
  const result = (row["result"] && typeof row["result"] === "object" ? row["result"] : {}) as Row;
  const metadata = (result["metadata"] && typeof result["metadata"] === "object" ? result["metadata"] : {}) as Row;
  const binding = (row["binding"] && typeof row["binding"] === "object" ? row["binding"] : {}) as Row;
  const status = rowString(row, "status");
  const hash = key.lastIndexOf("#");
  const shard = hash > 0 ? Number.parseInt(key.slice(hash + 1), 10) : 0;
  const dot = key.indexOf(".");
  const surface = rowString(binding, "surface");
  return {
    id: idTail(rowString(row, "id")),
    runId: idTail(rowString(row, "runId")),
    key,
    seq: rowNumber(row, "seq"),
    stage: rowString(call, "stage") || (dot > 0 ? key.slice(0, dot) : ""),
    name: rowString(call, "name") || (dot > 0 ? key.slice(dot + 1, hash > dot ? hash : undefined) : key),
    shard: Number.isFinite(shard) && shard > 0 ? shard : 0,
    status: (STEP_STATUSES.has(status) ? status : "pending") as StepStatus,
    refused: rowString(result, "status") === "refused",
    errorCode: rowString(row, "errorCode"),
    errorMessage: rowString(row, "errorMessage"),
    reason: rowString(result, "reason") || (status === "skipped" ? rowString(row, "errorMessage") : ""),
    durationMs: rowNumber(row, "durationMs"),
    startedAt: rowString(row, "startedAt"),
    finishedAt: rowString(row, "finishedAt"),
    where: {
      surface: surface === "cluster" || surface === "fleet" ? surface : "",
      nodeId: rowString(binding, "nodeId"),
      jobName: rowString(binding, "jobName"),
      workerId: rowString(binding, "workerId"),
    },
    logFileId: idTail(rowString(row, "logFileId")),
    artifactFileIds: stringsOf(row, "artifactFileIds").map(idTail).filter((id) => id !== ""),
    notes: notesOf(objectsOf(metadata, "notes")),
  };
}

/** Steps in plan order: by seq, then key, as the work spine queued them. */
export function stepsInOrder(steps: readonly StepRow[]): StepRow[] {
  return [...steps].sort((a, b) => a.seq - b.seq || (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
}

// ---------------------------------------------------------------------------
// A file a run produced (a v1:library:artifact index row)
// ---------------------------------------------------------------------------

export interface RunFileRow {
  /** The Library index row's id: what Files opens and the content route serves. */
  artifactId: string;
  /** The backing v1:library:file's short id: what a step's logFileId names. */
  fileId: string;
  /** The index row's own reference to its file, VERBATIM: what Files' open intent matches on. */
  sourceRef: string;
  /** The file's name as the runner stored it: `<stepKey>.log`, or a path with `/` turned into `__`. */
  name: string;
  format: string;
  archived: boolean;
}

export function runFileFromRow(raw: Row): RunFileRow {
  const row = flatten(raw);
  return {
    artifactId: idTail(rowString(row, "id")),
    fileId: idTail(rowString(row, "sourceConceptRef")),
    sourceRef: rowString(row, "sourceConceptRef"),
    name: rowString(row, "title"),
    format: rowString(row, "format"),
    archived: boolOr(row, "archived", false),
  };
}

/** An artifact's name as its step declared it: the runner stores `dist/a.html` as `dist__a.html`. */
export function artifactDisplayName(name: string): string {
  return name.split("__").join("/");
}
