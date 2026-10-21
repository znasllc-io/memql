import { formatBytes } from "../../../kit/format";

// What a machine's installer is doing (memql#6229), as its worker reported it
// on the heartbeat and the cluster stored it on `registration.setup`.
//
// ===========================================================================
// THE INSTALLER IS STILL WORKING AFTER THE MACHINE CONNECTS
// ===========================================================================
// The add-machine command starts the worker first, so the machine connects in
// seconds -- and then the installer spends minutes, sometimes an hour,
// installing the model runtime, downloading the models and setting up
// workbenches. Read from the registration alone that is a machine with no
// runtime, no models and no workbenches, and the wizard said so: "None yet",
// a Download models button for a download already running, and an amber
// "needs attention" over a stage the installer had simply not reached.
//
// This report is the installer's own account. A surface that reads it says
// what the machine is doing, holds a later stage at "after" the one in hand,
// and asks the person for something only when the installer says it could
// not do it.

export type SetupState = "running" | "finished" | "stopped";
export type SetupStageName = "permissions" | "runtime" | "models" | "workbench";
export type SetupStageState = "waiting" | "running" | "done" | "failed" | "skipped";

export interface SetupStage {
  name: SetupStageName;
  state: SetupStageState;
  /** The installer's own sentence: what it is doing, or why it stopped. */
  detail: string;
  /** The models stage only: the model downloading now, its bytes so far and
   *  its WHOLE size across every part (zero while the runtime has not said),
   *  and how many of the stage's models are on the machine of how many. */
  model: string;
  completedBytes: number;
  totalBytes: number;
  modelsDone: number;
  modelsTotal: number;
}

export interface MachineSetup {
  state: SetupState;
  /** In the installer's order. A stage it was not asked to run is absent. */
  stages: SetupStage[];
  /** When the installer last spoke, on the machine's clock. */
  updatedAt: string;
  /** When the cluster last heard the report, on the cluster's clock. */
  reportedAt: string;
}

const STATES: readonly SetupState[] = ["running", "finished", "stopped"];
const STAGE_NAMES: readonly SetupStageName[] = ["permissions", "runtime", "models", "workbench"];
const STAGE_STATES: readonly SetupStageState[] = ["waiting", "running", "done", "failed", "skipped"];

/**
 * How long a running report may go unheard before it is read as stopped.
 * The worker turns a quiet installer into "stopped" itself; this covers the
 * case it cannot, a worker that restarted and stopped carrying the report at
 * all, which leaves the cluster holding the last one it heard.
 */
export const SETUP_QUIET_MS = 2 * 60 * 1000;

function str(o: Record<string, unknown>, key: string): string {
  const v = o[key];
  return typeof v === "string" ? v.trim() : "";
}

function num(o: Record<string, unknown>, key: string): number {
  const v = o[key];
  const n = typeof v === "number" ? v : typeof v === "string" && v.trim() !== "" ? Number(v) : 0;
  return Number.isFinite(n) && n > 0 ? n : 0;
}

function oneOf<T extends string>(value: string, allowed: readonly T[]): T | null {
  return (allowed as readonly string[]).includes(value) ? (value as T) : null;
}

/** The report stored on a registration row, or null when there is none or it
 *  is not one this client can read. */
export function setupFrom(raw: unknown): MachineSetup | null {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) return null;
  const o = raw as Record<string, unknown>;
  const state = oneOf(str(o, "state"), STATES);
  if (state === null) return null;
  const stages: SetupStage[] = [];
  for (const entry of Array.isArray(o["stages"]) ? o["stages"] : []) {
    if (entry === null || typeof entry !== "object") continue;
    const s = entry as Record<string, unknown>;
    const name = oneOf(str(s, "name"), STAGE_NAMES);
    const stageState = oneOf(str(s, "state"), STAGE_STATES);
    if (name === null || stageState === null || stages.some((st) => st.name === name)) continue;
    stages.push({
      name,
      state: stageState,
      detail: str(s, "detail"),
      model: str(s, "model"),
      completedBytes: num(s, "completedBytes"),
      totalBytes: num(s, "totalBytes"),
      modelsDone: num(s, "modelsDone"),
      modelsTotal: num(s, "modelsTotal"),
    });
  }
  return { state, stages, updatedAt: str(o, "updatedAt"), reportedAt: str(o, "reportedAt") };
}

/** The report as it stands at `now`: a running one the cluster has not heard
 *  for SETUP_QUIET_MS is stopped. */
