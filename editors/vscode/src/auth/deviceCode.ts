// The RFC 8628 device authorization grant, and the rule for when to reach for it.
//
// -----------------------------------------------------------------------------
// WHAT THIS IS FOR
// -----------------------------------------------------------------------------
//
// The loopback flow (flow.ts) is the good path: the browser comes back to a
// port on this machine and nobody transcribes anything. It needs two things
// this host may not have -- a bindable loopback port, and a browser that can
// reach it. A container, a hardened corporate network, a headless box over SSH:
// each breaks one of those, and on each the loopback flow fails in a way no
// amount of retrying fixes.
//
// This is the fallback. The editor asks the identity service for a pair of
// codes, shows the human one of them, and polls until the human has approved it
// on whatever device they DO have a browser on. It costs a round trip through a
// second screen, which is exactly why it is a fallback and not the default.
//
// -----------------------------------------------------------------------------
// THE FAILURE-DETECTION RULE (shouldFallBackToDeviceCode)
// -----------------------------------------------------------------------------
//
// This is the load-bearing decision in the file, and it is wrong in BOTH
// directions:
//
//   TOO EAGER and the good path disappears. Every sign-in that hiccups drops to
//   a flow that makes the user type a code into a phone -- and worse, a
//   SECURITY refusal retried through a second channel is a refusal silently
//   worked around.
//
//   TOO LATE and the user is stranded. A headless host reports a perfectly
//   clear "no browser here" and gets an error message instead of the flow built
//   for precisely that host.
//
// So the trigger set is the ENVIRONMENT LIMITATIONS, named by `kind` and never
// by message text (errors.ts exists so this branch can be exact):
//
//   bindFailed          The loopback listener could not bind. Nothing was
//                       opened, no code exists, and the machine will refuse the
//                       next attempt identically.
//   timeout             The listener bound and the browser was opened, but
//                       nothing came back within the deadline. On a remote or
//                       firewalled host that is what "the browser could not
//                       reach 127.0.0.1" looks like from in here.
//   browserUnavailable  This host cannot open a browser at all.
//
// browserUnavailable IS a trigger, deliberately. It was split out of
// `cancelled` by memql#3402 for this exact decision: nobody declined anything,
// the host simply has nowhere to hand a URL, and that is the definition of the
// category this fallback serves. Filing it with `cancelled` would strand the
// headless user the fallback exists for -- and the taxonomy's own header says
// so ("This kind is a FALLBACK TRIGGER").
//
// And the non-triggers, which are errors rather than limitations:
//
//   cancelled           The user stopped it. Starting a second flow they did
//                       not ask for is the opposite of honouring that.
//   stateMismatch       A security refusal. Retrying it through another channel
//                       is the bug this rule exists to prevent.
//   exchangeRejected    The token exchange was refused. A second grant type
//                       does not make a refused redemption succeed.
//   authorizationDenied / invalidCallback  The server answered, and answered no
//                       (or answered nonsense). Neither is an environment
//                       limitation.
//   misconfigured / registrationFailed  Both flows need the same issuer and the
//                       same client registration. If those failed, the device
//                       grant fails identically -- one step later and after
//                       wasting the user's time.
//
// -----------------------------------------------------------------------------
// THE WIRE CONTRACT (component/identity/http/device.go + token_device.go)
// -----------------------------------------------------------------------------
//
//   POST <issuer>/device/code
//     -> {device_code, user_code, verification_uri, verification_uri_complete,
//         expires_in, interval}
//
//   POST <issuer>/oauth/token
//     {grant_type: "urn:ietf:params:oauth:grant-type:device_code",
//      device_code, client_id, code_verifier}
//     -> 200 {access_token, refresh_token, expires_in, scope}
//     -> 400 {error: "authorization_pending"}  keep waiting
//     -> 400 {error: "slow_down", interval: N}  poll slower, FOREVER
//     -> 400 {error: "access_denied"}          the human said no. Terminal.
//     -> 400 {error: "expired_token"}          the window closed. Terminal.
//     -> 400 {error: "invalid_grant"}          unknown / replayed / wrong client
//
// `slow_down` is not advisory here. The server raises the interval on the
// PERSISTED row (token_device.go: `raised := interval + SlowDownIncrementSeconds`,
// capped at MaxIntervalSeconds, written through TouchPoll) and judges every
// later poll against the raised value, so a client that ignores it earns
// slow_down again on the next poll and ratchets itself to the 60-second ceiling.
// Honouring the `interval` the error body carries -- and falling back to the
// same +5/cap-60 arithmetic when a proxy strips it -- is what keeps this flow
// from throttling itself into its own expiry.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
// Everything the user SEES -- the progress notification, the copy button, the
// clickable link -- is the adapter's (deviceCodeUi.ts); this module only reports
// the code through `onUserCode` and polls.

