// The wizard's platform gate (memql#4294).
//
// detect.sh is the authority: linux/amd64 only, exit 3 on anything else. The
// install graph already starts there. Uninstall and Create-deployment-without-
// a-cluster did not, so a Darwin machine could list tags or reach sudo/hosts
// before anything named the refuse. This module is the one probe those verbs
// share: run install.detect, and if it is the unsupported-platform refusal,
// return that result and stop. Missing docker/tools/ports are data (exit 0)
// and are not a refuse.
//
// Free of `vscode` imports.
//
// Refs: #4294 #3359

import type { ExecEvent, ExecutionReport, StepOutcome } from "./executor.js";
import type { Step } from "./graph.js";
import {
  capabilityScriptPath,
  runCapabilityScript,
  withInstalledTools,
  type RunScript,
  type ScriptOutcome,
} from "./runner.js";

/** The slice of a session this gate needs. Kept local to avoid a cycle with session.ts. */
export interface PlatformProbeOptions {
  root: string;
  toolDir?: string;
  env?: Record<string, string>;
  timeoutMs?: number;
}

export interface PlatformProbeHooks {
  run?: RunScript;
  onEvent?: (event: ExecEvent) => void | Promise<void>;
}

/** detect.sh's only refusal. */
export const UNSUPPORTED_PLATFORM_EXIT = 3;

const DETECT_MESSAGE_RE = /unsupported platform/i;

/** The detect step, as the install graph names it -- used when we refuse outside that graph. */
export const PLATFORM_DETECT_STEP: Step = {
  id: "detect",
  script: "install.detect",
  description: "Inventory the machine: OS/arch support, docker daemon, port availability, free disk.",
  readOnly: true,
  elevation: "none",
  retained: false,
  retainedReason: "",
  shared: false,
  sharedReason: "",
  verify: { kind: "resultTrue", field: "result.supported" },
};

export function isUnsupportedPlatformMessage(text: string): boolean {
  return DETECT_MESSAGE_RE.test(text);
}

export function isUnsupportedPlatformOutcome(outcome: ScriptOutcome): boolean {
  const message = outcome.envelope?.error?.message ?? outcome.stderr ?? "";
  return outcome.exitCode === UNSUPPORTED_PLATFORM_EXIT && isUnsupportedPlatformMessage(message);
}

function detectMessage(outcome: ScriptOutcome): string {
  return (
    outcome.envelope?.error?.message ??
    outcome.stderr.trim() ??
    "unsupported platform: the local cluster installer targets linux/amd64 only"
  );
}

/**
 * Runs install.detect through the same runner the graphs use.
 *
 * Returns a failed ExecutionReport when detect refuses the platform. Returns
 * undefined on every other answer -- including a successful inventory of a
 * bare machine -- so the caller proceeds to its own next gate.
 */
export async function refuseUnsupportedPlatform(
  opts: PlatformProbeOptions,
  hooks: PlatformProbeHooks = {},
): Promise<ExecutionReport | undefined> {
  const run: RunScript = hooks.run ?? runCapabilityScript;
  const outcome = await run({
    scriptPath: capabilityScriptPath("install.detect", opts.root),
    params: {},
    capability: "install.detect",
    cwd: opts.root,
    env: withInstalledTools({ ...process.env, ...(opts.env ?? {}) }, opts.toolDir),
    timeoutMs: opts.timeoutMs,
  });
  if (!isUnsupportedPlatformOutcome(outcome)) return undefined;
  const report = platformRefuseReport(outcome);
  await emitPlatformRefuse(report, hooks.onEvent);
  return report;
}

export function platformRefuseReport(outcome: ScriptOutcome): ExecutionReport {
  const now = new Date().toISOString();
  const reason = detectMessage(outcome);
  const detect: StepOutcome = {
    id: PLATFORM_DETECT_STEP.id,
    script: PLATFORM_DETECT_STEP.script,
    status: "failed",
    exitCode: UNSUPPORTED_PLATFORM_EXIT,
    envelope: outcome.envelope,
    verified: false,
    preExisting: false,
    params: {},
    reason,
    startedAt: now,
    finishedAt: now,
  };
  return {
    graph: "platform",
    ok: false,
    waves: [[PLATFORM_DETECT_STEP.id]],
    outcomes: [detect],
  };
}

export function platformRefuseEvents(report: ExecutionReport): ExecEvent[] {
  const outcome = report.outcomes[0];
  if (outcome === undefined) return [];
  return [
    { type: "runStarted", steps: [{ id: PLATFORM_DETECT_STEP.id, description: PLATFORM_DETECT_STEP.description }] },
    { type: "stepStarted", step: PLATFORM_DETECT_STEP, params: {} },
    { type: "stepFinished", step: PLATFORM_DETECT_STEP, outcome },
  ];
}

async function emitPlatformRefuse(
  report: ExecutionReport,
  onEvent?: PlatformProbeHooks["onEvent"],
): Promise<void> {
  if (onEvent === undefined) return;
  for (const event of platformRefuseEvents(report)) {
    await onEvent(event);
  }
}