export function setupNow(setup: MachineSetup | null, now: Date): MachineSetup | null {
  if (setup === null || setup.state !== "running") return setup;
  const heard = Date.parse(setup.reportedAt);
  if (Number.isNaN(heard) || now.getTime() - heard <= SETUP_QUIET_MS) return setup;
  return { ...setup, state: "stopped" };
}

/** Whether the installer itself spoke within `ms` of `now`, by its own
 *  clock (`updatedAt`). A stage it has just finished is not on the machine's
 *  registration yet -- models appear when it next reconnects -- and a
 *  surface that read the registration alone would offer to do it again. */
export function setupSpokeWithin(setup: MachineSetup | null, now: Date, ms = SETUP_QUIET_MS): boolean {
  if (setup === null) return false;
  const spoke = Date.parse(setup.updatedAt);
  return !Number.isNaN(spoke) && now.getTime() - spoke <= ms;
}

/** One stage of the report, or null when the installer was not asked to run it. */
export function setupStage(setup: MachineSetup | null, name: SetupStageName): SetupStage | null {
  return setup?.stages.find((stage) => stage.name === name) ?? null;
}

/** The stage the installer is working on now, while it is running. */
export function setupRunningStage(setup: MachineSetup | null): SetupStage | null {
  if (setup === null || setup.state !== "running") return null;
  return setup.stages.find((stage) => stage.state === "running") ?? null;
}

const STAGE_LABEL: Record<SetupStageName, string> = {
  permissions: "the permissions",
  runtime: "the model runtime",
  models: "the models",
  workbench: "the workbenches",
};

/**
 * What a waiting stage is after, in words: "After the models" -- the nearest
 * stage before it the installer has not finished. A waiting stage with
 * nothing unfinished before it is next.
 */
export function setupWaitingAnswer(setup: MachineSetup, name: SetupStageName): string {
  const at = setup.stages.findIndex((stage) => stage.name === name);
  for (let i = at - 1; i >= 0; i--) {
    const before = setup.stages[i]!;
    if (before.state === "waiting" || before.state === "running") return `After ${STAGE_LABEL[before.name]}`;
  }
  return "Up next";
}

/** The models stage's download as a fraction of the whole model, or null when
 *  the runtime has not said how big it is. Never zero for "not said": an
 *  empty bar would claim nothing had moved. */
export function setupDownloadFraction(stage: SetupStage): number | null {
  if (stage.totalBytes <= 0) return null;
  return Math.min(1, stage.completedBytes / stage.totalBytes);
}

/** The bytes line under the bar: "4.1 GiB of 18.2 GiB", with the model's
 *  place in the set when there is more than one. */
export function setupDownloadLabel(stage: SetupStage): string {
  const bytes = stage.totalBytes > 0
    ? `${formatBytes(stage.completedBytes)} of ${formatBytes(stage.totalBytes)}`
    : stage.completedBytes > 0
      ? `${formatBytes(stage.completedBytes)} so far`
      : "Starting";
  if (stage.modelsTotal <= 1) return bytes;
  return `Model ${Math.min(stage.modelsDone + 1, stage.modelsTotal)} of ${stage.modelsTotal} · ${bytes}`;
}

// ONE SET OF WORDS FOR EVERY SURFACE. The wizard's floor, the machine page and
// the machines list say the same thing about the same install, so the words
// live here rather than in each.
const RUNNING_WORDS: Record<SetupStageName, string> = {
  // Not the installer working: it waits for the person, on the machine.
  permissions: "Waiting for permissions",
  runtime: "Installing the model runtime",
  models: "Downloading models",
  workbench: "Setting up workbenches",
};

/** What the installer is doing while a stage runs, in a few words. */
export function setupRunningWords(name: SetupStageName): string {
  return RUNNING_WORDS[name];
}

/** The installer's download in flight, or null when it is not downloading. */
export function setupDownloading(setup: MachineSetup | null): SetupStage | null {
  const stage = setupRunningStage(setup);
  return stage?.name === "models" ? stage : null;
}

/**
 * One line for a machine's page: what the installer is doing now, or that it
 * stopped before it finished. "" when there is nothing to say -- no report,
 * or an install that finished, whose results are the machine's own facts.
 */
export function setupSummary(setup: MachineSetup | null): string {
  if (setup === null) return "";
  if (setup.state === "stopped") return "Stopped before it finished";
  if (setup.state !== "running") return "";
  const stage = setupRunningStage(setup);
  if (stage === null) return "In progress";
  if (stage.name === "models" && stage.model !== "") return `Downloading ${stage.model} · ${setupDownloadLabel(stage)}`;
  return RUNNING_WORDS[stage.name];
}
