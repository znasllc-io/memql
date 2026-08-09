// Running a capability script and reading what it says back.
//
// A capability script (scripts/lib/capability.sh) is a deterministic backend
// with a fixed contract: human logs on STDERR, exactly one JSON result envelope
// on STDOUT, honest exit codes (0 ok, 2 bad param, 3 refused, 4 prerequisite
// missing, 5 operation failed), no interactive prompts. This module is the
// TypeScript side of that contract, and it is deliberately thin -- everything
// it knows is how to invoke a script and how to read an envelope.
//
// TWO RULES IT DOES NOT BEND.
//
//   NO SHELL. Each param is its own argv element (`--name=value`), passed to
//   spawn without a shell anywhere in the path. That is what makes a param
//   value containing `;`, `$(...)` or backticks an INERT string: it is what
//   the script's cap_param returns, never something a shell re-parses. The Go
//   runner (component/automations/steps/capability_script.go) makes the same
//   promise; a `sh -c` here would quietly break it on the host path only.
//
//   THE ENVELOPE IS THE ANSWER, not the exit code. The exit code says how the
//   RUN went; result{} says what happened to the MACHINE. Callers verify
//   against the second (see graph.ts evaluateVerify), so a missing or
//   unparseable envelope is reported as such rather than smoothed over.
//
// Free of `vscode` imports -- cli.ts runs this under plain node.
//
// Refs: #3373 #3357

import { spawn } from "node:child_process";
import * as path from "node:path";

/** The `error` block of a failed envelope. */
export interface CapabilityError {
  code: number;
  message: string;
}

/** The one JSON document a capability script writes to stdout. */
export interface CapabilityEnvelope {
  ok: boolean;
  capability: string;
  changed: boolean;
  result: Record<string, unknown>;
  error: CapabilityError | null;
}

/** One invocation. */
export interface ScriptRun {
  /** Absolute path to the capability script. */
  scriptPath: string;
  /** Flags, as the script's own --print-spec spells them (kebab-case). */
  params: Record<string, string>;
  /** The capability id, carried for logging and for the receipt. */
  capability?: string;
  cwd?: string;
  env?: NodeJS.ProcessEnv;
  /** Kill the script after this long. 0 or absent means no timeout. */
  timeoutMs?: number;
  /** Called with each line the script writes to stderr, as it arrives. */
  onLog?: (line: string) => void;
}

export interface ScriptOutcome {
  argv: string[];
  exitCode: number;
  signal: NodeJS.Signals | null;
  stdout: string;
  stderr: string;
  /** null when the script wrote no readable envelope. */
  envelope: CapabilityEnvelope | null;
  /** Why the envelope could not be read, when it could not. */
  parseError?: string;
}

/** The injectable seam: executor tests substitute this. */
export type RunScript = (run: ScriptRun) => Promise<ScriptOutcome>;

/**
 * The params of a run as argv elements.
 *
 * One element per param, in the script's own kebab-case spelling. A conformant
 * capability script exits 2 on an undeclared flag, so a misspelling fails loudly
 * instead of silently falling back to a default.
 */
export function toArgv(params: Record<string, string>): string[] {
  return Object.entries(params).map(([name, value]) => `--${name}=${value}`);
}

/**
 * Reads the result envelope out of a script's stdout.
 *
 * The contract says exactly one envelope on stdout, but a script that a human
 * has been debugging may have left a stray line there, so the LAST line that
 * parses as a result envelope wins. "Parses as a result envelope" is checked
 * structurally (an object carrying `ok` and `capability`) so that a --print-spec
 * descriptor or an unrelated JSON blob is not mistaken for a result.
 */
export function parseEnvelope(stdout: string): { ok: true; envelope: CapabilityEnvelope } | { ok: false; error: string } {
  const lines = stdout.split("\n").map((l) => l.trim()).filter((l) => l !== "");
  for (let i = lines.length - 1; i >= 0; i -= 1) {
    const line = lines[i]!;
    if (!line.startsWith("{")) continue;
    let raw: unknown;
    try {
      raw = JSON.parse(line);
    } catch {
      continue;
    }
    if (raw === null || typeof raw !== "object" || Array.isArray(raw)) continue;
    const o = raw as Record<string, unknown>;
    if (typeof o.ok !== "boolean" || typeof o.capability !== "string") continue;
    return {
      ok: true,
      envelope: {
        ok: o.ok,
        capability: o.capability,
        changed: o.changed === true,
        result:
          o.result !== null && typeof o.result === "object" && !Array.isArray(o.result)
            ? (o.result as Record<string, unknown>)
            : {},
        error: readError(o.error),
      },
    };
  }
  return {
    ok: false,
    error:
      stdout.trim() === ""
        ? "the script wrote nothing to stdout -- a capability script always emits one result envelope"
        : "no result envelope on stdout (expected one JSON object carrying ok and capability)",
  };
}

function readError(v: unknown): CapabilityError | null {
  if (v === null || typeof v !== "object" || Array.isArray(v)) return null;
  const o = v as Record<string, unknown>;
  return {
    code: typeof o.code === "number" ? o.code : 1,
    message: typeof o.message === "string" ? o.message : "",
  };
}

/**
 * Spawns a capability script and reads its envelope.
 *
 * Never throws for a script-side problem: a missing script, a crash, a timeout
 * and a refusal all come back as an outcome, because the executor has to
 * record them on the receipt and carry on with the independent branches.
 */
