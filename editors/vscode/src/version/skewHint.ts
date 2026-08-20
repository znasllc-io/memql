// The sentence that would have ended the motivating incident.
//
// A extension built past v0.18.0 sent `AuthoringValidateBundleMsg.origin`. That
// field landed six hours AFTER the v0.18.0 tag was cut, so no release carries
// it; every cluster installed from a release refused it. And because the
// WebSocket bridge decoded with unknown fields on, the refusal came back as an
// error out of `readLoop` that SEVERED THE SESSION rather than failing one
// request. What the operator saw was:
//
//     ERROR (validate): stream closed
//
// Nothing anywhere said the cluster was simply older than the extension.
//
// IT IS A HINT, AND MUST READ AS ONE. A closed socket is consistent with
// version skew, and equally with a laptop waking from sleep, a pod restarting,
// an ingress timing out, or a token expiring mid-stream. The extension cannot
// prove causation from a socket that went away, and wording that asserted it
// would do two kinds of damage: send an operator to upgrade a cluster over an
// unrelated blip, and -- worse -- get itself disbelieved the first time it was
// wrong, which costs the hint its value in the case where it is right.
//
// Composition lives here rather than at a render site so the wording is
// asserted once and every surface that reports a failure can reuse it.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { DEFAULT_STACK_TAG } from "../install/stackPin.js";
import { compareVersions } from "./compare.js";

export interface SkewHintInputs {
  /** The failure as it stands. Returned unchanged when no hint applies. */
  message: string;
  /**
   * The SDK's `PendingError.reason`, when the failure carried one.
   *
   * "transport" means the socket went away underneath a request -- the read
   * side (`pendingError("stream closed", "transport")`) or the write side
   * (`"send failed: ..."`), which are the same evidence. "closed" means the
   * dispatcher was stopped or the request aborted: that is US hanging up, and
   * hinting at version skew when the extension itself closed the socket would be
   * nonsense.
   */
  reason?: string;
  /** The cluster's recorded version (memql#3990). */
  recorded: string | undefined;
  /** The release this extension is built against. Defaults to the shipped pin. */
  extensionTag?: string;
}

/**
 * Whether a thrown value is the SDK's transport-level failure.
 *
 * Structural rather than a message match: the message varies between the read
 * and write sides, while `reason` is the SDK's own name for the class.
 */
export function isTransportClose(err: unknown): boolean {
  return (
    typeof err === "object" &&
    err !== null &&
    (err as { reason?: unknown }).reason === "transport"
  );
}

/**
 * `message`, with one sentence appended when version skew could explain it.
 *
 * Appends nothing unless ALL of the following hold, because each is a way the
 * hint would otherwise be wrong:
 *
 *   - the failure is a transport-level close (an application error means the
 *     socket was fine, so a closed-session explanation does not apply);
 *   - the cluster's version is RECORDED and COMPARABLE (a branch name or the
 *     build stamp cannot be shown to be behind anything, and claiming it would
 *     be the same confident-and-wrong move `compare.ts` refuses);
 *   - and it is genuinely BEHIND this extension (a cluster at or ahead of the
 *     extension cannot be refusing a field the extension learned later, and telling
 *     a developer running a locally built cluster to upgrade would be wrong).
 */
export function withVersionSkewHint(inputs: SkewHintInputs): string {
  if (inputs.message === "") return inputs.message;
  if (inputs.reason !== "transport") return inputs.message;

  const extensionTag = inputs.extensionTag ?? DEFAULT_STACK_TAG;
  if (compareVersions(inputs.recorded, extensionTag) !== "behind") return inputs.message;

  const recorded = (inputs.recorded ?? "").trim();
  return (
    `${inputs.message} ` +
    `This cluster records ${recorded} and the extension is built against ${extensionTag}, ` +
    `so the session may have been closed by a field this extension sends that ${recorded} does not accept ` +
    `-- an older cluster refuses an unknown field by ending the session rather than by reporting an error, ` +
    `which looks exactly like this. Upgrading the cluster would rule it out.`
  );
}
