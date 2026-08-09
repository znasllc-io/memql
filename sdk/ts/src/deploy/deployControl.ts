// The typed deploy-control surface -- the TypeScript mirror of
// sdk/go/client/deploycontrol.go (memql#3311).
//
// The Go SDK dials DeployControlService natively, because it can: the
// service is a separate UNARY gRPC service mounted on the same listener.
// A browser cannot dial gRPC at all, and neither can any other WebSocket
// client, so this surface rides the bridged DeployControlMsg /
// DeployControlResult pair on MemqlService.Stream instead. The method set
// and the plain result shapes match the Go client one-for-one so a
// consumer moving between them is not learning two APIs.
//
// AUTHORIZATION IS SERVER-SIDE, ALWAYS. Nothing in this file checks a
// role. The engine enforces the locked matrix (epic #1871) on the streamed
// path through the SAME gate the unary path uses -- view is owner/admin,
// cut + deploy are developer/admin/owner, rollback is owner ONLY, not even
// admin -- and every action writes a v1:identity:auditEvent whose id comes
// back on the result. A caller may hide buttons a user cannot use, but
// that is presentation; the refusal is authoritative and arrives as a
// DeployControlError with code PERMISSION_DENIED.
//
// A REFUSAL is audited too, and carries its own id: DeployControlError
// .auditEventId (memql#3334). Surface it -- the operator who was told no is
// the one most likely to need a reference to quote.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import { readServerPayload } from "../client/wire.js";
import type {
  DeployControlRequestPayload,
  DeployControlResultPayload,
  DeploymentStatusWire,
  SuggestNextVersionResultWire,
  DeployActionResultWire,
} from "../client/wire.js";

// -----------------------------------------------------------------------------
// Plain result types (no wire shapes leak to consumers)
// -----------------------------------------------------------------------------

/** One deployed component's pinned image. */
export interface ComponentDigest {
  name: string;
  digest: string;
  repo: string;
}

/** The Argo CD application status. */
export interface ArgoStatus {
  syncStatus: string;
  healthStatus: string;
  lastSyncRevision: string;
  lastSyncAt: string;
  outOfSync: boolean;
}

/** One Argo Rollout's progressive-delivery state. */
export interface RolloutStatus {
  name: string;
  kind: string; // "bluegreen" | "canary"
  phase: string;
  activeColor: string;
  previewColor: string;
  canaryWeight: number;
  currentStep: number;
  latestAnalysisResult: string;
}

/** One leg of the deploy gate. */
export interface GateLeg {
  name: string;
  passed: boolean;
  detail: string;
}

/** The latest deploy-gate outcome. */
export interface GateResult {
  result: string; // "pass" | "fail" | "unknown"
  legs: GateLeg[];
  ranAt: string;
}

/** The aggregate per-env deployment view. */
export interface DeploymentStatus {
  env: string;
  version: string;
  engineVersion: string;
  validatedAt: string;
  gate: string;
  components: ComponentDigest[];
  argocd: ArgoStatus;
  rollouts: RolloutStatus[];
  gateResult: GateResult;
}

/**
 * The uniform return for every write action.
 *
 * `ok` is the ACTION's own success, distinct from "the call was
 * permitted" -- a refused call throws instead of returning ok=false.
 * `auditEventId` is the v1:identity:auditEvent this action wrote; show it
 * to the operator and quote it in support threads.
 */
export interface ActionResult {
  ok: boolean;
  message: string;
  auditEventId: string;
  correlationId: string;
  details: Record<string, string>;
}

/** The current version plus the three next-version proposals. */
export interface NextVersionSuggestion {
  currentVersion: string;
  nextMajor: string;
  nextMinor: string;
  nextPatch: string;
  source: string; // "deployment" | "overlay" | "none"
}

// -----------------------------------------------------------------------------
// Errors
// -----------------------------------------------------------------------------

/**
 * Canonical gRPC codes the deploy surface returns, by number. The bridge
 * carries the code as an int rather than a status error because a
 * multiplexed stream has no per-message status channel -- see
 * DeployControlResult in component/grpc/memql.proto.
 *
 * Only the codes this surface actually produces are named; anything else
 * surfaces as its number.
 */
