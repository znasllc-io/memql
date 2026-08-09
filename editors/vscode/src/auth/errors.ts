// The failure taxonomy for the browser sign-in flow.
//
// -----------------------------------------------------------------------------
// WHY THIS IS A DISCRIMINATED TYPE AND NOT A STRING
// -----------------------------------------------------------------------------
//
// An interactive sign-in has more than one way to go wrong, and the RECOVERY
// differs per way. "the listener could not bind a loopback port" is a machine
// problem the operator fixes by freeing a port or checking a firewall; "the
// callback never arrived" is a person who closed the tab; "the state did not
// match" is a security refusal that must never read as a transient glitch. A
// caller that receives only a sentence has to pattern-match on prose to tell
// those apart, which is how a retry loop ends up retrying a refusal.
//
// So every failure here carries a `kind`. The UI layer (memql#3411) branches on
// it to decide what to say, what to offer, and whether a retry is even sensible.
// The kinds are a CONTRACT: renaming one, or folding two together, changes what
// a downstream consumer can distinguish. Add rather than merge.
//
//   misconfigured       The cluster names no identity service, so there is
//                       nowhere to register or authorize. Nothing was
//                       attempted. Fix: set `issuer` (or `domain`).
//                       NOT retryable.
//
//   registrationFailed  POST <issuer>/register was refused, unreachable, or
//                       returned a body carrying no client_id. No browser was
//                       opened. Retryable once the server is reachable /
//                       MEMQL_IDENTITY_OAUTH_DCR_ENABLED is on.
//
//   bindFailed          The one-shot loopback listener could not bind
//                       127.0.0.1:0. Nothing was opened and no code exists.
//                       A local machine problem, not a server one.
//
//   timeout             The listener bound and the browser was opened, but no
//                       request reached the callback path within the deadline.
//                       The usual cause is a person who never finished (or
//                       never saw) the sign-in page. Retryable.
//
//   cancelled           The caller aborted -- an AbortSignal fired, or the
//                       listener was closed while still waiting. Deliberate;
//                       a UI should say nothing louder than "sign-in
//                       cancelled".
//
//   browserUnavailable  This host could not open a browser at all -- either
//                       asExternalUri could not resolve the URL for it, or
//                       openExternal failed. An ENVIRONMENT LIMITATION, not a
//                       refusal: a headless box, a container with no desktop
//                       session, an SSH session with nothing to hand the URL
//                       to. Nobody declined anything and no code exists.
//
//                       It is deliberately NOT `cancelled`, and the distinction
//                       is a contract rather than a nicety. The device-code
//                       fallback (memql#3411) triggers on environment
//                       limitations and explicitly does NOT trigger on user
//                       cancellation, so filing this under `cancelled` would
//                       strand precisely the headless user the fallback exists
//                       to serve. This kind is a FALLBACK TRIGGER.
//
//   authorizationDenied The callback arrived carrying an OAuth error envelope
//                       (?error=access_denied&...). The server or the user
//                       refused. No code was issued.
//
//   stateMismatch       The callback's `state` did not equal the value this
//                       flow generated. A SECURITY refusal: the code is
//                       discarded and NOT exchanged. Never auto-retry silently
//                       -- something replayed or forged a callback.
//
//   invalidCallback     The callback arrived on the right path but carried
//                       neither a code nor an error, or was otherwise
//                       unreadable. A malformed authorization server response.
//
//   exchangeRejected    POST <issuer>/oauth/token refused the
//                       authorization_code grant, was unreachable, or returned
//                       a body with no access_token. The code is spent either
//                       way -- a retry means a whole new flow.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

export type AuthFlowErrorKind =
  | "misconfigured"
  | "registrationFailed"
  | "bindFailed"
  | "timeout"
  | "cancelled"
  | "browserUnavailable"
  | "authorizationDenied"
  | "stateMismatch"
  | "invalidCallback"
  | "exchangeRejected";

/**
 * The single error type every function in `src/auth/` rejects with.
 *
 * `message` is a sentence an operator can act on; `kind` is what code branches
 * on. Both are always present -- a consumer never has to parse the message to
 * recover the category.
 */
export class AuthFlowError extends Error {
  readonly kind: AuthFlowErrorKind;

  constructor(kind: AuthFlowErrorKind, message: string, options?: { cause?: unknown }) {
    super(message);
    // `name` is set explicitly because a bundler may rename the class, and
    // isAuthFlowError() below reads it as the structural fallback.
    this.name = "AuthFlowError";
    this.kind = kind;
    if (options?.cause !== undefined) {
      (this as { cause?: unknown }).cause = options.cause;
    }
  }
}

/**
 * isAuthFlowError narrows an unknown rejection.
 *
 * It checks the shape as well as the prototype: this module is bundled
 * separately for the extension and for each test entry point (esbuild.js /
 * esbuild.test.js), so two copies of the class can legitimately coexist in one
 * process and `instanceof` alone would report false across that boundary.
 */
export function isAuthFlowError(err: unknown): err is AuthFlowError {
  if (err instanceof AuthFlowError) return true;
  if (typeof err !== "object" || err === null) return false;
  const candidate = err as { name?: unknown; kind?: unknown };
  return candidate.name === "AuthFlowError" && typeof candidate.kind === "string";
}

/** errorText renders an unknown thrown value as a sentence fragment. */
export function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
