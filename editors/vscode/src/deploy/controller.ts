// Running a deploy-control action and turning its result into one operator
// line.
//
// The whole point of this module is that "what happened" is decided in ONE
// place, off the SDK's typed result, rather than by whichever call site
// happened to run the action. There are four distinguishable outcomes and they
// are routinely conflated:
//
//   1. REFUSED -- the role gate said no. Arrives as a thrown DeployControlError
//      with code PERMISSION_DENIED. It must name the role required; a bare
//      "permission denied", or worse a silent no-op, leaves the operator with
//      no idea whether to escalate or to fix their request. It carries an
//      audit id too (memql#3334): a refusal is an audited event, and the
//      operator arguing about a denial is the one who most needs a reference.
//   2. FAILED -- the action ran and did not work. Comes back as a NORMAL
//      resolved ActionResult with ok=false (the envelope's own `ok` means "the
//      RPC returned", not "the action worked" -- see DeployControlResult in
//      memql.proto). It carries an audit event id, and that id is the thing to
//      quote in a support thread.
//   3. SUCCEEDED -- ok=true, with an audit id.
//   4. UNREACHABLE -- this node does not host the deploy-control service
//      (UNIMPLEMENTED), or the stream carries no actor (UNAUTHENTICATED).
//
// Treating 2 as 3 is the failure mode worth engineering against: a resolved
// promise reads as success everywhere else in the extension, so an action that
// ran and failed would print SUCCESS with an audit id pointing at the record
// of its own failure.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import type { DeployControlClient } from "@znasllc-io/memql-sdk-core/deploy";
import {
  CODE_UNAUTHENTICATED,
  CODE_UNIMPLEMENTED,
  DeployControlError,
} from "@znasllc-io/memql-sdk-core/deploy";

import { actionById, tierDescription, type DeployActionId } from "./actions.js";

/**
 * The subset of the SDK client this surface drives.
 *
 * A `Pick` of the real class rather than a hand-written interface: it is the
 * SDK's signatures by construction, so a method whose shape changes upstream
 * fails to compile here instead of drifting. A test fake stays trivial anyway
 * -- a function with FEWER parameters is assignable to one with more, so a
 * fake need not restate the trailing options argument.
 */
export type DeployControlPort = Pick<
  DeployControlClient,
  | "getDeploymentStatus"
  | "suggestNextVersion"
  | "cutVersion"
  | "deploy"
  | "promote"
  | "rollbackDeployment"
  | "rolloutAction"
>;

/** The parameters an action needs, by id. */
export type DeployActionRequest =
  | { id: "cutVersion"; env: string; bump: string; version: string }
  | { id: "deploy"; deploymentId: string }
  | { id: "promote"; version: string }
  | { id: "rollback"; toDeploymentId: string }
  | { id: "rolloutAction"; env: string; rollout: string; subAction: string };

export interface DeployOutcome {
  kind: "success" | "error";
  /** The single line to show. Always starts SUCCESS: or ERROR:. */
  line: string;
  /**
   * The v1:identity:auditEvent this action wrote -- on a REFUSAL too
   * (memql#3334), because the gate audits a denial before returning it.
   *
   * Empty means no event exists, not that one was dropped: an
   * INVALID_ARGUMENT is rejected before the gate runs, and an UNIMPLEMENTED
   * never reached a service to be gated by. Callers render nothing there
   * rather than a placeholder.
   */
  auditEventId: string;
  /** True only for a refusal by the role gate, so a caller can style or log
   *  "you may not do this" apart from "this did not work". */
  permissionDenied: boolean;
}

/**
 * Run one action and report exactly what happened.
 *
 * Never throws for an engine-side condition: a refusal, a failure and an
 * unreachable service all come back as an `error` outcome, because a thrown
 * error at a button's call site turns into VS Code's generic command-failure
 * toast and loses both the audit id and the role requirement. A programming
 * error (an unknown action id) still throws -- that is not an operator's
 * problem to read.
 */