const GRPC_CODE_NAMES: Record<number, string> = {
  0: "OK",
  3: "INVALID_ARGUMENT",
  7: "PERMISSION_DENIED",
  12: "UNIMPLEMENTED",
  13: "INTERNAL",
  16: "UNAUTHENTICATED",
};

/** PERMISSION_DENIED -- the caller's role is below the action's tier. */
export const CODE_PERMISSION_DENIED = 7;
/** UNAUTHENTICATED -- the stream carries no resolvable actor. */
export const CODE_UNAUTHENTICATED = 16;
/** UNIMPLEMENTED -- this node does not host the deploy-control service. */
export const CODE_UNIMPLEMENTED = 12;

/**
 * A refused or failed deploy-control call.
 *
 * `code` is the canonical gRPC code the engine returned, so a UI can tell
 * "you are not allowed to do this" (CODE_PERMISSION_DENIED) apart from
 * "this node cannot do this" (CODE_UNIMPLEMENTED) and from a genuine
 * failure, rather than string-matching a message.
 */
export class DeployControlError extends Error {
  readonly code: number;
  readonly codeName: string;
  /** The verb that was refused, as its own field for the same reason engineMessage is. */
  readonly verb: string;
  /**
   * The engine's own sentence, RAW -- no prefix, no code, no sentinel.
   *
   * Same split as AutomationRunError.engineMessage, and for the same reason
   * (memql#3339): `message` is shaped for a log line
   * (`deploy console: <verb>: <CODE>: <msg>`), which a UI already rendering the
   * verb and the code duplicates when it pastes it in. The deploy console had
   * the problem too and no helper to compensate with, so both consumers were
   * printing the code twice.
   *
   * Empty string when the engine sent no message.
   */
  readonly engineMessage: string;

  /**
   * The audit event this REFUSAL wrote (memql#3334).
   *
   * A denial on this surface is an audited event -- the role gate writes a
   * blocked `v1:identity:auditEvent` before it refuses -- and this is its
   * correlation id. Show it beside the refusal: an operator told "you cannot
   * roll back" needs a reference to quote, and until #3334 the one event they
   * most wanted to cite was the only one with no id.
   *
   * Empty for anything that is not a gate refusal. An INVALID_ARGUMENT runs
   * before the gate and writes no event; an UNAVAILABLE never reached the
   * service. Empty means "nothing was audited", not "the id went missing".
   *
   * Mirrors `IdentityAdminError.auditEventId`, which made the same call for
   * the same reason (memql#3324).
   */
  readonly auditEventId: string;

  constructor(verb: string, code: number, message: string, auditEventId = "") {
    const name = GRPC_CODE_NAMES[code] ?? String(code);
    super(`deploy console: ${verb}: ${name}: ${message || "(no message)"}`);
    this.name = "DeployControlError";
    this.code = code;
    this.codeName = name;
    this.verb = verb;
    this.engineMessage = message;
    this.auditEventId = auditEventId;
  }

  /** True when the refusal was the role gate, not a failure. */
  get isPermissionDenied(): boolean {
    return this.code === CODE_PERMISSION_DENIED;
  }
}

// -----------------------------------------------------------------------------
// Client
// -----------------------------------------------------------------------------

export interface DeployControlCallOptions {
  signal?: AbortSignal;
}

/**
 * A thin typed client over the bridged DeployControlService.
 *
 * Construct it from a Dispatcher (the same one Connection exposes) and
 * call the method you want; each one sends a single DeployControlMsg and
 * awaits its correlated DeployControlResult.
 *
 * NOT here, deliberately: deployment history. `v1:cluster:deployment` rows
 * are ordinary concept rows -- read them through the normal query surface
 * rather than a second, divergent path.
 */
export class DeployControlClient {
  constructor(private readonly dispatcher: Dispatcher) {
    if (!dispatcher) throw new Error("DeployControlClient: dispatcher is required");
  }

