// Taking ownership of a local cluster (znasllc-io#3905, #3885).
//
// WHAT AN OPERATOR IS ACTUALLY STUCK ON. A memQL cluster's owner account is
// created by `seedBootstrap`, from the values the installer seeds. That account
// then holds NO human credential -- no passkey, no magic-link identity -- and
// the only thing that gives it one is a single-use enrolment link minted inside
// the cluster.
//
// The install mints that link, and an operator who dismisses the notification,
// whose link expires, or who installed before the done screen offered it, has
// no route back to one. Clicking the cluster offers Sign in, which cannot
// succeed: there is no credential to sign in with, and no way to make one.
//
// So this mints a FRESH link on demand. It is not a recovery of the install's
// link -- that one is single-use and short-lived, and replaying a credential is
// the wrong shape anyway. `install.enrolmentLink` is the same capability the
// install runs, invoked the same way, so the two paths cannot diverge.
//
// WHY NOT OPEN THE PORTAL. The portal is the cluster's operations console and
// it authenticates like everything else, so opening it before the operator
// holds a credential lands them on a login page for an account with nothing to
// log in WITH. `/enroll` is the page that mints the first credential; the portal
// is where they go once they have one. The ordering is not a preference.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go)
// -- the host capabilities arrive injected, exactly as `install/enrolment.ts`
// takes them.

import type { ClusterConfig } from "./model.js";
import { receiptNamesAnotherCluster } from "./ownershipRoute.js";
import { capabilityScriptPath, type CapabilityEnvelope, type ScriptOutcome } from "../install/runner.js";

/** The capability that mints the link, shared with the install graph. */
export const ENROLMENT_CAPABILITY = "install.enrolmentLink";

/** How long a link minted this way lives. */
export const OWNERSHIP_LINK_TTL = "2h";

/** Runs a capability script. Bound to `runCapabilityScript` by the caller. */
export type RunCapability = (run: {
  scriptPath: string;
  params: Record<string, string>;
  capability?: string;
  timeoutMs?: number;
}) => Promise<ScriptOutcome>;

export interface TakeOwnershipInputs {
  cluster: ClusterConfig;
  /** The owner to enrol, off the install receipt. */
  ownerEmail: string;
  /**
   * The domain that same receipt was recorded against (`recordedDomain`).
   *
   * Carried so the mint can check that the owner it is about to name came from
   * a receipt describing THIS cluster -- see the refusal in mintOwnershipLink.
   */
  receiptDomain: string;
  /** Where the capability scripts live. */
  repoRoot: string;
}

/** Why ownership could not be taken. */
export type OwnershipFailureReason =
  /** The cluster is not one this machine installed, so there is no pod to mint in. */
  | "notLocal"
  /** No owner email is recorded, so there is no account to name. */
  | "noOwner"
  /** The recorded owner belongs to a DIFFERENT cluster's install. */
  | "otherCluster"
  /** The mint itself failed. */
  | "mintFailed"
  /** It succeeded and produced no link, which should not happen. */
  | "noLink";

export class OwnershipError extends Error {
  readonly reason: OwnershipFailureReason;
  constructor(reason: OwnershipFailureReason, message: string) {
    super(message);
    this.name = "OwnershipError";
    this.reason = reason;
  }
}

/**
 * The identity base URL a link should point at.
 *
 * From the cluster's DOMAIN rather than its endpoint: the endpoint is the api
 * front door and the link has to reach the identity one. A cluster with no
 * domain yields "" and the script falls back to the pod's own
 * MEMQL_IDENTITY_BASE_URL, which is the right answer when this side knows less
 * than the cluster does.
 */
export function identityBaseUrlForCluster(cluster: ClusterConfig): string {
  const domain = (cluster.domain ?? "").trim().replace(/^\.+|\.+$/g, "");
  return domain === "" ? "" : `https://identity.${domain}`;
}

/**
 * Mints a single-use enrolment link for this cluster's owner.
 *
 * Returns the URL. It is a CREDENTIAL: the caller opens it and does not log it,
 * store it, or render it into a webview.
 */
export async function mintOwnershipLink(
  inputs: TakeOwnershipInputs,
  run: RunCapability,
): Promise<string> {
  if (inputs.cluster.local !== true) {
    throw new OwnershipError(
      "notLocal",
      `"${inputs.cluster.name}" is not a cluster on this machine, so an enrolment link cannot be minted here. ` +
        "Ask an owner or admin of that cluster to send you one.",
    );
  }
  const email = inputs.ownerEmail.trim();
  if (email === "") {
    throw new OwnershipError(
      "noOwner",
      "No owner email is recorded for this cluster, so there is no account to enrol. " +
        "Re-run the installer, which records the owner it bootstraps.",
    );
  }
  // THE RECEIPT IS ONE FILE AND THE CLUSTER LIST IS MANY (memql#3906).
  //
  // `~/.memql/install-receipt.json` describes whichever cluster was installed
  // LAST, so an owner read out of it is only evidence about the cluster whose
  // domain it was recorded against. Without this check, taking ownership of a
  // second local cluster would name the first one's owner -- and the mint runs
  // `kubectl exec` against the CURRENT context, so the name it carries and the
  // cluster it lands on are chosen independently.
  //
  // Refused rather than degraded. The plausible-looking alternative -- mint
  // anyway and let the cluster reject an address it does not know -- turns a
  // question this side can answer into a failure the operator has to interpret,
  // and it would silently succeed on the day two clusters share an owner.
  //
  // It refuses on a CONTRADICTION, not on a gap: a cluster with no recorded
  // domain still mints, because that is the case the fallback below is written
  // for. See receiptNamesAnotherCluster.
  if (receiptNamesAnotherCluster(inputs.cluster.domain ?? "", inputs.receiptDomain)) {
    throw new OwnershipError(
      "otherCluster",
      `The install receipt on this machine records an owner for a different cluster, so it cannot name the account to enrol on "${inputs.cluster.name}". ` +
        "Repair this cluster from the wizard, which re-records the owner it bootstraps.",
    );
  }

  const params: Record<string, string> = {
    local: "true",
    "user-email": email,
    ttl: OWNERSHIP_LINK_TTL,
  };
  const base = identityBaseUrlForCluster(inputs.cluster);
  if (base !== "") params["base-url"] = base;

  const outcome = await run({
    scriptPath: capabilityScriptPath(ENROLMENT_CAPABILITY, inputs.repoRoot),
    params,
    capability: ENROLMENT_CAPABILITY,
    timeoutMs: 120_000,
  });

  const envelope: CapabilityEnvelope | null = outcome.envelope;
  if (outcome.exitCode !== 0 || envelope === null || envelope.ok !== true) {
    const detail = envelope?.error?.message ?? `the mint exited ${outcome.exitCode}`;
    throw new OwnershipError("mintFailed", `An enrolment link could not be minted: ${detail}`);
  }

  const url = envelope.result[ENROLMENT_RESULT_FIELD];
  if (typeof url !== "string" || url.trim() === "") {
    // `enrolmentState` says which of the two empty cases this is, and they ask
    // the operator for different things -- so the message names it rather than
    // reporting one blank for both.
    const state = envelope.result["enrolmentState"];
    throw new OwnershipError(
      "noLink",
      state === "awaitingFirstSignIn"
        ? "This cluster has no owner account yet -- the first sign-in is what creates it, so there is nothing to enrol against."
        : "The mint reported success and produced no link.",
    );
  }
  return url.trim();
}

/** The envelope field the link arrives on, shared with `install/enrolment.ts`. */
export const ENROLMENT_RESULT_FIELD = "enrolUrl";
