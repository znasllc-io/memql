// Claiming a cluster: the magic link, host side (znasllc-io/memql#3884).
//
// WHAT WAS MISSING. A MemQL cluster is claimed by its FIRST SIGN-IN, and the
// distinction that matters is between the ACCOUNT and the CREDENTIAL. The
// account is already there: an install that seeds the bootstrap values gets its
// owner row written on the identity node's own first boot
// (`App.provisionBootstrapOwner`, memql#3591), which is why `/setup` 404s on a
// freshly installed cluster. What does not exist yet is any way to authenticate
// as that owner, and the magic link is the first one offered. The install's
// `magicLink` step recovers it out of the identity workload's log and returns it
// on `result.link`, and until this module existed NOTHING READ IT. The only
// occurrence of `magicLink` anywhere in the extension was the script path in
// the runner's table.
//
// The consequence was not a missing convenience, it was a dead end. The step
// after it, `enrolmentLink`, points an operator at the owner magic link when
// there is nothing to enrol against -- naming the link the install had just
// thrown away. The operator was then offered SIGN IN for an account holding no
// credential, which times out and falls back to a device code that cannot
// complete either.
//
// So this is `enrolment.ts`'s sibling, and deliberately its mirror in shape:
// same injected host capabilities, same validate-before-open discipline, same
// refusal to hand a credential back to a caller that did not ask for it. Read
// that file's header for why the opening cannot live in the capability script
// and why `vscode` cannot be imported here.
//
// THE LINK IS A SINGLE-USE CREDENTIAL, and a more dangerous one than the
// enrolment token: it authenticates as the cluster OWNER. It is passed from the
// envelope to the opener and is never logged, never written to a file by this
// module, and never returned on a failure path. Note that the run log's
// `redactSecrets` would NOT catch it -- it matches provider keys only -- which
// is why `install/secrets.ts` grew `redactResult` for step results (memql#3886).

import type { ExecutionReport, StepOutcome } from "./executor.js";

/** The graph step this module pairs with. */
export const CLAIM_STEP_ID = "magicLink";

/** The envelope field the recovered link arrives on. */
export const CLAIM_RESULT_FIELD = "link";

/**
 * The envelope field saying whether there was a link to recover.
 *
 * `recovered` or `none`. It exists because "no link in the window" is an
 * ORDINARY outcome -- the pod's log may have rotated, or this may be a re-run
 * against a cluster somebody already claimed -- and the step deliberately
 * reports it as success rather than failing the install (memql#3632). Without
 * reading it, a caller cannot tell that case from a step that never ran.
 */
export const CLAIM_STATE_FIELD = "linkState";

/** Binds to `vscode.env.asExternalUri`. Takes and returns an absolute URL. */
export type ExternalUriResolver = (url: string) => string | Promise<string>;

/** Binds to `vscode.env.openExternal`. The return value is ignored. */
export type ExternalOpener = (url: string) => unknown | Promise<unknown>;

export interface ClaimDeps {
  resolveExternalUri: ExternalUriResolver;
  openExternal: ExternalOpener;
}

/** Why a cluster could not be claimed. */
export type ClaimFailureReason =
  /** The step did not run, was skipped, or failed. */
  | "notRun"
  /** The step ran and found no link in the log window. */
  | "noneRecovered"
  /** The value is not an https /auth/complete?ml= URL. */
  | "malformed"
  /** No browser could be opened on this host. */
  | "browserUnavailable";

export class ClaimError extends Error {
  readonly reason: ClaimFailureReason;
  readonly underlying?: unknown;

  constructor(reason: ClaimFailureReason, message: string, underlying?: unknown) {
    super(message);
    this.name = "ClaimError";
    this.reason = reason;
    this.underlying = underlying;
  }
}

/** The step's outcome, or undefined when it is not in the report. */
function claimOutcome(report: ExecutionReport): StepOutcome | undefined {
  return report.outcomes.find((o: StepOutcome) => o.id === CLAIM_STEP_ID);
}

