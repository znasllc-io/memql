import { permissionDecision } from "../rows";
import type { StopState } from "../../../kit/Rail";
import { formatFreshness } from "../../../kit/format";
import { isWorkerOnline } from "../online";
import { computerUseStatus, hasRoundTrip, isRevoked, machineName, type MachineRow } from "../rows";
import { isChatModel, machineModelsFrom, type ModelPull } from "../machines/models";
import {
  setupDownloadLabel,
  setupNow,
  setupRunningWords,
  setupSpokeWithin,
  setupStage,
  setupWaitingAnswer,
  type MachineSetup,
  type SetupStage,
} from "../machines/setup";
import type { ModelResponse } from "./useModelResponse";
import {
  INSTALL_PLATFORM_LABEL,
  setupCommand,
  workbenchSetupCommand,
  type InstallPlatform,
} from "./install";

// WHAT THE GUIDED INSTALL KNOWS, as functions over values (design record
// 2026-09-08-cockpit-install-wizard, D2 to D6).
//
// ===========================================================================
// PURE, BECAUSE THE CLAIMS ARE ABOUT THE READING AND NOT THE PICTURE
// ===========================================================================
// "The machine that arrived is the one we minted for", "two heartbeats past
// the registration is steady", "a missing permission is a repair the person
// makes and the feed settles", "Cancel after a mint asks which of two things"
// are statements about this file. Asserting them through a rendered rail
// would make each one an assertion about the rail as well, and the rail has
// its own tests. No React here.
//
// ===========================================================================
// THE ORDER IS A LAW HERE, AND THE RAIL MOVES BY WHAT HAPPENS
// ===========================================================================
// The first-run rail is a set of doors that may be opened in any order. This
// one is a sequence: no command without a token, no connection without the
// command having run, no check without a registration. So the unreached stops
// draw `ahead`, and the rail moves on a mint, a registration, a heartbeat --
// never on a click. There is still no Next: the mint is the bar's forward
// act, and everything after it is answered by the cluster.

export type Phase = "describe" | "minting" | "waiting" | "connected";

export interface Draft {
  name: string;
  platform: InstallPlatform;
  computerUse: boolean;
  inference: boolean;
  workbench?: boolean;
  userLocal?: boolean;
}

export const EMPTY_DRAFT: Draft = { name: "", platform: "mac", computerUse: false, inference: false, userLocal: false };

/** What the mint returned: the plain token, shown once, and the identity the
 *  machine will authenticate as -- which is how its registration is matched. */
export interface Mint {
  token: string;
  identityId: string;
}

export interface FlowFacts {
  draft: Draft;
  /** A live connection to the cluster. Nothing can be minted without one. */
  connected: boolean;
  minting: boolean;
  /** The server's refusal, verbatim, or "". */
  mintError: string;
  mint: Mint | null;
  /** When the mint landed, for the "this is taking a while" reading. */
  mintedAt: Date | null;
  /** The registration matched to `mint.identityId`, or null while waiting. */
  machine: MachineRow | null;
  /** Heartbeats heard since the registration first appeared. */
  beats: number;
  /** Whether Cancel has been pressed while a token exists, and is waiting for
   *  its answer: cancelling REVOKES a live credential, so it asks first. */
  cancelAsked: boolean;
  /** Whether Leave has been pressed while waiting, and is waiting for its
   *  answer. Optional so a caller that never asks need not say so. */
  leaveAsked?: boolean;
  /** The revoke's refusal, verbatim, or "". */
  revokeError: string;
  revoking: boolean;
  now: Date;
}

export function phaseOf(facts: Pick<FlowFacts, "minting" | "mint" | "machine">): Phase {
  if (facts.minting) return "minting";
  if (facts.mint === null) return "describe";
  if (facts.machine === null) return "waiting";
  return "connected";
}

// ---------------------------------------------------------------------------
// Matching the registration
// ---------------------------------------------------------------------------

/**
 * Whether two ids name the same row across the bare/canonical seam.
 *
 * The mint reply and a bare-ified graph row are two egress seams, and the
 * client contract says the engine owns both directions -- so this compares
 * the trailing segment, the way the presence map in live/machines.tsx already
 * reads a registration id, rather than composing or parsing anything. Two
 * empty ids are not the same id: an unmatched registration carries "" and a
 * flow with no mint carries "", and equating them would match every machine.
 */
export function sameId(a: string, b: string): boolean {
  const left = bare(a);
  const right = bare(b);
  return left !== "" && left === right;
}

function bare(id: string): string {
  const trimmed = id.trim();
  const at = trimmed.lastIndexOf(":");
  return at === -1 ? trimmed : trimmed.slice(at + 1);
}