import type { ClusterConfig } from "../clusters/model.js";
import {
  defaultFetch,
  oauthError,
  type FetchLike,
  type HttpResponseLike,
} from "../connection/credentials.js";
import { identityBaseUrlFor } from "../connection/endpoint.js";
import { AuthFlowError, errorText, isAuthFlowError, type AuthFlowErrorKind } from "./errors.js";
import { runAuthorizationFlow, type AuthFlowDeps, type AuthFlowTokens } from "./flow.js";
import { generatePkcePair } from "./pkce.js";
import { ensureClientId } from "./register.js";

/** The RFC 8628 grant_type URN. Must equal http.DeviceGrantType. */
export const DEVICE_GRANT_TYPE = "urn:ietf:params:oauth:grant-type:device_code";

/** Where the device authorization request goes, relative to the issuer. */
export const DEVICE_AUTHORIZATION_PATH = "/device/code";

/**
 * The poll floor to assume when the server names none.
 *
 * RFC 8628 §3.5 mandates 5 seconds for a client that received no `interval`;
 * identity returns it explicitly anyway (devicecode.DefaultIntervalSeconds).
 */
export const DEFAULT_POLL_INTERVAL_SECONDS = 5;

/** How much to raise the interval on a `slow_down` that carries no new one. */
export const SLOW_DOWN_INCREMENT_SECONDS = 5;

/** The ceiling on the escalation (devicecode.MaxIntervalSeconds). */
export const MAX_POLL_INTERVAL_SECONDS = 60;

/**
 * How many CONSECUTIVE transport failures the poll loop absorbs.
 *
 * A poll that cannot be delivered says nothing about the authorization: a
 * suspended laptop, a VPN reconnecting, a proxy dropping one request. Failing
 * the whole sign-in on the first of those would throw away a code the human may
 * already have approved. Three in a row is a different claim -- the service is
 * not there -- and continuing to the ten-minute expiry would be a ten-minute
 * wait for an answer already known.
 */
export const MAX_CONSECUTIVE_POLL_TRANSPORT_FAILURES = 3;

/** The kinds that mean "this environment cannot do loopback". See the header. */
const FALLBACK_TRIGGER_KINDS: ReadonlySet<AuthFlowErrorKind> = new Set<AuthFlowErrorKind>([
  "bindFailed",
  "timeout",
  "browserUnavailable",
]);

/** What POST /device/code returned, in this codebase's spelling. */
export interface DeviceAuthorization {
  /** The machine-held credential. Never shown to the user. */
  deviceCode: string;
  /** The human-held credential, in its XXXX-XXXX display form. */
  userCode: string;
  /** The page the human opens and types the code into. */
  verificationUri: string;
  /** The same page with `?user_code=` pre-filled. May be "" if the server sent none. */
  verificationUriComplete: string;
  /** How long the authorization is approvable for. 0 when the server named none. */
  expiresInSeconds: number;
  /** The poll floor the server set, already clamped. */
  intervalSeconds: number;
}

/** Injected so a test can drive the loop without real time passing. */
export type SleepFn = (ms: number, signal?: AbortSignal) => Promise<void>;