export async function runDeployAction(
  port: DeployControlPort,
  request: DeployActionRequest,
): Promise<DeployOutcome> {
  const spec = actionById(request.id);
  try {
    const result = await invoke(port, request);
    if (result.ok) {
      // The audit id is the deliverable, not a decoration: it is what makes a
      // deploy traceable after the fact, and deployment-console.md specifies
      // this exact line shape for both the portal and the cockpit.
      return {
        kind: "success",
        line: auditSuffix(`SUCCESS: ${spec.verb}${detailSuffix(result.message)}`, result.auditEventId),
        auditEventId: result.auditEventId,
        permissionDenied: false,
      };
    }
    // Ran, did not work. `message` is the engine's own words, surfaced
    // verbatim rather than reworded -- a paraphrase is one more thing that can
    // be wrong, and the operator may need to match it against a log line.
    return {
      kind: "error",
      line: auditSuffix(
        `ERROR: ${result.message === "" ? `${spec.verb} failed` : result.message}`,
        result.auditEventId,
      ),
      auditEventId: result.auditEventId,
      permissionDenied: false,
    };
  } catch (err) {
    return errorOutcome(request.id, err);
  }
}

/**
 * Preview the next version proposals before cutting.
 *
 * Read-only, but developer-or-above gated server-side -- it is the read
 * companion to cutVersion, so it is refused exactly where cutVersion would be.
 * Returns null on any failure and hands the caller a message instead: a
 * missing preview must degrade to "type the version yourself", never to a
 * blocked Cut button.
 */
export async function previewNextVersion(
  port: DeployControlPort,
  env: string,
): Promise<{ suggestion: Awaited<ReturnType<DeployControlPort["suggestNextVersion"]>> | null; message: string }> {
  try {
    return { suggestion: await port.suggestNextVersion(env), message: "" };
  } catch (err) {
    return { suggestion: null, message: describeReadFailure("version suggestion", err) };
  }
}

/**
 * Read the per-env deployment status.
 *
 * Returns a message rather than throwing for the same reason the panel exists:
 * the status read is owner/admin-gated (#728) while topology and deployment
 * history are ordinary concept rows that any role can read. A developer or a
 * writer therefore gets PERMISSION_DENIED HERE and a perfectly good read
 * surface everywhere else, and the panel must show that as one explained
 * section rather than as an empty pane or a failed load.
 */
export async function readDeploymentStatus(
  port: DeployControlPort,
  env: string,
): Promise<{ status: Awaited<ReturnType<DeployControlPort["getDeploymentStatus"]>> | null; message: string }> {
  try {
    return { status: await port.getDeploymentStatus(env), message: "" };
  } catch (err) {
    return { status: null, message: describeReadFailure("deployment status", err) };
  }
}

function invoke(
  port: DeployControlPort,
  request: DeployActionRequest,
): Promise<{ ok: boolean; message: string; auditEventId: string }> {
  switch (request.id) {
    case "cutVersion":
      return port.cutVersion(request.env, request.bump, request.version);
    case "deploy":
      return port.deploy(request.deploymentId);
    case "promote":
      return port.promote(request.version);
    case "rollback":
      return port.rollbackDeployment(request.toDeploymentId);
    case "rolloutAction":
      return port.rolloutAction(request.env, request.rollout, request.subAction);
    default:
      // The union is exhaustive; this keeps a future variant from silently
      // resolving to some other RPC.
      throw new Error(`unsupported deploy action: ${JSON.stringify(request)}`);
  }
}