/**
 * The registration this flow minted for: the unrevoked row whose
 * `identityId` is the mint's. MATCHED, never counted (D3).
 *
 * Counting the population lied in both directions. Two people adding
 * machines at once, or any other machine of yours reconnecting, grew the
 * count with the wrong machine; a machine of yours revoked elsewhere shrank
 * it and hid a real arrival. A revoked row never matches: a token revoked
 * from the cancel question and then pasted anyway must not read as success.
 */
export function matchRegistration(rows: readonly MachineRow[], identityId: string): MachineRow | null {
  if (identityId.trim() === "") return null;
  return rows.find((row) => !isRevoked(row) && sameId(row.identityId, identityId)) ?? null;
}

// ---------------------------------------------------------------------------
// Heartbeats
// ---------------------------------------------------------------------------

/** How many heartbeats past the registration count as "staying". Two, because
 *  that is the online window (30 s at the 15 s cadence): a machine heard from
 *  across one whole window is one that the service kept alive, not one whose
 *  installer happened to still be running. */
export const STEADY_BEATS = 2;

export function isSteady(machine: MachineRow | null, beats: number, now: Date): boolean {
  return machine !== null && beats >= STEADY_BEATS && isWorkerOnline(machine, now);
}

// ---------------------------------------------------------------------------
// The checks (D4, D5)
// ---------------------------------------------------------------------------

/** The rail's own vocabulary, so a check draws with the rail's marks:
 *  `done` settled, `current` the machine is moving, `open` waiting on the
 *  person, `skipped` with a reason, `stopped` something went wrong, `unknown`
 *  the cockpit did not say, `ahead` not reachable until another check is. */
export type CheckState = Extract<StopState, "done" | "current" | "open" | "skipped" | "stopped" | "unknown" | "ahead">;

export interface Check {
  id: "connection" | "build" | "permissions" | "display" | "runtime" | "models" | "response" | "workbench";
  name: string;
  state: CheckState;
  /** The state in words, in a few: the answer once settled, what is being
   *  waited on otherwise. */
  answer: string;
  /** What the PERSON does, when there is something to do: one sentence. */
  repair?: string;
  /** The exact command the repair takes, when it takes one. */
  command?: string;
  /** An act the OS can take on the person's behalf, offered in the footer:
   *  pull the recommended models onto the machine (D13), or ask the
   *  served model something to prove the whole path (D14). */
  act?: "pullRecommended" | "retryResponse";
  /** The pull in flight, drawn as progress under the Models line. */
  pull?: ModelPull;
  /** The installer's own download (memql#6229), drawn as progress under the
   *  Models line: whole-model bytes, where `pull` counts the part in flight. */
  download?: SetupStage;
  /** What the floor says while this check is the one moving: "Downloading
   *  models" rather than "Checking", and the measure of it. */
  footer?: { state: string; detail: string };
}

/** The worker log, read on the machine: where a worker that cannot connect says why. */
const WORKER_LOG_COMMAND = "tail -n 40 ~/.memql/state/worker.log";

/** What the flow knows about pulls onto the matched machine: the one still
 *  running, if any. Read from the `modelPullsForWorker` feed by the hook. */
export interface PullFacts {
  live: ModelPull | null;
  failed?: ModelPull | null;
  feedError?: string;
  loading?: boolean;
  /** The cluster's refusal to START the recommended pulls, verbatim, or "".
   *  It belongs on the Models line, not in a banner above the checks. */
  startError?: string;
}

const NO_PULLS: PullFacts = { live: null };

