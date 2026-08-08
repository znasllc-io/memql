// Deploy console client (memql#3311).
//
// DeployControlService is a separate UNARY gRPC service mounted on the same
// listener as MemqlService. sdk/go dials it natively (sdk/go/client/
// deploycontrol.go); a browser cannot, and neither can anything else speaking
// `/memql/ws`. The engine therefore bridges all nine RPCs onto
// MemqlService.Stream behind a single DeployControlMsg envelope, and this
// module is the typed surface over that bridge -- the TS mirror of the Go
// client, method for method.
//
// AUTHORIZATION IS SERVER-SIDE AND IDENTICAL ON BOTH TRANSPORTS. The bridge
// calls the same service methods the unary path calls, so the locked role
// matrix holds here without this file knowing anything about it:
//
//   view (deploymentStatus)                 admin / owner
//   cut + deploy (suggestNextVersion,
//     cutVersion, deploy)                   developer / admin / owner
//   roll back (rollbackDeployment)          OWNER ONLY -- not even admin
//
// A caller below the floor gets a queryError with code "PermissionDenied",
// which every function here raises as a DeployControlError carrying that code.
// Do NOT pre-filter actions on the client as a security measure; hide buttons
// for UX if you like, but the server is the gate.
//
// Every action writes a v1:identity:auditEvent (category `admin`, action
// `deployment_console_<verb>`) and returns its id in ActionResult.auditEventId.
//
// Deployment HISTORY is deliberately not here: v1:cluster:deployment rows are
// ordinary concept rows -- read them with a normal query rather than through a
// second, drifting path.

import type { Dispatcher } from "../client/dispatcher.js";
import { newShortId } from "../client/id.js";
import {
  readServerPayload,
  type DeployControlRequestWire,
  type DeployControlResultPayload,
} from "../client/wire.js";

// -----------------------------------------------------------------------------
// Result types (mirror sdk/go/client/deploycontrol.go)
// -----------------------------------------------------------------------------

export interface ComponentDigest {
  name: string;
  digest: string;
  repo: string;
}

export interface ArgoStatus {
  syncStatus: string;
  healthStatus: string;
  lastSyncRevision: string;
  lastSyncAt: string;
  outOfSync: boolean;
}

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

export interface GateLeg {
  name: string;
  passed: boolean;
  detail: string;
}

export interface GateResult {
  result: string; // "pass" | "fail" | "unknown"
  legs: GateLeg[];
  ranAt: string;
}

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

export interface ActionResult {
  ok: boolean;
  message: string;
  // Id of the v1:identity:auditEvent this action wrote. Present on the
  // streamed path exactly as on the unary one.
  auditEventId: string;
  correlationId: string;
  details: Record<string, string>;
}

export interface NextVersionSuggestion {
  currentVersion: string;
  nextMajor: string;
  nextMinor: string;
  nextPatch: string;
  source: string; // "deployment" | "overlay" | "none"
}

export type DeployEnv = "staging" | "prod";
export type SemverBump = "major" | "minor" | "patch";

// DeployControlError carries the gRPC status code the server returned, so a
// caller can distinguish "you are not allowed" (PermissionDenied) from "wrong
// node" (Unimplemented) or "bad input" (InvalidArgument) without string
// matching the message.
export class DeployControlError extends Error {
  readonly code: string;
  constructor(rpc: string, code: string, message: string) {
    super(`deploy console: ${rpc}: ${code}: ${message}`);
    this.name = "DeployControlError";
    this.code = code;
  }
}

// -----------------------------------------------------------------------------
// Transport
// -----------------------------------------------------------------------------

// call sends one DeployControlMsg and returns the correlated result payload.
// Every method below funnels through it so the error handling -- and the
// PermissionDenied surface in particular -- is written once.
async function call(
  dispatcher: Dispatcher,
  rpc: string,
  request: DeployControlRequestWire,
  signal?: AbortSignal,
): Promise<DeployControlResultPayload> {
  if (!dispatcher) throw new Error(`${rpc}: dispatcher is required`);
  const requestId = newShortId();
  const reply = await dispatcher.sendAndWait(
    { deployControl: { requestId, ...request } },
    signal,
  );
  const payload = readServerPayload(reply);
  if (payload?.kind === "queryError") {
    throw new DeployControlError(
      rpc,
      payload.value.error?.code ?? "Unknown",
      payload.value.error?.message ?? "(no message)",
    );
  }
  if (payload?.kind !== "deployControlResult") {
    throw new Error(`deploy console: ${rpc}: unexpected reply envelope`);
  }
  return payload.value;
}

// -----------------------------------------------------------------------------
// Reads
// -----------------------------------------------------------------------------