export interface DeviceCodeDeps {
  /** Defaults to the real network. */
  fetch?: FetchLike;
  /** Cancels the flow. Rejects with kind `cancelled` and STOPS POLLING at once. */
  signal?: AbortSignal;
  /** Injected clock, so the computed expiry is assertable. Defaults to Date.now. */
  now?: () => number;
  /** Injected delay, so a test does not wait out a real poll interval. */
  sleep?: SleepFn;
  /** Overrides the client_name shown on the approval screen. */
  clientName?: string;
  /**
   * Called ONCE, the moment the codes exist and before the first poll.
   *
   * This is the whole user-facing half of the flow: the caller shows the code
   * and the link, and keeps them on screen until this function's promise
   * settles. It is deliberately synchronous-looking (the return value is
   * ignored) so a slow UI cannot delay the poll clock.
   */
  onUserCode?: (authorization: DeviceAuthorization) => void;
}

/**
 * shouldFallBackToDeviceCode decides whether a failed loopback sign-in should
 * be retried through the device grant.
 *
 * True ONLY for the environment limitations listed in the header. Anything that
 * is not an AuthFlowError at all is false: an unrecognised throw is not
 * evidence that this host cannot open a browser, and guessing would be exactly
 * the over-eager half of the failure the rule exists to avoid.
 */
export function shouldFallBackToDeviceCode(err: unknown): boolean {
  return isAuthFlowError(err) && FALLBACK_TRIGGER_KINDS.has(err.kind);
}

export interface DeviceCodeFallbackDeps extends AuthFlowDeps, DeviceCodeDeps {
  /**
   * Called with the loopback failure that triggered the fallback, just before
   * the device flow starts. The caller uses it to explain the switch -- a flow
   * that silently changes shape reads as a bug.
   */
  onFallback?: (reason: AuthFlowError) => void;
}

/**
 * signInWithDeviceCodeFallback runs the loopback flow and, when this host
 * cannot do loopback, the device flow instead.
 *
 * The client registration happens HERE, once, and the resolved client_id is
 * handed to whichever flow runs. Letting each flow register its own would mint
 * a second OAuth client every time the fallback fires -- the loopback attempt
 * registers, throws, and loses the id it just created (an AuthFlowError carries
 * no payload), and nothing ever revokes the orphan. `clientIdWasRegistered` is
 * carried out of here so the caller still persists a freshly minted id exactly
 * once.
 */
export async function signInWithDeviceCodeFallback(
  cluster: ClusterConfig,
  deps: DeviceCodeFallbackDeps,
): Promise<AuthFlowTokens> {
  const issuer = requireIssuer(cluster);
  const { clientId, registered } = await ensureClientId({
    issuer,
    clientId: cluster.clientId,
    clientName: deps.clientName,
    fetch: deps.fetch ?? defaultFetch,
  });
  const authorized: ClusterConfig = { ...cluster, clientId };

  let tokens: AuthFlowTokens;
  try {
    tokens = await runAuthorizationFlow(authorized, deps);
  } catch (err) {
    if (!shouldFallBackToDeviceCode(err)) throw err;
    deps.onFallback?.(err as AuthFlowError);
    tokens = await runDeviceCodeFlow(authorized, deps);
  }
  return { ...tokens, clientIdWasRegistered: registered || tokens.clientIdWasRegistered };
}

/**
 * runDeviceCodeFlow signs the operator in through a code they approve on
 * another device, and returns the resulting tokens.
 *
 * The return type is `AuthFlowTokens` -- the SAME type the loopback flow
 * returns -- so the caller persists a device sign-in through the one code path
 * it already has (persistSignIn, memql#3404) rather than growing a second.
 */
