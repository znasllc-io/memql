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

/**
 * Where the installer puts the tools every capability script shells out to.
 *
 * `install.binary` drops k3d / kubectl / mkcert here, and no shell has ever
 * heard of it.
 */
export function installedToolDir(env: NodeJS.ProcessEnv = process.env): string {
  return path.join(env.HOME ?? "", ".memql", "bin");
}

/**
 * `base`, with the installer's tool directory at the front of PATH.
 *
 * WHY THIS IS THE SPAWNER'S JOB AND NOT THE CALLER'S (memql#3911). This
 * composition used to live only in the install session, which meant a
 * capability script found `kubectl` when the install graph ran it and not when
 * anything else did. `mintOwnershipLink` calls the runner directly, so taking
 * ownership -- the whole point of memql#3906 -- died on
 *
 *     An enrolment link could not be minted: kubectl is not installed;
 *     cannot exec the identity pod
 *
 * on a machine where the installer had put kubectl at `~/.memql/bin/kubectl`
 * twenty minutes earlier. Same script, same cluster, same binary; the only
 * difference was which caller assembled the environment.
 *
 * Needing these tools is a property of BEING a capability script, not of any
 * one call site -- they all shell out to kubectl, k3d or mkcert -- and the type
 * says `env?`, so nothing about the call site suggests an omission is fatal.
 * Putting it here makes the whole class impossible instead of fixing one
 * instance.
 *
 * IDEMPOTENT. The session passes an env that already names its own tool
 * directory; prepending a duplicate would be harmless but untidy, and a caller
 * that has deliberately chosen a directory should see it stay first.
 */
export function withInstalledTools(
  base: NodeJS.ProcessEnv,
  toolDir?: string,
): NodeJS.ProcessEnv {
  const dir = (toolDir ?? installedToolDir(base)).trim();
  if (dir === "") return base;
  const current = base.PATH ?? "";
  const entries = current.split(path.delimiter);
  if (entries.includes(dir)) return base;
  return { ...base, PATH: current === "" ? dir : `${dir}${path.delimiter}${current}` };
}

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
 * The exit codes MemQL assigns ITSELF, as opposed to the ones a capability
 * script reports (docs/internal/design/capability-script-contract.md).
 *
 * NAMED, AND EXPORTED, because they have to be explainable. A step killed by
 * the ten-minute ceiling reaching the operator as "MemQL cannot say what code
 * 124 means" is MemQL disclaiming a number it chose -- and that was live until
 * memql#3493. `installProgress.test.ts` derives its reachable set from this
 * object plus the contract's own table, so a code added here without guidance
 * is a red test rather than a shrug in front of an operator.
 */