function errorOutcome(id: DeployActionId, err: unknown): DeployOutcome {
  const spec = actionById(id);
  if (err instanceof DeployControlError) {
    if (err.isPermissionDenied) {
      // NAMING THE ROLE is the requirement. The engine's message is appended
      // rather than replaced so nothing it said is lost, but the sentence an
      // operator reads first tells them which role would have worked.
      //
      // engineMessage, not `message`: the latter carries the SDK's log prefix
      // (`deploy console: <verb>: PERMISSION_DENIED: ...`), which restates the
      // verb this line already named and a code the line has already explained
      // in words (memql#3339).
      return {
        kind: "error",
        // The audit id of the BLOCKED attempt, appended in the same
        // `(audit <id>)` shape a success carries (memql#3334).
        //
        // This line used to end at the role requirement, with a comment
        // explaining that the id existed server-side but was not carried back:
        // the gate returned a bare status error and DeployControlResult
        // populated `action` only when the call was permitted. Saying nothing
        // was right then -- inventing a placeholder would have been worse than
        // the gap -- and #3334 closed the gap at the source rather than here,
        // so the panel now has a real id to print. auditSuffix still renders
        // nothing when it is empty, which is what an older engine returns.
        line: auditSuffix(
          `ERROR: ${spec.label} requires the ${tierDescription(spec.tier)} cluster role${detailSuffix(err.engineMessage)}`,
          err.auditEventId,
        ),
        auditEventId: err.auditEventId,
        permissionDenied: true,
      };
    }
    if (err.code === CODE_UNIMPLEMENTED) {
      return unreachable(
        `ERROR: ${spec.label} is unavailable -- this node does not host the deploy-control service. Connect to a cluster whose bff runs it.`,
      );
    }
    if (err.code === CODE_UNAUTHENTICATED) {
      // Also a GATE refusal, and also audited (the "no authenticated actor"
      // branch of authorizeWith writes a blocked event with an empty actor).
      // So the id is threaded here for the same reason as above, even though
      // this outcome is not `permissionDenied` -- the operator's next move is
      // to fix their token, but the attempt is still on the record.
      return unreachable(
        `ERROR: ${spec.label} was rejected as unauthenticated -- the connection carries no resolvable actor. Check the cluster's access token (an expired one renews on reconnect; a PAT never authenticates here).`,
        err.auditEventId,
      );
    }
    // The catch-all for a code with no tailored sentence. It names the action
    // and the code itself, so the SDK's log-shaped `message` would restate
    // both -- engineMessage is the part that is not already on the line.
    //
    // err.auditEventId is passed rather than "" because it is self-correcting:
    // the SDK sets it only when the engine attached one, so a code that was
    // not audited (UNIMPLEMENTED above, INVALID_ARGUMENT here) carries "" and
    // renders nothing.
    return unreachable(
      `ERROR: ${spec.label} failed (${err.codeName})${detailSuffix(err.engineMessage)}`,
      err.auditEventId,
    );
  }
  return unreachable(`ERROR: ${err instanceof Error ? err.message : String(err)}`);
}

function unreachable(line: string, auditEventId = ""): DeployOutcome {
  return { kind: "error", line: auditSuffix(line, auditEventId), auditEventId, permissionDenied: false };
}

/** Describe a failed READ, which never has an audit id (reads are unaudited). */
function describeReadFailure(what: string, err: unknown): string {
  if (err instanceof DeployControlError && err.isPermissionDenied) {
    // Spelled out because it is the designed split an operator meets first
    // (memql#3332): the read gate is owner/admin, so a developer sees topology
    // and history and is refused this one section. Nothing is broken.
    return `${what} requires the owner or admin cluster role. Topology and deployment history above are ordinary concept rows and are unaffected.`;
  }
  if (err instanceof DeployControlError && err.code === CODE_UNIMPLEMENTED) {
    return `${what} is unavailable: this node does not host the deploy-control service.`;
  }
  if (err instanceof DeployControlError) {
    return `${what} unavailable (${err.codeName})${detailSuffix(err.engineMessage)}`;
  }
  return `${what} unavailable: ${err instanceof Error ? err.message : String(err)}`;
}

function detailSuffix(message: string): string {
  return message === "" ? "" : ` -- ${message}`;
}

function auditSuffix(line: string, auditEventId: string): string {
  return auditEventId === "" ? line : `${line} (audit ${auditEventId})`;
}