export async function runDeviceCodeFlow(
  cluster: ClusterConfig,
  deps: DeviceCodeDeps = {},
): Promise<AuthFlowTokens> {
  const issuer = requireIssuer(cluster);
  const doFetch = deps.fetch ?? defaultFetch;
  const now = deps.now ?? (() => Date.now());
  const sleep = deps.sleep ?? delay;
  const signal = deps.signal;

  throwIfAborted(signal);

  const { clientId, registered } = await ensureClientId({
    issuer,
    clientId: cluster.clientId,
    clientName: deps.clientName,
    fetch: doFetch,
  });

  // PKCE, even though there is no redirect to intercept. identity binds the
  // challenge to the row at request time and REQUIRES the verifier at
  // redemption (token_device.go), so a device_code observed in transit is
  // useless without the verifier that never left this process.
  const pkce = generatePkcePair();

  const authorization = await requestDeviceAuthorization({
    issuer,
    clientId,
    codeChallenge: pkce.challenge,
    codeChallengeMethod: pkce.method,
    fetch: doFetch,
  });

  deps.onUserCode?.(authorization);

  const tokens = await pollForDeviceTokens({
    issuer,
    clientId,
    authorization,
    codeVerifier: pkce.verifier,
    fetch: doFetch,
    now,
    sleep,
    signal,
  });

  const nowSeconds = Math.floor(now() / 1000);
  return {
    ...tokens,
    expiresAtEpochSeconds: tokens.expiresInSeconds > 0 ? nowSeconds + tokens.expiresInSeconds : 0,
    clientId,
    clientIdWasRegistered: registered,
  };
}

interface DeviceAuthorizationRequest {
  issuer: string;
  clientId: string;
  codeChallenge: string;
  codeChallengeMethod: string;
  fetch: FetchLike;
}

/**
 * requestDeviceAuthorization performs POST <issuer>/device/code.
 *
 * JSON rather than form-encoded, matching every other request this extension
 * makes at the identity service; readDeviceAuthorizationRequest accepts both
 * and says so ("a hand-rolled editor extension usually posts the latter").
 *
 * A refusal here is `registrationFailed` rather than `exchangeRejected`: this
 * is the step before any credential exists, the analogue of /register failing
 * on the loopback path, and it is retryable the moment the server is willing.
 */
export async function requestDeviceAuthorization(
  request: DeviceAuthorizationRequest,
): Promise<DeviceAuthorization> {
  const url = `${request.issuer}${DEVICE_AUTHORIZATION_PATH}`;
  let response: HttpResponseLike;
  try {
    response = await request.fetch(url, {
      method: "POST",
      headers: { "content-type": "application/json", accept: "application/json" },
      body: JSON.stringify({
        client_id: request.clientId,
        code_challenge: request.codeChallenge,
        code_challenge_method: request.codeChallengeMethod,
      }),
    });
  } catch (err) {
    throw new AuthFlowError(
      "registrationFailed",
      `The identity service at ${request.issuer} could not be reached to start a device sign-in (${errorText(err)}).`,
      { cause: err },
    );
  }

  const raw = await response.text().catch(() => "");
  if (!response.ok) {
    throw new AuthFlowError(
      "registrationFailed",
      `${url} returned ${response.status}: ${oauthError(raw)}`,
    );
  }

  let parsed: {
    device_code?: unknown;
    user_code?: unknown;
    verification_uri?: unknown;
    verification_uri_complete?: unknown;
    expires_in?: unknown;
    interval?: unknown;
  };
  try {
    parsed = JSON.parse(raw) as typeof parsed;
  } catch {
    throw new AuthFlowError("registrationFailed", `${url} returned a body that is not JSON.`);
  }

  const deviceCode = readString(parsed.device_code);
  const userCode = readString(parsed.user_code);
  const verificationUri = readString(parsed.verification_uri);
  if (deviceCode === "" || userCode === "" || verificationUri === "") {
    throw new AuthFlowError(
      "registrationFailed",
      `${url} returned no device_code, user_code or verification_uri, so there is nothing to show or to poll with.`,
    );
  }

  return {
    deviceCode,
    userCode,
    verificationUri,
    verificationUriComplete: readString(parsed.verification_uri_complete),
    expiresInSeconds: readNonNegativeInt(parsed.expires_in),
    intervalSeconds: clampInterval(readNonNegativeInt(parsed.interval)),
  };
}