// ONLY WHAT WAS ASKED FOR (memql#6214). The checks used to list every
// capability the machine has -- commands and files, the build, the apps --
// whatever was chosen, each with a sentence, so a machine added for local
// models read as ten lines of prose. A check is here when the person picked
// the feature it checks, and it says its state in a few words; what to do
// appears only when there is something to do. What else the machine has --
// its apps among them -- is on its own page.
export function checksFor(
  draft: Draft,
  machine: MachineRow,
  beats: number,
  now: Date,
  pulls: PullFacts = NO_PULLS,
  response: ModelResponse = { state: "waiting" },
): Check[] {
  const checks: Check[] = [connectionCheck(machine, beats, now)];
  // The installer's own account (memql#6229), as it stands NOW: a running
  // report the cluster stopped hearing is an installer that stopped.
  const setup = setupNow(machine.setup, now);
  // THE INSTALLER HAS NOT SPOKEN YET (memql#6235). The registration lands
  // before the first heartbeat that carries the installer's report, so for
  // those seconds a machine being set up for local models read as a runtime
  // with nothing on it -- and Download models was offered, and pressed, for
  // the download the installer was about to start. Until two heartbeats have
  // gone by with no report, a line the installer is about to answer waits
  // for it. A cockpit that never reports (an older one) is read as before.
  const awaiting = setup === null && beats < STEADY_BEATS;
  if (draft.computerUse) checks.push(buildCheck(machine));
  if (draft.computerUse && draft.platform === "mac") checks.push(permissionsCheck(machine, setup, awaiting));
  if (draft.computerUse && draft.platform === "linux") checks.push(displayCheck(machine));
  if (draft.inference) {
    checks.push(...inferenceChecks(machine, pulls, draft, setup, awaiting, now));
    const model = machineModelsFrom(machine.reportedLabels).find(isChatModel);
    const modelsState = checks.find(check => check.id === "models")?.state;
    const available = modelsState === "done";
    checks.push(modelsState === "skipped"
      // A machine that will have no models has nothing to answer with, and
      // a check held "after the models" forever would keep the wizard from
      // ever saying the machine is ready.
      ? { id: "response", name: "Model response", state: "skipped", answer: "No models here" }
      : !available
      ? { id: "response", name: "Model response", state: "ahead", answer: "After the models" }
      : !model
        ? { id: "response", name: "Model response", state: "open", answer: "No text model", repair: "Download a text model to finish this check.", act: "pullRecommended" }
        : response.state === "passed"
          ? { id: "response", name: "Model response", state: "done", answer: "Answered" }
          : response.state === "failed"
            ? { id: "response", name: "Model response", state: "stopped", answer: "No answer", repair: response.error, act: "retryResponse" }
            : { id: "response", name: "Model response", state: "current", answer: response.state === "running" ? "Asking the model" : "Waiting to ask the model" });
  }
  // LAST, BECAUSE THE INSTALLER DOES IT LAST (memql#6229): read top to bottom,
  // the list is the order things happen, so "After the models" points up.
  if (draft.workbench) checks.push(workbenchCheck(machine, draft, setup, awaiting));
  return checks;
}

/** A line the installer will answer, before its first report has arrived. */
const AWAITING_INSTALLER = { state: "current", answer: "Waiting for the installer" } as const;

/** The cluster's own round trip as a figure -- "11 ms, checked 40s ago" --
 *  or "" when never measured. */
export function roundTripFigure(machine: MachineRow, now: Date): string {
  if (!hasRoundTrip(machine)) return "";
  return `${machine.rttMs} ms, checked ${formatFreshness(machine.rttAt, now)}`;
}

function connectionCheck(machine: MachineRow, beats: number, now: Date): Check {
  if (!isWorkerOnline(machine, now)) {
    return {
      id: "connection",
      name: "Connection",
      state: "stopped",
      answer: `Offline · last heard ${formatFreshness(machine.lastSeenAt, now)}`,
      repair: "The worker log on the machine says why.",
      command: WORKER_LOG_COMMAND,
    };
  }
  if (beats >= STEADY_BEATS) {
    return { id: "connection", name: "Connection", state: "done", answer: hasRoundTrip(machine) ? `Online · ${machine.rttMs} ms` : "Online" };
  }
  return { id: "connection", name: "Connection", state: "current", answer: beats === 0 ? "Waiting for a heartbeat" : "Checking the connection" };
}

function buildCheck(machine: MachineRow): Check {
  const tag = machine.buildTag.trim();
  if (tag === "headless") {
    return {
      id: "build",
      name: "Computer use",
      state: "stopped",
      answer: "Headless build installed",
      repair: "Run the install command again; it installs the computer-use build in place.",
    };
  }
  if (tag === "computeruse" || machine.capabilities.includes("COMPUTERUSE")) {
    return { id: "build", name: "Computer use", state: "done", answer: "Installed" };
  }
  return { id: "build", name: "Computer use", state: "unknown", answer: "Build not reported" };
}

const WORKBENCH_REPAIR = "Start Docker with at least 3 CPUs and 4 GiB, then run this on the machine.";

// THE INSTALLER SAYS WHETHER IT IS THE PERSON'S TURN (memql#6229). The
// install command sets workbenches up itself, after the models, so while it
// runs this check waits its turn ("After the models") or says it is being
// done, and the command is offered only once the installer says it could not
// do it -- in its own words -- or stopped before it got here.
function workbenchCheck(machine: MachineRow, draft: Draft, setup: MachineSetup | null, awaiting: boolean): Check {
  const base = { id: "workbench", name: "Fleet workbenches" } as const;
  if (machine.capabilities.includes("WORKBENCH") && (machine.workbenchContract ?? 0) >= 1) {
    return { ...base, state: "done", answer: "Ready" };
  }
  if (awaiting) return { ...base, ...AWAITING_INSTALLER };
  const command = workbenchSetupCommand(draft.userLocal);
  const stage = setupStage(setup, "workbench");
  if (setup !== null && stage !== null) {
    if (setup.state === "running" && stage.state === "waiting") {
      return { ...base, state: "ahead", answer: setupWaitingAnswer(setup, "workbench") };
    }
    if (setup.state === "running" && stage.state === "running") {
      return { ...base, state: "current", answer: "Setting up", footer: { state: setupRunningWords("workbench"), detail: "" } };
    }
    if (stage.state === "done") {
      // Set up, and not yet reported: the machine names the feature when it
      // next reconnects, which the installer asks it to do.
      return { ...base, state: "current", answer: "Waiting for the machine to report it", footer: { state: "Finishing workbenches", detail: "" } };
    }
    if ((stage.state === "failed" || stage.state === "skipped") && stage.detail !== "") {
      return { ...base, state: "open", answer: "Not available yet", repair: stage.detail, command };
    }
    if (setup.state === "stopped") {
      return { ...base, state: "open", answer: "Not set up", repair: `The installer stopped before this. ${WORKBENCH_REPAIR}`, command };
    }
  }
  return { ...base, state: "open", answer: "Not available yet", repair: WORKBENCH_REPAIR, command };
}

