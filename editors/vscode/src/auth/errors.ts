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
//   registrationFailed  The DEVICE AUTHORIZATION REQUEST (POST
//                       <issuer>/device/code) was refused, unreachable, or
//                       returned an unreadable body. Nothing was opened and no
//                       code exists. Retryable once the server is reachable.
//
//                       The name is older than what it now covers: it used to
//                       mean POST <issuer>/register, back when this extension
//                       obtained its client_id by RFC 7591 dynamic client
//                       registration. That path is gone -- identity carries the
//                       editor as a compiled-in first-party client
//                       (wellKnownClient.ts) -- and the kind was kept rather
//                       than renamed because these kinds are a CONTRACT, and a
//                       rename is a breaking change for a downstream consumer
//                       branching on it. The kind is never shown to a person.
//
//   bindFailed          The one-shot loopback listener could not bind
//                       127.0.0.1:0. Nothing was opened and no code exists.
//                       A local machine problem, not a server one.
//
//   timeout             The listener bound and the browser was opened, but no
//                       request reached the callback path within the deadline.
//                       The usual cause is a person who never finished (or
//                       never saw) the sign-in page. Retryable. NOT a fallback
//                       trigger (memql#4594): a browser was opened, so a live
//                       tab may still complete, and auto-switching to a device
//                       code under it closes the listener that tab is about to
//                       redirect to. The advice names `MemQL: Sign In with
//                       Code` for the host that truly cannot receive the
//                       callback.
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

// -----------------------------------------------------------------------------
// WHY errorText WALKS THE CAUSE CHAIN (memql#4619)
// -----------------------------------------------------------------------------
//
// This extension targets Node 20 (esbuild.js), whose global `fetch` is undici,
// and undici reports EVERY transport failure the same way: it throws
// `TypeError: fetch failed` and puts the reason -- the DNS, socket or TLS error
// -- in `.cause`. Rendering `err.message` alone therefore reduced a wrong
// hostname, a firewall, an expired certificate and an unknown CA to one
// identical sentence on every sign-in surface at once:
//
//   The identity service at https://identity.example.com could not be reached
//   to start a device sign-in (fetch failed).
//
// Four problems with four different fixes, and a sentence that distinguishes
// none of them. So the chain is walked to the first link that names a `code`,
// and that code is what gets appended. `.errors` is walked as well as `.cause`,
// because a host that resolves to several addresses fails on each and undici
// wraps the set in an AggregateError whose OWN message is EMPTY -- which is the
// ordinary shape of "the local cluster is not running", and the one a
// cause-only walk reports as nothing at all.
//
// THE TLS CODES GET A SENTENCE OF THEIR OWN, because their fix is not guessable
// from the code and the most confusing one is the most common one: Node does
// not read the operating system trust store. A CA that the browser and curl on
// the very same machine already trust -- mkcert's local root, a corporate root
// -- is unknown to the extension host, so a perfectly good local cluster
// produces a certificate error that reads like a server problem when it is a
// client configuration one. An expired certificate and a wrong-hostname
// certificate are NOT that problem and deliberately get different sentences:
// offering NODE_EXTRA_CA_CERTS for either would send an operator to edit a
// setting that cannot help them.
//
// This is the ONE renderer. src/connection/credentials.ts imports it rather
// than keeping the identical copy it used to have, because two copies of the
// same prose is exactly how a fix to one of them leaves the refresh path still
// reporting a bare "fetch failed" while the sign-in path reports the truth.
// The copies in src/install/ and src/webview/ are the same hazard and are not
// this file's to remove (memql#4619 names them).

/**
 * How far down a `cause`/`errors` chain to look. Deep enough for undici's real
 * shape (TypeError -> AggregateError -> system error), shallow enough that an
 * error whose cause points back at itself cannot spin.
 */
const MAX_CAUSE_DEPTH = 6;

/** How many entries of an AggregateError to read. The first names the fault. */
const MAX_AGGREGATED = 4;

const TRUST_STORE_ADVICE =
  "Node does not read the operating system trust store, so a CA this machine's browser and curl already trust (a mkcert or corporate root) is still unknown to the editor: point NODE_EXTRA_CA_CERTS at the CA file and restart VS Code.";

