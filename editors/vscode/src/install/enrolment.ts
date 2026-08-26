// The install wizard's enrolment step, host side (memql#3408, memql#3906).
//
// WHY THE OPENING IS NOT IN THE STEP. A capability script's product is a JSON
// envelope on stdout; it has no browser and must not acquire one -- the same
// script has to behave identically whether a person or an automation ran it.
// And nothing under src/install/ may import `vscode` (the import rule in
// cmd/memql-lsp/vscodeimportrule_test.go), because cli.ts runs these modules
// under plain node. So the two host capabilities this needs arrive as injected
// functions, exactly as src/auth/flow.ts takes them for the sign-in flow.
//
// THIS MODULE NO LONGER READS THE RUN'S LINK (memql#3906). It used to lift
// `result.enrolUrl` off the execution report and open that, which was wrong in
// two ways that only show up after the install screen has been sitting open:
//
//   - the link is SINGLE-USE and short-lived, so the button was dead exactly
//     when an operator came back to it, and failed in a way that reads as the
//     feature being broken rather than as a link having expired;
//   - a run whose enrolment step produced nothing offered no route at all, so
//     the operator's only remaining option was a terminal.
//
// The remedy is to MINT ON DEMAND -- `clusters/takeOwnership.ts` runs the same
// `install.enrolmentLink` capability the graph runs, at the moment of the
// click. What survives here is the opener, which is still the one place a
// minted link is validated before a browser is pointed at it, and a reader for
// the one field of that step worth keeping: whether an owner account exists.
//
// THE LINK IS A CREDENTIAL. It is passed straight from the mint to the opener
// and is never logged, never written to a file by this module, and never
// returned to a caller that has not asked for it.

import type { ExecutionReport, StepOutcome } from "./executor.js";

/** The graph step this module pairs with. */
export const ENROLMENT_STEP_ID = "enrolmentLink";

/**
 * The envelope field saying an owner ACCOUNT exists on the cluster.
 *
 * `enrolment-link.sh` reports it alongside `enrolmentState`, and it is the
 * cheapest honest answer to "is there anything here to enrol against" -- the
 * script has just asked the cluster, from inside it, with the only authority
 * that exists before anyone holds a credential.
 *
 * THE NAME OVERSTATES WHAT IT MEANS, so read it as this doc says and not as it
 * is spelled. An install that seeds the bootstrap values gets its owner ROW at
 * identity boot (memql#3591), so `ownerClaimed` is true on a cluster nobody has
 * signed into -- an account exists, no credential does, and the cluster is not
 * claimed in the sense `Store.HasClaimedOwner` means. Reading it as "somebody
 * has signed in" is wrong in exactly the window this extension operates in.
 */
export const OWNER_CLAIMED_FIELD = "ownerClaimed";

/**
 * Binds to `vscode.env.asExternalUri`. Takes and returns an absolute URL
 * string.
 */
export type ExternalUriResolver = (url: string) => string | Promise<string>;

/** Binds to `vscode.env.openExternal`. The return value is ignored. */
export type ExternalOpener = (url: string) => unknown | Promise<unknown>;

export interface EnrolmentDeps {
  resolveExternalUri: ExternalUriResolver;
  openExternal: ExternalOpener;
}

/**
 * Why an enrolment link could not be opened.
 *
 * The two "the run produced nothing" reasons went with the replay path
 * (memql#3906). Nothing mints a link and then asks this module to find it any
 * more, so a mint that produces nothing is `OwnershipError`'s to report, from
 * where it happened.
 */
export type EnrolmentFailureReason =
  /** The value is not an https /enroll?code= URL. */
  | "malformed"
  /** No browser could be opened on this host. */
  | "browserUnavailable";

export class EnrolmentError extends Error {
  readonly reason: EnrolmentFailureReason;
  /** The underlying failure, when there was one. Not an Error `cause` option:
   * this package's lib target predates it, and a silently-dropped cause is
   * worse than an explicit field. */
  readonly underlying?: unknown;

  constructor(reason: EnrolmentFailureReason, message: string, underlying?: unknown) {
    super(message);
    this.name = "EnrolmentError";
    this.reason = reason;
    this.underlying = underlying;
  }
}

/**
 * Whether the run established that this cluster has an owner ACCOUNT.
 *
 * The done screen's question, and deliberately not "did the run produce a
 * link": a link is a perishable artefact of one moment, an owner account is a
 * durable fact about the cluster, and it is the fact that decides whether
 * enrolment is the route to offer.
 *
 * FALSE is returned for a step that did not run, was skipped, or failed. That
 * is the honest reading -- nothing was established -- and it costs the operator
 * only the offer, not the capability: `memql.clusters.takeOwnership` is
 * reachable from the Clusters tree whatever this says.
 */
export function ownerAccountExistsFrom(report: ExecutionReport): boolean {
  const outcome = report.outcomes.find((o: StepOutcome) => o.id === ENROLMENT_STEP_ID);
  if (outcome === undefined || outcome.status !== "ok" || outcome.envelope === null) return false;
  return outcome.envelope.result[OWNER_CLAIMED_FIELD] === true;
}

/**
 * Validates the minted link before anything is done with it.
 *
 * TWO CHECKS, BOTH LOAD-BEARING.
 *
 *  1. https. The link carries a plaintext bearer in its query string, and the
 *     server refuses to mint an http one -- so an http value here means
 *     something between the mint and this process rewrote it, which is exactly
 *     the case not to open.
 *  2. The shape `/enroll?code=...`. The step's product is one specific URL. A
 *     value that is some other URL is not "an enrolment link we do not
 *     recognise", it is a value from somewhere this module has no model of,
 *     and opening an arbitrary URL because a script printed it is the failure
 *     mode worth spending five lines to make impossible.
 */
export function isEnrolmentUrl(candidate: string): boolean {
  let parsed: URL;
  try {
    parsed = new URL(candidate);
  } catch {
    return false;
  }
  if (parsed.protocol !== "https:") return false;
  if (parsed.pathname !== "/enroll") return false;
  return (parsed.searchParams.get("code") ?? "") !== "";
}

/**
 * Opens the enrolment link in the operator's browser.
 *
 * `asExternalUri` runs first and is not optional: without it the URL breaks
 * under Remote-SSH, Codespaces and devcontainers, where the extension host and
 * the browser are on different machines. A failure to open is
 * `browserUnavailable` rather than a generic error, because "this machine has
 * no browser" is a real, recoverable state -- the caller can fall back to
 * showing the link.
 */
export async function openEnrolmentLink(url: string, deps: EnrolmentDeps): Promise<void> {
  if (!isEnrolmentUrl(url)) {
    throw new EnrolmentError(
      "malformed",
      "The installer produced something that is not an https enrolment link, so it was not opened.",
    );
  }
  let external: string;
  try {
    external = await deps.resolveExternalUri(url);
  } catch (err) {
    throw new EnrolmentError(
      "browserUnavailable",
      `The enrolment link could not be resolved into one this host can open (${errorText(err)}).`,
      err,
    );
  }
  try {
    await deps.openExternal(external);
  } catch (err) {
    throw new EnrolmentError(
      "browserUnavailable",
      `A browser could not be opened for enrolment (${errorText(err)}).`,
      err,
    );
  }
}

function errorText(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