  /** The full deployment picture for env ("staging" | "prod"). */
  async getDeploymentStatus(
    env: string,
    opts: DeployControlCallOptions = {},
  ): Promise<DeploymentStatus> {
    requireArg("getDeploymentStatus", "env", env);
    const res = await this.call("get_status", { getDeploymentStatus: { env } }, opts);
    return deploymentStatusFromWire(res.deploymentStatus);
  }

  /**
   * The latest succeeded deployment's version for env plus the next
   * major / minor / patch proposals. Read-only, but developer-or-above
   * gated server-side: it is the read companion to cutVersion.
   */
  async suggestNextVersion(
    env: string,
    opts: DeployControlCallOptions = {},
  ): Promise<NextVersionSuggestion> {
    requireArg("suggestNextVersion", "env", env);
    const res = await this.call("suggest_version", { suggestNextVersion: { env } }, opts);
    return nextVersionFromWire(res.nextVersion);
  }

  /** Deploy a release version into staging. */
  async deployStaging(version: string, opts: DeployControlCallOptions = {}): Promise<ActionResult> {
    requireArg("deployStaging", "version", version);
    return actionFromWire(
      await this.call("deploy_staging", { deployStaging: { version } }, opts),
    );
  }

  /** Copy a validated staging release into prod (digest copy, no rebuild). */
  async promote(version: string, opts: DeployControlCallOptions = {}): Promise<ActionResult> {
    requireArg("promote", "version", version);
    return actionFromWire(await this.call("promote", { promote: { version } }, opts));
  }

  /** Revert the overlay commit identified by commitSha for env. */
  async rollback(
    env: string,
    commitSha: string,
    opts: DeployControlCallOptions = {},
  ): Promise<ActionResult> {
    requireArg("rollback", "env", env);
    requireArg("rollback", "commitSha", commitSha);
    return actionFromWire(await this.call("rollback", { rollback: { env, commitSha } }, opts));
  }

  /** Promote or abort an in-flight Argo Rollout (action: "promote" | "abort"). */
  async rolloutAction(
    env: string,
    rollout: string,
    action: string,
    opts: DeployControlCallOptions = {},
  ): Promise<ActionResult> {
    requireArg("rolloutAction", "env", env);
    requireArg("rolloutAction", "rollout", rollout);
    requireArg("rolloutAction", "action", action);
    return actionFromWire(
      await this.call("rollout_action", { rolloutAction: { env, rollout, action } }, opts),
    );
  }

  /**
   * Create a new pending deployment record at the chosen next version,
   * ready for deploy() to ship.
   *
   * `bump` selects the semver part to increment off the current version
   * ("major" | "minor" | "patch"; empty defaults to patch). `version` is
   * an explicit override -- when set, bump is ignored.
   */
  async cutVersion(
    env: string,
    bump = "",
    version = "",
    opts: DeployControlCallOptions = {},
  ): Promise<ActionResult> {
    requireArg("cutVersion", "env", env);
    return actionFromWire(
      await this.call(
        "cut_version",
        {
          cutVersion: {
            env,
            ...(bump ? { bump } : {}),
            ...(version ? { version } : {}),
          },
        },
        opts,
      ),
    );
  }

  /**
   * Ship a cut (pending) deployment record to its target.
   *
   * Async by design: a successful result means "accepted + kicked off",
   * NOT "deployed". Poll getDeploymentStatus or the v1:cluster:deployment
   * row for the terminal status.
   */
  async deploy(deploymentId: string, opts: DeployControlCallOptions = {}): Promise<ActionResult> {
    requireArg("deploy", "deploymentId", deploymentId);
    return actionFromWire(await this.call("deploy", { deploy: { deploymentId } }, opts));
  }

  /**
   * Redeploy a prior SUCCEEDED deployment's stored image digest -- NOT a
   * blue-green colour flip. A new deployment record is created pointing at
   * the historical digest. OWNER-ONLY server-side: an admin caller gets
   * PERMISSION_DENIED here, by design.
   *
   * Async, like deploy(): the result acknowledges the kick-off.
   */
  async rollbackDeployment(
    toDeploymentId: string,
    opts: DeployControlCallOptions = {},
  ): Promise<ActionResult> {
    requireArg("rollbackDeployment", "toDeploymentId", toDeploymentId);
    return actionFromWire(
      await this.call("rollback_deployment", { rollbackDeployment: { toDeploymentId } }, opts),
    );
  }