/** Where a person turns the permissions on, on the Mac, whenever they like. */
export const PERMISSIONS_MENU = "MemQL in the menu bar → macOS permissions → Set Up Permissions…";

function turnOnFromMenu(names: readonly string[]): string {
  return `Turn ${names.length > 1 ? "them" : "it"} on from ${PERMISSIONS_MENU}`;
}

// THE PERSON'S STEP, IN THE INSTALLER'S ORDER (memql#6235). On a computer-use
// Mac the installer asks for the permissions FIRST and waits: the person
// allows both in MemQL's Permissions window, or says there -- or in Terminal
// -- that setup should go on without them. Only then do the long unattended
// stages start. So while the installer waits this line is the person's turn,
// a skip is theirs to make and settles the line rather than failing it, and
// what is missing always comes from the worker's own probe, never from the
// installer's sentence. Nothing else in the sequence can be skipped.
function permissionsCheck(machine: MachineRow, setup: MachineSetup | null, awaiting: boolean): Check {
  const base = { id: "permissions", name: "macOS permissions" } as const;
  const p = machine.permissions;
  const evidence = [
    { name: "Accessibility", state: permissionDecision(p, "accessibility") },
    { name: "Screen Recording", state: permissionDecision(p, "screenRecording") },
  ];
  const denied = evidence.filter((s) => s.state === "denied").map((s) => s.name);
  const unknown = evidence.filter((s) => s.state === "unknown").map((s) => s.name);
  if (denied.length === 0 && unknown.length === 0) return { ...base, state: "done", answer: "Granted" };
  const missing = evidence.filter((s) => s.state !== "granted").map((s) => s.name);
  const stage = setupStage(setup, "permissions");
  if (setup?.state === "running" && stage?.state === "running") {
    const label = machineName(machine);
    // The window's own words for the way on: "Skip for now" while neither is
    // allowed, "Continue without Screen Recording" once one is.
    const choice = missing.length > 1 ? "Skip for now" : `Continue without ${missing[0]}`;
    return {
      ...base,
      state: "open",
      answer: missing.length > 1 ? "Waiting for you" : `Waiting for ${missing[0]}`,
      repair: `Allow MemQL in the Permissions window on ${label}, or choose ${choice} there.`,
      footer: { state: `Waiting for you on ${label}`, detail: "" },
    };
  }
  if (awaiting) return { ...base, ...AWAITING_INSTALLER };
  if (stage?.state === "skipped") {
    return { ...base, state: "skipped", answer: `${missing.join(" and ")} skipped`, repair: turnOnFromMenu(missing) };
  }
  if (setup?.state === "running" && stage?.state === "waiting") {
    return { ...base, state: "ahead", answer: setupWaitingAnswer(setup, "permissions") };
  }
  if (stage?.state === "failed" && stage.detail !== "") {
    return { ...base, state: "open", answer: `${missing.join(" and ")} not granted`, repair: stage.detail };
  }
  if (denied.length > 0) {
    return {
      ...base,
      state: "open",
      answer: `${denied.join(" and ")} not granted`,
      repair: `${turnOnFromMenu(denied)}, or allow MemQL under System Settings → Privacy & Security → ${denied.join(" and ")}.`,
    };
  }
  return {
    ...base,
    state: "unknown",
    answer: `${unknown.join(" and ")} not reported yet`,
    repair: "This Cockpit build does not report its permissions; update Cockpit on the machine.",
  };
}

function displayCheck(machine: MachineRow): Check {
  const status = computerUseStatus(machine);
  return {
    id: "display",
    name: "Display",
    state: status.state === "available" ? "done" : status.state === "unknown" ? "unknown" : "skipped",
    answer: status.answer,
  };
}

