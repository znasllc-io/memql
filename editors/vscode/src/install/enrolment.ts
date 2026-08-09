// The install wizard's enrolment step, host side (memql#3408).
//
// The `enrolmentLink` graph step mints a single-use passkey-enrolment link
// inside the cluster and puts it on `result.enrolUrl`. This module is the
// other half: it reads that link off the execution report and OPENS it, so
// the operator ends an install holding a passkey rather than a string they
// have to paste somewhere.
//
// WHY THE OPENING IS NOT IN THE STEP. A capability script's product is a JSON
// envelope on stdout; it has no browser and must not acquire one -- the same
// script has to behave identically whether a person or an automation ran it.
// And nothing under src/install/ may import `vscode` (the import rule in
// cmd/memql-lsp/vscodeimportrule_test.go), because cli.ts runs these modules
// under plain node. So the two host capabilities this needs arrive as injected
// functions, exactly as src/auth/flow.ts takes them for the sign-in flow.
//
// THE LINK IS A CREDENTIAL. It is passed straight from the envelope to the
// opener and is never logged, never written to a file by this module, and
// never returned to a caller that has not asked for it. It is already in the
// receipt -- the executor records every step's `result` verbatim -- which is
// noted below rather than silently relied on.

import type { ExecutionReport, StepOutcome } from "./executor.js";

/** The graph step this module pairs with. */
export const ENROLMENT_STEP_ID = "enrolmentLink";

/** The envelope field the step's link arrives on. */
export const ENROLMENT_RESULT_FIELD = "enrolUrl";

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

/** Why an enrolment link could not be opened. */
export type EnrolmentFailureReason =
  /** The step did not run, was skipped, or failed. */
  | "notRun"
  /** The step ran but produced no link. */
  | "missing"
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
 * Pulls the enrolment link out of an execution report.
 *
 * Returns "" when the step did not run or produced nothing -- an install that
 * skipped enrolment is not a broken install, and the caller decides whether to
 * say anything about it.
 */
export function enrolmentUrlFrom(report: ExecutionReport): string {
  const outcome = report.outcomes.find((o: StepOutcome) => o.id === ENROLMENT_STEP_ID);
  if (outcome === undefined || outcome.status !== "ok" || outcome.envelope === null) return "";
  const value = outcome.envelope.result[ENROLMENT_RESULT_FIELD];
  return typeof value === "string" ? value : "";
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

/**
 * The whole host-side step: find the link, open it, report what happened.
 *
 * Returns the link on success so a caller can say "we opened this" without
 * re-reading the report. It does NOT return the link on failure -- a caller
 * that could not open a browser should show the operator the link, and it gets
 * that by calling enrolmentUrlFrom itself, deliberately, rather than by
 * catching an error that happens to carry a credential.
 */
export async function completeEnrolment(
  report: ExecutionReport,
  deps: EnrolmentDeps,
): Promise<string> {
  const url = enrolmentUrlFrom(report);
  if (url === "") {
    const ran = report.outcomes.some((o) => o.id === ENROLMENT_STEP_ID);
    throw new EnrolmentError(
      ran ? "missing" : "notRun",
      ran
        ? "The enrolment step ran but produced no link, so there is nothing to open."
        : "The install did not run the enrolment step, so there is no link to open.",
    );
  }
  await openEnrolmentLink(url, deps);
  return url;
}

function errorText(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