export const runCapabilityScript: RunScript = async (run: ScriptRun): Promise<ScriptOutcome> => {
  const argv = toArgv(run.params);
  return new Promise<ScriptOutcome>((resolve) => {
    let stdout = "";
    let stderr = "";
    let logCarry = "";
    let settled = false;

    const finish = (exitCode: number, signal: NodeJS.Signals | null, failure?: string): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      const parsed = parseEnvelope(stdout);
      resolve({
        argv,
        exitCode,
        signal,
        stdout,
        stderr,
        envelope: parsed.ok ? parsed.envelope : null,
        parseError: parsed.ok ? undefined : failure ?? parsed.error,
      });
    };

    let child;
    try {
      // No shell, ever: argv elements go to execve untouched.
      child = spawn(run.scriptPath, argv, {
        cwd: run.cwd,
        env: run.env ?? process.env,
        stdio: ["ignore", "pipe", "pipe"],
        shell: false,
      });
    } catch (err) {
      finish(127, null, `could not start ${run.scriptPath}: ${(err as Error).message}`);
      return;
    }

    const timer =
      run.timeoutMs && run.timeoutMs > 0
        ? setTimeout(() => {
            child.kill("SIGKILL");
            finish(124, "SIGKILL", `${run.scriptPath} exceeded ${run.timeoutMs}ms`);
          }, run.timeoutMs)
        : (undefined as unknown as NodeJS.Timeout);

    child.stdout?.setEncoding("utf8");
    child.stdout?.on("data", (chunk: string) => {
      stdout += chunk;
    });
    child.stderr?.setEncoding("utf8");
    child.stderr?.on("data", (chunk: string) => {
      stderr += chunk;
      if (!run.onLog) return;
      logCarry += chunk;
      const lines = logCarry.split("\n");
      logCarry = lines.pop() ?? "";
      for (const line of lines) run.onLog(line);
    });

    child.on("error", (err) => {
      finish(127, null, `could not run ${run.scriptPath}: ${err.message}`);
    });
    child.on("close", (code, signal) => {
      if (run.onLog && logCarry.trim() !== "") run.onLog(logCarry);
      finish(code ?? (signal ? 128 : 1), signal);
    });
  });
};

// --------------------------------------------------------------------------
// resolving a capability id to a script
// --------------------------------------------------------------------------

/**
 * Capability id -> repo-relative script path.
 *
 * This is the host-side twin of capabilityScriptAllowlist in
 * component/automations/steps/capability_script.go, which is the SECURITY
 * boundary for the in-engine path. Two copies of one table drift, so
 * installExecutor.test.ts parses the Go map and asserts this one equals it --
 * adding a capability stays a deliberate two-file edit and forgetting the
 * second half is a red test rather than a capability that is silently
 * unreachable from the CLI.
 */
export const CAPABILITY_SCRIPTS: Record<string, string> = {
  // Local k3d cluster lifecycle.
  "k3d.up": "scripts/k3d/up.sh",
  "k3d.down": "scripts/k3d/down.sh",
  "k3d.dev": "scripts/k3d/dev.sh",
  "k3d.bringup": "scripts/k3d/bringup.sh",
  "k3d.scale": "scripts/k3d/scale.sh",
  "k3d.status": "scripts/k3d/status.sh",
  "k3d.seedSecrets": "scripts/k3d/seed-secrets.sh",
  "k3d.importImage": "scripts/k3d/import-image.sh",

  // Deploy-pack capability backends.
  "deploy.cloneRepo": "scripts/deploy/clone-repo.sh",
  "deploy.buildImage": "scripts/deploy/build-image.sh",
  "deploy.gate": "scripts/deploy/deploy-gate.sh",
  "deploy.notify": "scripts/deploy/notify.sh",
  "overlay.pinDigests": "scripts/deploy/pin-overlay-digests.sh",
  "overlay.revert": "scripts/deploy/revert-overlay.sh",
  "argocd.sync": "scripts/deploy/argo-sync.sh",

  // Local-cluster install/uninstall substrate (epic #3357).
  "install.refreshPins": "scripts/install/refresh-tool-pins.sh",
  "install.detect": "scripts/install/detect.sh",
  "install.binary": "scripts/install/install-binary.sh",
  "install.hostsEntries": "scripts/install/hosts-entries.sh",
  "install.mkcert": "scripts/install/mkcert-setup.sh",
  "install.cloneStack": "scripts/install/clone-stack.sh",
  "install.seedBootstrap": "scripts/install/seed-bootstrap.sh",
  "install.verifyProviderKey": "scripts/install/verify-provider-key.sh",
  "install.verifyFrontDoor": "scripts/install/verify-frontdoor.sh",
  "install.magicLink": "scripts/install/magic-link.sh",
  "install.removeArtifact": "scripts/install/remove-artifact.sh",
};

/** Resolves a capability id under a repository root. Unknown ids are refused. */
export function capabilityScriptPath(capability: string, repoRoot: string): string {
  const rel = CAPABILITY_SCRIPTS[capability];
  if (!rel) {
    throw new Error(
      `${capability} is not an allowlisted capability script -- ` +
        `a graph step can only run a script registered in CAPABILITY_SCRIPTS`,
    );
  }
  return path.join(repoRoot, ...rel.split("/"));
}