interface PollRequest {
  issuer: string;
  clientId: string;
  authorization: DeviceAuthorization;
  codeVerifier: string;
  fetch: FetchLike;
  now: () => number;
  sleep: SleepFn;
  signal?: AbortSignal;
}

type PolledTokens = Pick<
  AuthFlowTokens,
  "accessToken" | "refreshToken" | "expiresInSeconds" | "scope"
>;

/**
 * pollForDeviceTokens polls the token endpoint until the human answers.
 *
 * It SLEEPS FIRST. The very first poll cannot possibly succeed -- the code has
 * only just been put on screen -- and firing it immediately would spend the
 * one poll the server's clock allows before the interval, earning a slow_down
 * (and a permanently raised interval) for nothing.
 */
async function pollForDeviceTokens(request: PollRequest): Promise<PolledTokens> {
  const url = `${request.issuer}/oauth/token`;
  const startedAt = request.now();
  // A local backstop for the server's own window. Without it a server that
  // stopped answering `expired_token` -- or a row swept out from under us --
  // would poll until the editor closed.
  const deadline =
    request.authorization.expiresInSeconds > 0
      ? startedAt + request.authorization.expiresInSeconds * 1000
      : undefined;

  let interval = request.authorization.intervalSeconds;
  let transportFailures = 0;

  for (;;) {
    // Cancellation is checked here AND inside the sleep, so an abort during a
    // long interval stops the loop the instant it fires rather than at the end
    // of the wait it interrupted.
    throwIfAborted(request.signal);
    await request.sleep(interval * 1000, request.signal);
    throwIfAborted(request.signal);

    if (deadline !== undefined && request.now() >= deadline) {
      throw expiredError(request.authorization);
    }

    let response: HttpResponseLike;
    try {
      response = await request.fetch(url, {
        method: "POST",
        headers: { "content-type": "application/json", accept: "application/json" },
        body: JSON.stringify({
          grant_type: DEVICE_GRANT_TYPE,
          device_code: request.authorization.deviceCode,
          client_id: request.clientId,
          code_verifier: request.codeVerifier,
        }),
      });
    } catch (err) {
      transportFailures += 1;
      if (transportFailures >= MAX_CONSECUTIVE_POLL_TRANSPORT_FAILURES) {
        throw new AuthFlowError(
          "exchangeRejected",
          `The identity service at ${request.issuer} could not be reached to complete the device sign-in, ${transportFailures} attempts in a row (${errorText(err)}).`,
          { cause: err },
        );
      }
      continue;
    }
    transportFailures = 0;

    const raw = await response.text().catch(() => "");
    if (response.ok) return readTokenResponse(url, raw);

    const failure = readGrantError(raw);
    switch (failure.error) {
      case "authorization_pending":
        continue;
      case "slow_down":
        // The server has ALREADY raised its own floor and will judge the next
        // poll against the raised value. Take the number it sent; compute the
        // same +5 (capped) it would have if a proxy stripped the field.
        interval = clampInterval(
          failure.interval > 0 ? failure.interval : interval + SLOW_DOWN_INCREMENT_SECONDS,
        );
        continue;
      case "access_denied":
        throw new AuthFlowError(
          "authorizationDenied",
          "The device sign-in was declined on the approval page, so no credentials were issued. Start the sign-in again if that was not what you meant.",
        );
      case "expired_token":
        throw expiredError(request.authorization);
      default:
        throw new AuthFlowError(
          "exchangeRejected",
          `${url} refused the device sign-in with ${response.status}: ${oauthError(raw)}`,
        );
    }
  }
}

/**
 * The sentence an expired authorization gets.
 *
 * An ACTIONABLE message, not `expired_token`: the raw code is the server's
 * vocabulary, and the only thing the person reading it needs to know is that
 * the window closed and that starting again gives them a fresh code. The kind
 * is `timeout` because that is what this is -- a deadline that passed with
 * nobody finishing -- and it is the same kind the loopback path reports for the
 * same human behaviour.
 */