function runtimeCheck(machine: MachineRow, draft: Draft, setup: MachineSetup | null, models: number, awaiting: boolean): Check {
  const base = { id: "runtime", name: "Model runtime" } as const;
  const runtimes = machine.hardware.runtimes.filter((r) => r.name !== "");
  if (models > 0 || runtimes.length > 0) {
    return { ...base, state: "done", answer: runtimes.length > 0 ? runtimes.map((r) => `${r.name} ${r.version}`.trim()).join(", ") : "Serving models" };
  }
  if (awaiting) return { ...base, ...AWAITING_INSTALLER };
  const command = setupCommand(draft.userLocal, true);
  const stage = setupStage(setup, "runtime");
  if (setup !== null && stage !== null) {
    if (setup.state === "running" && stage.state === "running") {
      return { ...base, state: "current", answer: "Installing", footer: { state: setupRunningWords("runtime"), detail: "" } };
    }
    if (setup.state === "running" && stage.state === "waiting") {
      return { ...base, state: "ahead", answer: setupWaitingAnswer(setup, "runtime") };
    }
    // A machine the hardware floor rules out is not a failure: nothing will
    // fix it short of other hardware, so it is said and settled.
    if (stage.state === "skipped" && stage.detail !== "") {
      return { ...base, state: "skipped", answer: "Not available here", repair: stage.detail };
    }
    if (stage.state === "failed" && stage.detail !== "") {
      return { ...base, state: "stopped", answer: "Couldn’t install", repair: stage.detail, command };
    }
  }
  return { ...base, state: "open", answer: "Not installed", repair: "Run this on the machine to install it and download the models.", command };
}

function inferenceChecks(machine: MachineRow, pulls: PullFacts, draft: Draft, setup: MachineSetup | null, awaiting: boolean, now: Date): Check[] {
  const models = machineModelsFrom(machine.reportedLabels);
  const runtime = runtimeCheck(machine, draft, setup, models.length, awaiting);
  const startError = (pulls.startError ?? "").trim();
  const stage = setupStage(setup, "models");
  let modelsCheck: Check;
  if (pulls.live !== null) {
    // A PARTIAL START keeps both facts on the one line: what is downloading,
    // and why the rest of the set did not start.
    modelsCheck = {
      id: "models", name: "Models", state: "current", answer: `Downloading ${pulls.live.model}`, pull: pulls.live,
      footer: { state: setupRunningWords("models"), detail: pulls.live.model },
      ...(startError === "" ? {} : { repair: `The rest did not start: ${startError}` }),
    };
  } else if (setup?.state === "running" && stage?.state === "running") {
    // THE INSTALLER IS DOWNLOADING THEM (memql#6229): said, measured, and no
    // Download models -- that would start the same gigabytes a second time.
    const what = stage.model === "" ? "the models" : stage.model;
    modelsCheck = {
      id: "models", name: "Models", state: "current", answer: `Downloading ${what}`, download: stage,
      footer: { state: setupRunningWords("models"), detail: stage.model === "" ? "" : `${stage.model} · ${setupDownloadLabel(stage)}` },
    };
  } else if (startError !== "") {
    // A REFUSED START IS THE MODELS CHECK'S OWN STATE, never a banner over the
    // checks: it is about this line, and Download models in the footer is
    // the way to try again.
    modelsCheck = { id: "models", name: "Models", state: "stopped", answer: `Couldn’t start: ${startError}`, act: "pullRecommended" };
  } else if (pulls.feedError) {
    modelsCheck = {
      id: "models", name: "Models", state: "stopped",
      answer: "Can’t read download progress",
      repair: `${pulls.feedError.replace(/[.\s]+$/, "")}. Downloads continue on the machine.`,
    };
  } else if (pulls.loading) {
    modelsCheck = { id: "models", name: "Models", state: "current", answer: "Checking downloads" };
  } else if (pulls.failed && !models.some((model) => model.modelId === pulls.failed?.model)) {
    const latest = pulls.failed;
    const reason = latest.errorMessage.trim() || latest.statusLine.trim();
    modelsCheck = {
      id: "models", name: "Models", state: "stopped",
      answer: latest.status === "cancelled" ? `Stopped: ${latest.model}` : `Download failed${reason === "" ? "" : `: ${reason}`}`,
      act: "pullRecommended",
    };
  } else if (models.length > 0) {
    modelsCheck = { id: "models", name: "Models", state: "done", answer: models.map((m) => m.modelId).join(", ") };
  } else if (awaiting) {
    modelsCheck = { id: "models", name: "Models", ...AWAITING_INSTALLER };
  } else if (stage?.state === "done" && setupSpokeWithin(setup, now)) {
    // Downloaded, and not reported yet: the machine names its models when it
    // next reconnects, which the installer asks it to do. Download models
    // here would fetch what is already on the disk.
    modelsCheck = { id: "models", name: "Models", state: "current", answer: "Waiting for the machine to report them", footer: { state: "Finishing models", detail: "" } };
  } else if (setup !== null && stage !== null && setup.state === "running" && stage.state === "waiting") {
    modelsCheck = { id: "models", name: "Models", state: "ahead", answer: setupWaitingAnswer(setup, "models") };
  } else if (setup?.state === "stopped" && stage?.state === "running" && runtime.state === "done") {
    // The installer quit mid-download. What it fetched stays on the machine,
    // and the runtime resumes from it, so the footer's Download models is the
    // way on rather than a fresh start.
    modelsCheck = {
      id: "models", name: "Models", state: "open", answer: "Download stopped",
      repair: "The installer stopped before the models finished. Download models picks up where it left off.",
      act: "pullRecommended",
    };
  } else if (stage?.state === "failed" && stage.detail !== "") {
    // The installer's own words for why, and Download models in the footer
    // to try again from here.
    modelsCheck = { id: "models", name: "Models", state: "stopped", answer: "Download failed", repair: stage.detail, act: "pullRecommended" };
  } else if (stage?.state === "skipped" && stage.detail !== "" && runtime.state === "skipped") {
    modelsCheck = { id: "models", name: "Models", state: "skipped", answer: "Not available here" };
  } else if (stage?.state === "skipped" && stage.detail !== "" && runtime.state === "done") {
    // Skipped by this machine's own policy (models.pull is off): the
    // person's to change, on the machine.
    modelsCheck = { id: "models", name: "Models", state: "open", answer: "Not downloaded", repair: stage.detail };
  } else if (runtime.state !== "done") {
    modelsCheck = { id: "models", name: "Models", state: "ahead", answer: "After the runtime" };
  } else {
    // THE RUNTIME IS THERE AND NOTHING IS ON IT: the footer's Download models
    // pulls the set the catalog recommends for this machine (D13).
    modelsCheck = {
      id: "models",
      name: "Models",
      state: "open",
      answer: "None yet",
      repair: "The recommended models take several gigabytes.",
      act: "pullRecommended",
    };
  }
  return [runtime, modelsCheck];
}

