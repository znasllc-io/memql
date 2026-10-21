// The wizard's platform gate (memql#4294).
//
// detect.sh is the authority on which platforms are supported (it composes the
// set from scripts/lib/platform.sh), exit 3 on anything else. The
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

/** One line of detect's requirements report (scripts/lib/requirements.sh). */
export interface RequirementsCheck {
  id: string;
  label: string;
  observed: string;
  minimum: string;
  recommended: string;
  status: "recommended" | "minimum" | "below" | "unknown";
}

/**
 * How this computer measures against the minimum requirements for running the
 * whole cluster (docs/public/operate/minimum-requirements.md), as detect.sh
 * reports it. The figures live in scripts/lib/requirements.sh and nowhere here:
 * this side only reads the verdict, so the wizard and `make up` cannot disagree
 * about one machine.
 */
export interface RequirementsReport {
  meetsMinimum: boolean;
  meetsRecommended: boolean;
  checks: RequirementsCheck[];
  /** One sentence per check below the minimum, written by the script. */
  shortfalls: string[];
  /** Where the requirements are explained. */
  docs: string;
}

/** What one detect run says about this computer. */
export interface ComputerProbe {
  /** Present when detect refused the platform; the run to show for it. */
  refused?: ExecutionReport;
  /** Absent when detect predates the report or it could not be read. */
  requirements?: RequirementsReport;
}

const CHECK_STATUSES = new Set(["recommended", "minimum", "below", "unknown"]);

/**
 * Reads the `requirements` object out of a detect result. Anything malformed is
 * ABSENT rather than guessed: an install the wizard cannot judge is not refused
 * here, and the install's own requirements step still has the last word.
 */
export function parseRequirements(result: unknown): RequirementsReport | undefined {
  if (typeof result !== "object" || result === null) return undefined;
  const raw = (result as { requirements?: unknown }).requirements;
  if (typeof raw !== "object" || raw === null) return undefined;
  const r = raw as Record<string, unknown>;
  if (typeof r.meetsMinimum !== "boolean" || typeof r.meetsRecommended !== "boolean") return undefined;
  const text = (v: unknown): string => (typeof v === "string" ? v : "");
  const checks: RequirementsCheck[] = [];
  for (const c of Array.isArray(r.checks) ? r.checks : []) {
    if (typeof c !== "object" || c === null) continue;
    const o = c as Record<string, unknown>;
    const status = text(o.status);
    if (!CHECK_STATUSES.has(status)) continue;
    checks.push({
      id: text(o.id),
      label: text(o.label),
      observed: text(o.observed),
      minimum: text(o.minimum),
      recommended: text(o.recommended),
      status: status as RequirementsCheck["status"],
    });
  }
  const shortfalls = (Array.isArray(r.shortfalls) ? r.shortfalls : []).filter((x): x is string => typeof x === "string");
  return { meetsMinimum: r.meetsMinimum, meetsRecommended: r.meetsRecommended, checks, shortfalls, docs: text(r.docs) };
}

const DETECT_MESSAGE_RE = /unsupported platform/i;

/**
 * The detect step, as the install document names it -- used when we refuse
 * outside a run.
 *
 * A SECOND COPY OF ONE SENTENCE (and its label), and the reason it is safe is
 * that a test pins it: `screenOrdering.test.ts` reads
 * `scripts/install/graph/install.json` and fails when this description and
 * that one differ. Without that, the content
 * sweep in memql#4456 would have rewritten the document and left this one
 * reading in the old voice, on the one path an operator reaches when the
 * install refuses before it starts.
 */
export const PLATFORM_DETECT_STEP: Step = {
  id: "detect",
  script: "install.detect",
  label: "Checking this computer",
  description:
    "Taking stock of this machine before anything is installed: what system it is, which supporting tools are already here, whether Docker is answering, whether the ports a cluster needs are free, and how much disk is left. Only a system MemQL cannot support stops the install here; the rest is reported for you to read.",
  readOnly: true,
  elevation: "none",
  retained: false,
  retainedReason: "",
  shared: false,
  sharedReason: "",
  verify: { kind: "resultTrue", field: "result.supported" },
};

/** The pop-up's heading when this computer is below the minimum requirements. */
export const REQUIREMENTS_SHORTFALL_TITLE = "This computer doesn't meet the minimum requirements for MemQL";

/**
 * The pop-up's body: what falls short, in the script's own sentences, then why
 * it matters and what is still possible.
 */
export function requirementsShortfallDetail(report: RequirementsReport): string {
  const lines = report.shortfalls.length > 0 ? report.shortfalls : ["Something this computer needs is below the minimum."];
  return [
    ...lines,
    "",
    "A local install runs the whole cluster and its AI models on this computer. You can still connect to a cluster that runs somewhere else.",
  ].join("\n");
}

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
    // Last resort only: detect names the supported set itself, so this string
    // is what an operator sees when detect produced no message at all.
    "unsupported platform: this machine is not one the local cluster installer supports"
  );
}

/**
 * Runs install.detect through the same runner the graphs use, and returns both
 * of its answers about this computer: the platform refusal, when there is one,
 * and the requirements report, when detect wrote one.
 */
export async function probeThisComputer(
  opts: PlatformProbeOptions,
  hooks: PlatformProbeHooks = {},
): Promise<ComputerProbe> {
  const run: RunScript = hooks.run ?? runCapabilityScript;
  const outcome = await run({
    scriptPath: capabilityScriptPath("install.detect", opts.root),
    params: {},
    capability: "install.detect",
    cwd: opts.root,
    env: withInstalledTools({ ...process.env, ...(opts.env ?? {}) }, opts.toolDir),
    timeoutMs: opts.timeoutMs,
  });
  if (isUnsupportedPlatformOutcome(outcome)) {
    const report = platformRefuseReport(outcome);
    await emitPlatformRefuse(report, hooks.onEvent);
    return { refused: report };
  }
  const requirements = parseRequirements(outcome.envelope?.result);
  return requirements === undefined ? {} : { requirements };
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
  return (await probeThisComputer(opts, hooks)).refused;
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
    {
      type: "runStarted",
      steps: [
        {
          id: PLATFORM_DETECT_STEP.id,
          label: PLATFORM_DETECT_STEP.label,
          description: PLATFORM_DETECT_STEP.description,
        },
      ],
    },
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