function expiredError(authorization: DeviceAuthorization): AuthFlowError {
  const minutes = Math.max(1, Math.round(authorization.expiresInSeconds / 60));
  return new AuthFlowError(
    "timeout",
    `The device sign-in code ${authorization.userCode} expired before it was approved. ` +
      `Codes last about ${minutes} minute${minutes === 1 ? "" : "s"} -- start the sign-in again to get a fresh one, ` +
      `then approve it at ${authorization.verificationUri}.`,
  );
}

function readTokenResponse(url: string, raw: string): PolledTokens {
  let parsed: {
    access_token?: unknown;
    refresh_token?: unknown;
    expires_in?: unknown;
    scope?: unknown;
  };
  try {
    parsed = JSON.parse(raw) as typeof parsed;
  } catch {
    throw new AuthFlowError("exchangeRejected", `${url} returned a body that is not JSON.`);
  }
  const accessToken = readString(parsed.access_token);
  if (accessToken === "") {
    throw new AuthFlowError("exchangeRejected", `${url} returned no access_token.`);
  }
  const scope = readString(parsed.scope);
  return {
    accessToken,
    refreshToken: readString(parsed.refresh_token),
    expiresInSeconds: readNonNegativeInt(parsed.expires_in),
    ...(scope === "" ? {} : { scope }),
  };
}

interface GrantError {
  error: string;
  /** Only `slow_down` carries one (token_device.go, deviceGrantErrorResponse). */
  interval: number;
}

function readGrantError(raw: string): GrantError {
  try {
    const parsed = JSON.parse(raw) as { error?: unknown; interval?: unknown };
    return { error: readString(parsed.error), interval: readNonNegativeInt(parsed.interval) };
  } catch {
    // A body that is not JSON is not one of the four RFC codes either, so it
    // falls through to the terminal branch with the raw text as its detail.
    return { error: "", interval: 0 };
  }
}

/**
 * delay resolves after `ms`, or rejects the moment `signal` aborts.
 *
 * The abort listener is removed on every exit. A flow that ran to completion
 * while a long-lived signal stayed alive would otherwise accumulate one
 * listener per poll.
 */
export function delay(ms: number, signal?: AbortSignal): Promise<void> {
  if (signal?.aborted === true) return Promise.reject(cancelledError());
  return new Promise<void>((resolve, reject) => {
    const done = (): void => {
      clearTimeout(timer);
      signal?.removeEventListener("abort", onAbort);
    };
    const onAbort = (): void => {
      done();
      reject(cancelledError());
    };
    const timer = setTimeout(() => {
      done();
      resolve();
    }, ms);
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

function cancelledError(): AuthFlowError {
  return new AuthFlowError("cancelled", "Device sign-in was cancelled.");
}

function throwIfAborted(signal?: AbortSignal): void {
  if (signal?.aborted === true) throw cancelledError();
}

function requireIssuer(cluster: ClusterConfig): string {
  const issuer = identityBaseUrlFor(cluster);
  if (issuer === undefined) {
    throw new AuthFlowError(
      "misconfigured",
      `No identity service URL is known for cluster "${cluster.name}", so there is nowhere to sign in -- set \`issuer\` (or \`domain\`) in clusters.yaml.`,
    );
  }
  return issuer;
}

/** clampInterval keeps a poll floor inside [DEFAULT, MAX]. */
export function clampInterval(seconds: number): number {
  if (!Number.isFinite(seconds) || seconds <= 0) return DEFAULT_POLL_INTERVAL_SECONDS;
  return Math.min(Math.max(Math.floor(seconds), DEFAULT_POLL_INTERVAL_SECONDS), MAX_POLL_INTERVAL_SECONDS);
}

function readString(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

function readNonNegativeInt(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? Math.max(0, Math.floor(value)) : 0;
}