/** Missing optional diagnostics may settle, but requested macOS permissions
 * must be measured. An unknown grant cannot make setup ready. */
export function checkIsSettled(check: Check): boolean {
  // Skipped only ever by the person, during setup: their choice settles it.
  if (check.id === "permissions") return check.state === "done" || check.state === "skipped";
  return check.state === "done" || check.state === "skipped" || check.state === "unknown";
}

export function checksSettled(checks: readonly Check[]): boolean {
  return checks.length > 0 && checks.every(checkIsSettled);
}

// ---------------------------------------------------------------------------
// The stops (D2, D3)
// ---------------------------------------------------------------------------

export type StopId = "machine" | "install" | "connect" | "checks";

export interface FlowStop {
  id: StopId;
  name: string;
  state: StopState;
  /** What the stop is for, under its name while open. */
  sentence: string;
  /** The state in words, on the collapsed line. */
  answer: string;
}

export function draftSummary(draft: Draft): string {
  const parts = [INSTALL_PLATFORM_LABEL[draft.platform]];
  if (draft.inference) parts.push("local models");
  if (draft.computerUse) parts.push("computer use");
  if (draft.workbench) parts.push("workbenches");
  const name = draft.name.trim();
  return [name, ...parts].filter((part) => part !== "").join(" · ");
}

export function stopsFor(facts: FlowFacts, checks: readonly Check[]): FlowStop[] {
  const phase = phaseOf(facts);
  const machine = facts.machine;
  const host = machine === null ? "" : machine.hostname.trim() || machine.name.trim();
  const settled = checksSettled(checks);

  const machineStop: FlowStop = {
    id: "machine",
    name: "This machine",
    sentence: "Name and features",
    state: phase === "describe" ? "open" : phase === "minting" ? "current" : "done",
    answer: phase === "describe" ? (facts.mintError === "" ? "" : "Couldn’t prepare installation") : draftSummary(facts.draft),
  };

  const installStop: FlowStop = {
    id: "install",
    name: "Install",
    sentence: "One command in Terminal",
    state: phase === "describe" || phase === "minting" ? "ahead" : phase === "waiting" ? "open" : "done",
    answer:
      phase === "waiting"
        ? "Run the command"
        : phase === "connected"
          ? host === ""
            ? "Installed"
            : `Installed on ${host}`
          : "",
  };

  const connectStop: FlowStop = {
    id: "connect",
    name: "Connect",
    sentence: "Waiting for it to check in",
    state: phase === "describe" || phase === "minting" ? "ahead" : phase === "waiting" ? "current" : "done",
    answer:
      phase === "waiting"
        ? "Listening"
        : phase === "connected" && machine !== null
          ? connectedAnswer(machine)
          : "",
  };

  // THE CHECKS STOP'S MARK IS THE CHECKS' OWN VERDICT: moving while the machine
  // is (a heartbeat, a pull), held while only the person's hand is missing (a
  // permission, a command), lit when nothing more is asked of anybody.
  const anyCurrent = checks.some((c) => c.state === "current");
  const anyOpen = checks.some((c) => c.state === "open");
  const anyStopped = checks.some((c) => c.state === "stopped");
  const checksStop: FlowStop = {
    id: "checks",
    name: "Checks",
    sentence: "Making sure it works",
    state:
      phase !== "connected"
        ? "ahead"
        : settled
          ? "done"
          : anyStopped && !anyCurrent
            ? "stopped"
            : anyOpen && !anyCurrent
              ? "open"
              : "current",
    answer: phase !== "connected" ? "" : checksAnswer(checks),
  };

  return [machineStop, installStop, connectStop, checksStop];
}