// getDeploymentStatus returns the full deployment picture for one environment:
// deployed digests, the resolved release lockfile, Argo CD sync/health,
// per-rollout progressive-delivery state, and the latest deploy-gate result.
// Admin/owner gated server-side.
export async function getDeploymentStatus(
  dispatcher: Dispatcher,
  env: DeployEnv,
  signal?: AbortSignal,
): Promise<DeploymentStatus> {
  const res = await call(dispatcher, "getDeploymentStatus", {
    getDeploymentStatus: { env },
  }, signal);
  const s = res.deploymentStatus ?? {};
  return {
    env: s.env ?? "",
    version: s.version ?? "",
    engineVersion: s.engineVersion ?? "",
    validatedAt: s.validatedAt ?? "",
    gate: s.gate ?? "",
    components: (s.components ?? []).map((c) => ({
      name: c.name ?? "",
      digest: c.digest ?? "",
      repo: c.repo ?? "",
    })),
    argocd: {
      syncStatus: s.argocd?.syncStatus ?? "",
      healthStatus: s.argocd?.healthStatus ?? "",
      lastSyncRevision: s.argocd?.lastSyncRevision ?? "",
      lastSyncAt: s.argocd?.lastSyncAt ?? "",
      outOfSync: s.argocd?.outOfSync === true,
    },
    rollouts: (s.rollouts ?? []).map((r) => ({
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
      result: s.gateResult?.result ?? "",
      legs: (s.gateResult?.legs ?? []).map((l) => ({
        name: l.name ?? "",
        passed: l.passed === true,
        detail: l.detail ?? "",
      })),
      ranAt: s.gateResult?.ranAt ?? "",
    },
  };
}

// suggestNextVersion reads the env's current version and proposes the next
// major / minor / patch semver. Developer/admin/owner gated server-side.
export async function suggestNextVersion(
  dispatcher: Dispatcher,
  env: DeployEnv,
  signal?: AbortSignal,
): Promise<NextVersionSuggestion> {
  const res = await call(dispatcher, "suggestNextVersion", {
    suggestNextVersion: { env },
  }, signal);
  const n = res.nextVersion ?? {};
  return {
    currentVersion: n.currentVersion ?? "",
    nextMajor: n.nextMajor ?? "",
    nextMinor: n.nextMinor ?? "",
    nextPatch: n.nextPatch ?? "",
    source: n.source ?? "",
  };
}

// -----------------------------------------------------------------------------
// Actions
// -----------------------------------------------------------------------------

function actionResult(res: DeployControlResultPayload): ActionResult {
  const a = res.action ?? {};
  return {
    ok: a.ok === true,
    message: a.message ?? "",
    auditEventId: a.auditEventId ?? "",
    correlationId: a.correlationId ?? "",
    details: a.details ?? {},
  };
}

// deployStaging assembles + applies a release into the staging overlay.
export async function deployStaging(
  dispatcher: Dispatcher,
  version: string,
  signal?: AbortSignal,
): Promise<ActionResult> {
  return actionResult(
    await call(dispatcher, "deployStaging", { deployStaging: { version } }, signal),
  );
}

// promote copies a validated staging release into the prod overlay (digest
// copy, no rebuild).
export async function promote(
  dispatcher: Dispatcher,
  version: string,
  signal?: AbortSignal,
): Promise<ActionResult> {
  return actionResult(await call(dispatcher, "promote", { promote: { version } }, signal));
}

// rollback reverts the overlay commit identified by commitSha for env.
export async function rollback(
  dispatcher: Dispatcher,
  env: DeployEnv,
  commitSha: string,
  signal?: AbortSignal,
): Promise<ActionResult> {
  return actionResult(
    await call(dispatcher, "rollback", { rollback: { env, commitSha } }, signal),
  );
}

// rolloutAction promotes or aborts an in-flight Argo Rollout.
export async function rolloutAction(
  dispatcher: Dispatcher,
  env: DeployEnv,
  rollout: string,
  action: "promote" | "abort",
  signal?: AbortSignal,
): Promise<ActionResult> {
  return actionResult(
    await call(dispatcher, "rolloutAction", { rolloutAction: { env, rollout, action } }, signal),
  );
}

export interface CutVersionArgs {
  env: DeployEnv;
  // bump selects the semver part to increment off the current version.
  // Ignored when version is set; defaults to "patch" server-side.
  bump?: SemverBump;
  // version is an explicit clean-semver override. When set, bump is ignored.
  version?: string;
  signal?: AbortSignal;
}

// cutVersion creates a new pending v1:cluster:deployment record at the chosen
// next version with the resolved image digest -- ready for deploy() to ship.
// Developer/admin/owner gated server-side.
export async function cutVersion(
  dispatcher: Dispatcher,
  args: CutVersionArgs,
): Promise<ActionResult> {
  return actionResult(
    await call(dispatcher, "cutVersion", {
      cutVersion: {
        env: args.env,
        ...(args.bump ? { bump: args.bump } : {}),
        ...(args.version ? { version: args.version } : {}),
      },
    }, args.signal),
  );
}

// deploy ships a cut (pending) deployment record to its target, selecting the
// driver by the record's stored provider. Developer/admin/owner gated
// server-side. The RPC returns an async ack; poll getDeploymentStatus or the
// v1:cluster:deployment row for resolution.
export async function deploy(
  dispatcher: Dispatcher,
  deploymentId: string,
  signal?: AbortSignal,
): Promise<ActionResult> {
  return actionResult(await call(dispatcher, "deploy", { deploy: { deploymentId } }, signal));
}

// rollbackDeployment redeploys a prior SUCCEEDED deployment's stored image
// digest through the same driver -- NOT a blue-green color flip. A new
// deployment record is created pointing at the historical digest. OWNER ONLY
// server-side: an admin caller gets PermissionDenied, by design.
export async function rollbackDeployment(
  dispatcher: Dispatcher,
  toDeploymentId: string,
  signal?: AbortSignal,
): Promise<ActionResult> {
  return actionResult(
    await call(dispatcher, "rollbackDeployment", { rollbackDeployment: { toDeploymentId } }, signal),
  );
}