/**
 * Pulls the recovered magic link out of an execution report.
 *
 * Returns "" when the step did not run or recovered nothing. An install that
 * recovered no link is not a broken install -- see CLAIM_STATE_FIELD -- so the
 * caller decides what to say about it.
 */
export function claimUrlFrom(report: ExecutionReport): string {
  const outcome = claimOutcome(report);
  if (outcome === undefined || outcome.status !== "ok" || outcome.envelope === null) return "";
  const value = outcome.envelope.result[CLAIM_RESULT_FIELD];
  return typeof value === "string" ? value : "";
}

/** Whether the step reported recovering a link, whatever this host then did with it. */
export function claimWasRecovered(report: ExecutionReport): boolean {
  const outcome = claimOutcome(report);
  if (outcome === undefined || outcome.envelope === null) return false;
  return outcome.envelope.result[CLAIM_STATE_FIELD] === "recovered";
}

/**
 * Validates the recovered link before anything is done with it.
 *
 * The same two checks `isEnrolmentUrl` makes, for the same two reasons, against
 * this link's own shape:
 *
 *  1. https. The link carries a plaintext bearer in its query string. The
 *     identity service builds it from its own configured base URL, so an http
 *     value here means something between the log line and this process rewrote
 *     it -- exactly the case not to open.
 *  2. The shape `/auth/complete?ml=...`, which is what
 *     `component/identity/magiclink/issuer.go` builds and what the step's own
 *     extraction pattern matches. A value that is some other URL is not "a
 *     magic link we do not recognise", it is a value from somewhere this module
 *     has no model of, and opening an arbitrary URL because a script printed it
 *     is the failure mode worth spending five lines to make impossible.
 */
export function isClaimUrl(candidate: string): boolean {
  let parsed: URL;
  try {
    parsed = new URL(candidate);
  } catch {
    return false;
  }
  if (parsed.protocol !== "https:") return false;
  if (parsed.pathname !== "/auth/complete") return false;
  return (parsed.searchParams.get("ml") ?? "") !== "";
}

/**
 * Opens the magic link in the operator's browser.
 *
 * `asExternalUri` runs first and is not optional: without it the URL breaks
 * under Remote-SSH, Codespaces and devcontainers, where the extension host and
 * the browser are on different machines.
 */
export async function openClaimLink(url: string, deps: ClaimDeps): Promise<void> {
  if (!isClaimUrl(url)) {
    throw new ClaimError(
      "malformed",
      "The installer produced something that is not an https sign-in link, so it was not opened.",
    );
  }
  let external: string;
  try {
    external = await deps.resolveExternalUri(url);
  } catch (err) {
    throw new ClaimError(
      "browserUnavailable",
      `The sign-in link could not be resolved into one this host can open (${errorText(err)}).`,
      err,
    );
  }
  try {
    await deps.openExternal(external);
  } catch (err) {
    throw new ClaimError(
      "browserUnavailable",
      `A browser could not be opened to claim the cluster (${errorText(err)}).`,
      err,
    );
  }
}

/**
 * The whole host-side step: find the link, open it, report what happened.
 *
 * Returns the link on success so a caller can say "we opened this" without
 * re-reading the report. It does NOT return the link on failure -- a caller
 * that could not open a browser should show the operator the link, and it gets
 * that by calling `claimUrlFrom` itself, deliberately, rather than by catching
 * an error that happens to carry a cluster-owner credential.
 */
export async function completeClaim(report: ExecutionReport, deps: ClaimDeps): Promise<string> {
  const url = claimUrlFrom(report);
  if (url === "") {
    const ran = claimOutcome(report) !== undefined;
    throw new ClaimError(
      ran ? "noneRecovered" : "notRun",
      ran
        ? "The install found no sign-in link in the identity log window, so there is nothing to open."
        : "The install did not run the sign-in step, so there is no link to open.",
    );
  }
  await openClaimLink(url, deps);
  return url;
}

function errorText(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