function connectedAnswer(machine: MachineRow): string {
  return `Connected as ${machineName(machine)}`;
}

/**
 * The Checks stop's collapsed line: a COUNT and the name of what is next, never
 * the next check's own sentence. That sentence is drawn one line below on the
 * checks' own rail the moment the stop is open, and the rail's rule is that a
 * line says something the line above does not.
 */
function checksAnswer(checks: readonly Check[]): string {
  const settled = checks.filter(checkIsSettled).length;
  if (settled === checks.length) return "All set";
  const count = `${settled} of ${checks.length} done`;
  const busy = checks.find((c) => c.state === "current" && c.footer !== undefined)?.footer;
  if (busy) return `${count} · ${busy.state}`;
  const next = checks.find((c) => !checkIsSettled(c) && c.state !== "ahead");
  return next === undefined ? count : `${count} · ${next.name} next`;
}

/** The stop the rail opens when nobody has chosen one: the person's own
 *  stop, or the one the cluster is answering. */
export function openStopFor(stops: readonly FlowStop[]): StopId {
  const open = stops.find((s) => s.state === "open");
  if (open) return open.id;
  const current = stops.find((s) => s.state === "current" || s.state === "stopped");
  if (current) return current.id;
  return stops[stops.length - 1]?.id ?? "machine";
}

// ---------------------------------------------------------------------------
// The action bar (D6, interface rule 12)
// ---------------------------------------------------------------------------

export type ActId = "cancel" | "mint" | "leave" | "keepWaiting" | "revokeAndLeave" | "leaveKeepToken" | "back" | "done" | "retryResponse" | "pullRecommended";

export interface FlowAct {
  id: ActId;
  label: string;
  tone: "quiet" | "primary" | "danger";
  busy?: boolean;
  /** A text action rather than a button: the way out beside the one act. */
  text?: boolean;
}

export interface FlowBar {
  state: string;
  detail: string;
  tone: "live" | "paused" | "busy" | "none";
  /** Prose the bar carries in place of nothing: the cancel question. */
  question: string;
  acts: FlowAct[];
}

/** How long a wait is before the Connect stop names the usual causes. */
export const LONG_WAIT_MS = 10 * 60 * 1000;

export function waitedLong(facts: Pick<FlowFacts, "mintedAt" | "now">): boolean {
  return facts.mintedAt !== null && facts.now.getTime() - facts.mintedAt.getTime() >= LONG_WAIT_MS;
}

/** A name that starts a sentence: "the machine" is a phrase, "The machine" a subject. */
function capitalized(words: string): string {
  return words.charAt(0).toUpperCase() + words.slice(1);
}