/** The codes whose fix cannot be read off the code itself. */
const TLS_ADVICE: Record<string, string> = {
  UNABLE_TO_VERIFY_LEAF_SIGNATURE: TRUST_STORE_ADVICE,
  UNABLE_TO_GET_ISSUER_CERT: TRUST_STORE_ADVICE,
  UNABLE_TO_GET_ISSUER_CERT_LOCALLY: TRUST_STORE_ADVICE,
  SELF_SIGNED_CERT_IN_CHAIN: TRUST_STORE_ADVICE,
  DEPTH_ZERO_SELF_SIGNED_CERT: TRUST_STORE_ADVICE,
  CERT_HAS_EXPIRED:
    "The server's certificate is past its expiry date and must be reissued -- for a local cluster, regenerate the mkcert certificate. Node checks the date itself, so no trust setting makes an expired certificate acceptable.",
  ERR_TLS_CERT_ALTNAME_INVALID:
    "The certificate is valid but was not issued for this hostname, so check the `endpoint` or `domain` this cluster names, or reissue the certificate to cover that name.",
};

/**
 * errorText renders an unknown thrown value as a sentence fragment, including
 * the transport reason undici hides in `.cause` (memql#4619).
 */
export function errorText(err: unknown): string {
  let text = err instanceof Error ? err.message : String(err);

  const chain = causeChain(err);
  // The first link that names a `code` is the one that says what went wrong;
  // everything above it is undici's wrapper.
  const carrier = chain.find((link) => errorCode(link) !== "");
  const code = carrier === undefined ? "" : errorCode(carrier);

  if (carrier === undefined) {
    // No code anywhere -- but a wrapper plus an inner message still says more
    // than the wrapper alone ("fetch failed: socket hang up").
    const inner = chain
      .slice(1)
      .map((link) => errorMessage(link))
      .find((message) => message !== "" && !text.includes(message));
    if (inner !== undefined) text = `${text}: ${inner}`;
  } else if (carrier === err) {
    // The thrown value IS the system error: its message is already the head, so
    // only the code is missing.
    if (code !== "" && !text.includes(code)) text = `${text} (${code})`;
  } else {
    const detail = withCode(code, errorMessage(carrier));
    if (detail !== "" && !text.includes(detail)) text = `${text}: ${detail}`;
  }

  const advice = code === "" ? undefined : TLS_ADVICE[code];
  if (advice === undefined) return text;
  return `${text.endsWith(".") ? text : `${text}.`} ${advice}`;
}

/**
 * causeChain flattens a thrown value into itself plus everything it blames:
 * `.cause` all the way down, and the entries of an AggregateError on the way.
 * Depth-capped rather than cycle-detected -- the cap is what makes a
 * self-referential cause terminate, and it is cheaper than tracking identity.
 */
function causeChain(err: unknown, depth = 0): unknown[] {
  if (depth >= MAX_CAUSE_DEPTH || err === null || typeof err !== "object") return [];
  const link = err as { cause?: unknown; errors?: unknown };
  const aggregated = Array.isArray(link.errors) ? link.errors.slice(0, MAX_AGGREGATED) : [];
  return [
    err,
    ...aggregated.flatMap((inner: unknown) => causeChain(inner, depth + 1)),
    ...causeChain(link.cause, depth + 1),
  ];
}

/** errorCode reads a Node system error's `code`, or "" if it carries none. */
function errorCode(err: unknown): string {
  if (err === null || typeof err !== "object") return "";
  const code = (err as { code?: unknown }).code;
  return typeof code === "string" ? code.trim() : "";
}

/** errorMessage reads a link's message whether or not it is a real Error. */
function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message.trim();
  if (err === null || typeof err !== "object") return "";
  const message = (err as { message?: unknown }).message;
  return typeof message === "string" ? message.trim() : "";
}

/**
 * withCode prefixes a message with its code -- unless the message already
 * names it. Node's socket errors read "getaddrinfo ENOTFOUND host", and
 * "ENOTFOUND -- getaddrinfo ENOTFOUND host" is noise an operator has to read
 * past to reach the hostname that actually failed.
 */
function withCode(code: string, message: string): string {
  if (message === "") return code;
  if (code === "" || message.includes(code)) return message;
  return `${code} -- ${message}`;
}