export const SYNTHESISED_EXIT_CODES = {
  /** The step outran `timeoutMs` and was SIGKILLed. */
  timeout: 124,
  /** The script could not be spawned at all -- missing, or not executable. */
  spawnFailure: 127,
  /** The child died on a signal nobody here sent, and reported no code. */
  signalled: 128,
} as const;

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
        // The installer's tools go on PATH here rather than at the call site,
        // so a caller that passes no env still finds kubectl (memql#3911).
        env: withInstalledTools(run.env ?? process.env),
        stdio: ["ignore", "pipe", "pipe"],
        shell: false,
      });
    } catch (err) {
      finish(
        SYNTHESISED_EXIT_CODES.spawnFailure,
        null,
        `could not start ${run.scriptPath}: ${(err as Error).message}`,
      );
      return;
    }

    const timer =
      run.timeoutMs && run.timeoutMs > 0
        ? setTimeout(() => {
            child.kill("SIGKILL");
            finish(
              SYNTHESISED_EXIT_CODES.timeout,
              "SIGKILL",
              `${run.scriptPath} exceeded ${run.timeoutMs}ms`,
            );
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
      finish(
        SYNTHESISED_EXIT_CODES.spawnFailure,
        null,
        `could not run ${run.scriptPath}: ${err.message}`,
      );
    });
    child.on("close", (code, signal) => {
      if (run.onLog && logCarry.trim() !== "") run.onLog(logCarry);
      finish(code ?? (signal ? SYNTHESISED_EXIT_CODES.signalled : 1), signal);
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

  // Azure substrate lifecycle (epic memql#4463). The deploy backends above
  // place WORKLOADS onto a cluster; these three act on the cluster ITSELF --
  // provision it, resize it, delete it.
  "deploy.azureProvision": "scripts/deploy/azure-provision.sh",
  "deploy.azureScale": "scripts/deploy/azure-scale.sh",
  "deploy.azureTeardown": "scripts/deploy/azure-teardown.sh",

  // The INSTALL phase (epic memql#4490) -- the eleven ordered steps between a
  // provisioned substrate and an argoSync that means something. Six of them are
  // dependencies that exist on no manifest and every one fails SILENTLY, which
  // is why they are scripts: a missing Secret named by a volume leaves a pod in
  // ContainerCreating forever with no log line.
  "deploy.installClusterOperators": "scripts/deploy/install-cluster-operators.sh",
  "deploy.seedInstanceSecrets": "scripts/deploy/seed-instance-secrets.sh",
  "deploy.wireExternalSecrets": "scripts/deploy/wire-external-secrets.sh",
  "deploy.registerGitOpsRepo": "scripts/deploy/register-gitops-repo.sh",
  "deploy.verifyInstallDependencies": "scripts/deploy/verify-install-dependencies.sh",
  "deploy.settleAfterSync": "scripts/deploy/settle-after-sync.sh",
  "deploy.renderDiff": "scripts/deploy/render-diff.sh",

  // Release + version provenance (epic memql#4493). reportInstanceVersion
  // reports the declared, rendered and running engine refs together;
  // release.engine publishes a GitHub release and verifies the image build it
  // triggers actually started -- a pushed git tag builds nothing.
  "deploy.reportInstanceVersion": "scripts/deploy/report-instance-version.sh",
  "release.engine": "scripts/release/release-engine.sh",

  // Tenant lifecycle (epic memql#3852, task memql#3853).
  "fleet.tenantProvision": "scripts/fleet/tenant-provision.sh",
  "fleet.tenantSuspend": "scripts/fleet/tenant-suspend.sh",
  "fleet.tenantResume": "scripts/fleet/tenant-resume.sh",
  "fleet.tenantTeardown": "scripts/fleet/tenant-teardown.sh",

  // Local-cluster install/uninstall substrate (epic #3357).
  "install.refreshPins": "scripts/install/refresh-tool-pins.sh",
  "install.detect": "scripts/install/detect.sh",
  "install.dockerAccess": "scripts/install/docker-access.sh",
  "install.nssTools": "scripts/install/nss-tools.sh",
  "install.binary": "scripts/install/install-binary.sh",
  "install.hostsEntries": "scripts/install/hosts-entries.sh",
  "install.mkcert": "scripts/install/mkcert-setup.sh",
  "install.cloneStack": "scripts/install/clone-stack.sh",
  "install.updateStack": "scripts/install/update-stack.sh",
  "install.seedBootstrap": "scripts/install/seed-bootstrap.sh",
  "install.verifyProviderKey": "scripts/install/verify-provider-key.sh",
  "install.verifyFrontDoor": "scripts/install/verify-frontdoor.sh",
  "install.magicLink": "scripts/install/magic-link.sh",
  "install.enrolmentLink": "scripts/install/enrolment-link.sh",
  "install.recoveryKey": "scripts/install/recovery-key.sh",
  "install.removeArtifact": "scripts/install/remove-artifact.sh",
  // CI's instrument for the install/uninstall round trip, not a graph step.
  "install.e2eBaseline": "scripts/install/e2e-baseline.sh",
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