export function barFor(facts: FlowFacts, checks: readonly Check[], pulling = false): FlowBar {
  const phase = phaseOf(facts);
  const name = facts.draft.name.trim();
  const label = name === "" ? "the machine" : name;

  if (phase === "minting") {
    return { state: "Preparing installation", detail: label, tone: "busy", question: "", acts: [] };
  }

  if (phase === "describe") {
    const canMint = facts.connected && name !== "";
    return {
      // THE SAME TWO WORDS EVERY WIZARD USES. What is still needed while the
      // step cannot go forward, and "Ready to ..." the moment it can -- so the
      // words change at the same instant the forward act appears beside them.
      state: facts.mintError !== "" ? "Couldn’t prepare installation" : canMint ? "Ready to continue" : "Name your machine",
      detail:
        facts.mintError !== ""
          ? "Try again."
          : !facts.connected
            ? "Reconnect to the cluster to continue."
            : "",
      tone: "none",
      question: "",
      acts: [
        { id: "cancel", label: "Cancel", tone: "quiet", text: true },
        // ABSENT, NEVER DISABLED (rule 12): a mint with no name is refused by
        // the engine, so it is not offered until there is one.
        ...(canMint ? [{ id: "mint" as const, label: "Continue", tone: "primary" as const }] : []),
      ],
    };
  }

  // THE FLOOR HAS TWO VERBS, and they are the ones every add wizard uses:
  //
  //   CANCEL   undoes the whole thing. A token exists by now, so that means
  //            REVOKING it -- a credential nobody will use should not stay
  //            live -- and it asks first.
  //   LEAVE    goes, and everything stays: the token keeps working, and the
  //            machine appears in Machines by itself when the install
  //            finishes. It asks ONCE too, for the one thing leaving costs
  //            here and nowhere else: the token is shown only on this page.
  //
  // This was one question with three answers ("Keep waiting", "Revoke the
  // token and leave", "Leave, keep the token") behind a single Cancel, so the
  // way to leave WITHOUT cancelling was found by pressing Cancel.
  if (phase === "waiting") {
    if (facts.cancelAsked) {
      return {
        state: "Cancel?",
        detail: "",
        tone: "paused",
        question:
          facts.revokeError === ""
            ? `Cancel adding ${label}? The install command stops working.`
            : `The token was not revoked: ${facts.revokeError}. It still works; try again, or leave and keep it.`,
        acts: [
          { id: "keepWaiting", label: "Keep setting up", tone: "quiet", text: true },
          // A REFUSED REVOKE MUST NOT TRAP ANYBODY. The token is still live and
          // the cluster will not take it back, so going with it is offered
          // right here rather than two questions away.
          ...(facts.revokeError === "" ? [] : [{ id: "leaveKeepToken" as const, label: "Leave, keep the token", tone: "quiet" as const, text: true }]),
          { id: "revokeAndLeave", label: "Cancel setup", tone: "danger", busy: facts.revoking },
        ],
      };
    }
    if (facts.leaveAsked === true) {
      return {
        state: "Back to Machines?",
        detail: "",
        tone: "paused",
        question: `The install command won’t be shown again. ${capitalized(label)} appears in Machines when it connects.`,
        acts: [
          { id: "keepWaiting", label: "Stay", tone: "quiet", text: true },
          { id: "leaveKeepToken", label: "Back to Machines", tone: "primary" },
        ],
      };
    }
    return {
      state: `Waiting for ${label}`,
      detail: waitedLong(facts) ? "Taking longer than usual." : "",
      tone: "busy",
      question: "",
      acts: [
        { id: "cancel", label: "Cancel setup", tone: "quiet", text: true },
        { id: "leave", label: "Back to Machines", tone: "quiet" },
      ],
    };
  }

  // connected
  const machine = facts.machine;
  const shown = machine === null ? label : machineName(machine);
  const settled = checksSettled(checks);
  // WHOSE TURN IT IS (rule 12). A stopped check needs somebody whatever else
  // is moving; an open one -- a permission to grant, an app to sign in to --
  // is the person's turn once nothing else is, and is not drawn as the
  // cluster working.
  const moving = checks.some((c) => c.state === "current");
  const attention = checks.find((c) => c.state === "stopped") ?? (moving ? undefined : checks.find((c) => c.state === "open"));
  // THE WORK IN HAND, IN ITS OWN WORDS: "Downloading models", not
  // "Checking", while the machine is busy with it (memql#6229).
  const busy = attention ? undefined : checks.find((c) => c.state === "current" && c.footer !== undefined)?.footer;
  const pending = attention ?? checks.find((c) => !checkIsSettled(c) && c.state !== "ahead");
  const repair = checks.find((c) => c.act !== undefined)?.act;
  const canRepair = facts.connected && machine !== null && isWorkerOnline(machine, facts.now);
  const forward: FlowAct[] = settled
    ? [{ id: "done", label: "Done", tone: "primary" }]
    : pulling
      ? [{ id: "pullRecommended", label: "Download models", tone: "primary", busy: true }]
      : canRepair && repair
        ? [{ id: repair, label: repair === "retryResponse" ? "Retry" : "Download models", tone: "primary" }]
        : [];
  return {
    // A line waiting on the person's hand on the machine says so in its own
    // words ("Waiting for you on studio") rather than as a fault.
    state: settled ? "Ready" : attention ? (attention.state === "open" && attention.footer ? attention.footer.state : `${attention.name} needs attention`) : busy ? busy.state : "Checking",
    // The detail is the check that is MOVING; one that needs somebody names
    // itself in the state, and says why on its own line in the checks.
    detail: settled ? `${shown} is ready.` : attention || pending === undefined ? "" : busy ? busy.detail : `${pending.name}: ${pending.answer}`,
    tone: settled ? "live" : attention ? "paused" : "busy",
    question: "",
    acts: [
      // Back keeps the registration and returns to Machines. Only completed
      // checks earn Done; recovery is the primary action while blocked.
      { id: "back", label: "Back", tone: "quiet", text: true },
      ...forward,
    ],
  };
}