  // call sends one bridged request and unwraps its reply, turning a
  // carried gRPC code into a thrown DeployControlError. `verb` is only
  // used to label the error.
  private async call(
    verb: string,
    request: DeployControlRequestPayload,
    opts: DeployControlCallOptions,
  ): Promise<DeployControlResultPayload> {
    const requestId = newShortId();
    const reply = await this.dispatcher.sendAndWait(
      { deployControl: { requestId, ...request } },
      opts.signal,
    );
    const payload = readServerPayload(reply);
    if (payload?.kind === "queryError") {
      throw new Error(`deploy console: ${verb}: ${payload.value.error?.message ?? "(no message)"}`);
    }
    if (payload?.kind !== "deployControlResult") {
      throw new Error(`deploy console: ${verb}: unexpected reply envelope`);
    }
    const res = payload.value;
    // protojson omits zero values, so an absent `ok` is false and an
    // absent errorCode is 0 (OK). Trusting `ok` alone would read a
    // malformed reply as success, so a non-zero code is authoritative.
    const code = res.errorCode ?? 0;
    if (code !== 0 || res.ok !== true) {
      throw new DeployControlError(verb, code, res.errorMessage ?? "", res.auditEventId ?? "");
    }
    return res;
  }
}

function requireArg(method: string, name: string, value: string): void {
  if (!value) throw new Error(`${method}: ${name} is required`);
}

// -----------------------------------------------------------------------------
// wire -> plain translation
// -----------------------------------------------------------------------------

function deploymentStatusFromWire(w: DeploymentStatusWire | undefined): DeploymentStatus {
  return {
    env: w?.env ?? "",
    version: w?.version ?? "",
    engineVersion: w?.engineVersion ?? "",
    validatedAt: w?.validatedAt ?? "",
    gate: w?.gate ?? "",
    components: (w?.components ?? []).map((c) => ({
      name: c.name ?? "",
      digest: c.digest ?? "",
      repo: c.repo ?? "",
    })),
    argocd: {
      syncStatus: w?.argocd?.syncStatus ?? "",
      healthStatus: w?.argocd?.healthStatus ?? "",
      lastSyncRevision: w?.argocd?.lastSyncRevision ?? "",
      lastSyncAt: w?.argocd?.lastSyncAt ?? "",
      outOfSync: w?.argocd?.outOfSync === true,
    },
    rollouts: (w?.rollouts ?? []).map((r) => ({
      name: r.name ?? "",
      kind: r.kind ?? "",
      phase: r.phase ?? "",
      activeColor: r.activeColor ?? "",
      previewColor: r.previewColor ?? "",
      canaryWeight: r.canaryWeight ?? 0,
      currentStep: r.currentStep ?? 0,
      latestAnalysisResult: r.latestAnalysisResult ?? "",
    })),
    gateResult: {
      result: w?.gateResult?.result ?? "",
      legs: (w?.gateResult?.legs ?? []).map((l) => ({
        name: l.name ?? "",
        passed: l.passed === true,
        detail: l.detail ?? "",
      })),
      ranAt: w?.gateResult?.ranAt ?? "",
    },
  };
}

function nextVersionFromWire(w: SuggestNextVersionResultWire | undefined): NextVersionSuggestion {
  return {
    currentVersion: w?.currentVersion ?? "",
    nextMajor: w?.nextMajor ?? "",
    nextMinor: w?.nextMinor ?? "",
    nextPatch: w?.nextPatch ?? "",
    source: w?.source ?? "",
  };
}

function actionFromWire(res: DeployControlResultPayload): ActionResult {
  const w: DeployActionResultWire = res.action ?? {};
  return {
    ok: w.ok === true,
    message: w.message ?? "",
    auditEventId: w.auditEventId ?? "",
    correlationId: w.correlationId ?? "",
    details: w.details ?? {},
  };
}
