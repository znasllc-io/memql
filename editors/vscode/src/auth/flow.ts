// The browser sign-in flow: authorize -> callback -> exchange.
//
// -----------------------------------------------------------------------------
// WHAT THIS DOES AND DOES NOT OWN
// -----------------------------------------------------------------------------
//
// It RETURNS tokens. It does not store them, does not touch clusters.yaml, does
// not touch SecretStorage, and does not know a ConnectionManager exists.
// Persistence is memql#3404 and the connection wiring is memql#3403; keeping
// the split means this module can be exercised end to end with nothing but a
// stub fetch and a stub browser opener.
//
// -----------------------------------------------------------------------------
// WHY vscode.env.asExternalUri IS A PARAMETER
// -----------------------------------------------------------------------------
//
// The extension's unit tests run under bare `node --test` with no editor, which
// only works because no module carrying logic imports `vscode` -- that module
// exists solely inside a running VS Code and one import of it anywhere in a
// dependency graph makes the whole file unrunnable outside the editor
// (enforced by cmd/memql-lsp/vscodeimportrule_test.go). This flow genuinely
// needs two editor capabilities, so both arrive as plain functions the adapter
// layer binds:
//
//   resolveExternalUri  <- vscode.env.asExternalUri
//   openExternal        <- vscode.env.openExternal
//
// asExternalUri is not decoration. Under Remote-SSH, Codespaces or a
// dev-container the "browser" runs on a different machine from this extension
// host, and the URL has to be rewritten into one that machine can actually
// reach. Skipping it produces a sign-in that works on a laptop and silently
// fails for every remote user.
//
// IT IS ALSO NOT SUFFICIENT, AND THIS COMMENT USED TO CLAIM IT WAS (memql#4623).
// asExternalUri tunnels LOOPBACK authorities, and it was applied to the
// AUTHORIZE url -- an `https://identity...` URL, which comes back unchanged, so
// no tunnel was ever created for the callback port. The redirect_uri, which is
// the loopback one, was never passed through it at all. A remote user therefore
// got a browser on their machine redirecting to THEIR 127.0.0.1, where nothing
// listens; the bind succeeded and openExternal succeeded, so neither fallback
// trigger fired and they watched the whole 600-second deadline.
//
// The fix is a different REDIRECT rather than a rewritten one: `isRemote`
// selects the registered `vscode://` callback (uriCallback.ts), which the
// user's own VS Code client forwards across the remote boundary over the
// connection that already exists.
//
// -----------------------------------------------------------------------------
// WHY state IS CHECKED BEFORE THE EXCHANGE, NOT AFTER
// -----------------------------------------------------------------------------
//
// The loopback listener answers anything that reaches its port. `state` is the
// only evidence that the callback belongs to THIS flow rather than to something
// that guessed the port and walked a code of its choosing in. Exchanging first
// and checking after would mean redeeming an attacker-chosen code against this
// user's client -- so a mismatch rejects with `stateMismatch` and the code is
// never sent anywhere.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import type { ClusterConfig } from "../clusters/model.js";
import {
  defaultFetch,
  oauthError,
  type FetchLike,
  type HttpResponseLike,
} from "../connection/credentials.js";
import { identityBaseUrlFor } from "../connection/endpoint.js";
import { awaitUriCallback as awaitUriCallbackImpl } from "./uriCallback.js";
import {
  describeDiscoveryFailure,
  discoverAuthorizationServer,
  supportsAuthorizationCodeWithS256,
} from "./discovery.js";
import { AuthFlowError, errorText } from "./errors.js";
import {
  DEFAULT_CALLBACK_TIMEOUT_MS,
  startLoopbackListener,
  type CallbackParams,
  type LoopbackListener,
  type LoopbackOptions,
} from "./loopback.js";
import { generatePkcePair, generateState } from "./pkce.js";
import { resolveClientId, WELL_KNOWN_REDIRECT_URI_VSCODE } from "./wellKnownClient.js";

/** Binds to `vscode.env.asExternalUri`. Takes and returns an absolute URL string. */
export type ExternalUriResolver = (url: string) => string | Promise<string>;

/**
 * Binds to `vscode.env.openExternal`. THE RETURN VALUE IS NOT IGNORED.
 *
 * env.openExternal signals failure BOTH ways -- by rejecting, and by resolving
 * `false` -- and a handler for only the rejection misses the way most hosts
 * actually say no (memql#4618). This type used to be `unknown`, which is what
 * made discarding the answer read as deliberate rather than as an oversight.
 *
 * `PromiseLike`, not `Promise`, because the editor's own signature returns
 * `Thenable<boolean>` (interface Thenable<T> extends PromiseLike<T>): an
 * adapter that hands env.openExternal straight through would not typecheck
 * against `Promise`. `void` stays in the union because most bindings and every
 * test double resolve nothing at all, and only an EXPLICIT `false` is a
 * refusal.
 */
export type ExternalOpener = (url: string) => boolean | void | PromiseLike<boolean | void>;

/** Injected so a test can drive the listener without binding a real socket. */
export type LoopbackStarter = (options: LoopbackOptions) => Promise<LoopbackListener>;

export interface AuthFlowDeps {
  /** vscode.env.asExternalUri. Required -- see the header on remote hosts. */
  resolveExternalUri: ExternalUriResolver;
  /** vscode.env.openExternal. */
  openExternal: ExternalOpener;
  /** Defaults to the real network. */
  fetch?: FetchLike;
  /** Defaults to startLoopbackListener. */
  startListener?: LoopbackStarter;
  /** Overrides the callback deadline (loopback.ts owns the default). */
  timeoutMs?: number;
  /** Cancels an in-flight sign-in. Rejects with kind `cancelled`. */
  signal?: AbortSignal;
  /** Injected clock, so the computed expiry is assertable. Defaults to Date.now. */
  now?: () => number;
  /**
   * True when this extension host runs somewhere the user's browser cannot
   * reach by loopback -- Remote-SSH, Codespaces, a dev container (memql#4623).
   *
   * `vscode.env.remoteName !== undefined` is the editor's own answer, and it is
   * INJECTED rather than read here so this module stays free of `vscode`
   * imports and so the remote path is testable without one.
   */
  isRemote?: boolean;
  /**
   * Arms the vscode:// callback waiter. Defaults to the real one; injected so
   * the remote path can be driven under bare `node --test`.
   */
  awaitUriCallback?: typeof awaitUriCallbackImpl;
}

/**
 * What the flow needs of a callback route, whichever transport provides it.
 * The loopback listener carries more (host, port); nothing after the choice
 * looks at those.
 */
interface CallbackTransport {
  readonly redirectUri: string;
  waitForCallback(): Promise<CallbackParams>;
  close(): void;
}

export interface AuthFlowTokens {
  /** The identity-issued JWT access token to dial the bff with. */
  accessToken: string;
  /** The long-lived token that renews the access token. May be "" if none was issued. */
  refreshToken: string;
  /** Lifetime the server reported, in seconds. 0 when it reported none. */
  expiresInSeconds: number;
  /** Absolute expiry in epoch seconds, computed from the injected clock. 0 when unknown. */
  expiresAtEpochSeconds: number;
  /** The scope string the server returned, when it returned one. */
  scope?: string;
  /**
   * The client_id this flow authorized with: the cluster's override, or the
   * well-known first-party id. Reported so a caller can log or display it;
   * nothing persists it, because there is nothing minted to keep.
   */
  clientId: string;
}

/**
 * runAuthorizationFlow signs the operator in through their browser and returns
 * the resulting tokens.
 *
 * Every rejection is an AuthFlowError carrying a `kind` (see errors.ts) so the
 * caller can tell "the port would not bind" from "nobody finished the page"
 * from "that callback was forged".
 */
export async function runAuthorizationFlow(
  cluster: ClusterConfig,
  deps: AuthFlowDeps,
): Promise<AuthFlowTokens> {
  const issuer = identityBaseUrlFor(cluster);
  if (issuer === undefined) {
    throw new AuthFlowError(
      "misconfigured",
      `No identity service URL is known for cluster "${cluster.name}", so there is nowhere to sign in -- set \`issuer\` (or \`domain\`) in clusters.yaml.`,
    );
  }

  const doFetch = deps.fetch ?? defaultFetch;
  const now = deps.now ?? (() => Date.now());
  const startListener = deps.startListener ?? startLoopbackListener;

  // PRE-FLIGHT BEFORE THE BROWSER, NOT AFTER THE DEADLINE (memql#4624).
  //
  // This used to open a browser and park for 600 seconds without ever asking
  // whether the issuer exists. A wrong domain, an unreachable host, a bad
  // certificate, an old cluster and a host that is not MemQL at all ALL cost
  // the full deadline, and were then blamed on the browser: "No sign-in
  // callback arrived within 600 seconds. The browser page was never completed,
  // or it could not reach 127.0.0.1" -- wrong in every one of those cases.
  //
  // The sharpest one is a cluster predating the memql-vscode built-in client:
  // /authorize renders an HTML 400 "Unknown client" page that is never
  // redirected, so there is no callback AND no OAuth error envelope. Silence,
  // for ten minutes. One GET answers it in a round trip, which is what the
  // device path has always done (deviceCode.ts).
  //
  // The metadata is also USED, not merely checked -- the endpoints come from
  // the document rather than from hard-coded paths, so an identity service that
  // is not at `identity.<domain>` works instead of being unrecoverable without
  // hand-editing clusters.yaml.
  const discovery = await discoverAuthorizationServer({ baseUrl: issuer, fetch: doFetch });
  if (discovery.kind !== "ok") {
    // THE KIND SAYS WHICH OF TWO THINGS IS WRONG, because callers branch on it.
    // `unreachable` is a NETWORK failure before any credential exists, which is
    // the situation deviceCode.ts already calls `registrationFailed` and
    // describes as retryable the moment the server is willing. The other two
    // mean the host answered and is not a MemQL identity service, which no
    // retry fixes.
    throw new AuthFlowError(
      discovery.kind === "unreachable" ? "registrationFailed" : "misconfigured",
      describeDiscoveryFailure(issuer, discovery),
    );
  }
  const metadata = discovery.metadata;
  if (!supportsAuthorizationCodeWithS256(metadata)) {
    // Named here rather than discovered as a silent non-callback later. The
    // device grant is the fallback, and the caller already offers it.
    throw new AuthFlowError(
      "misconfigured",
      `${metadata.issuer} does not advertise the authorization-code grant with S256 PKCE, ` +
        `which is the only browser sign-in this extension performs.`,
    );
  }

  // A local constant lookup, not a network call: identity carries this client
  // compiled in (wellKnownClient.ts). The step that used to be the first thing
  // to fail -- POST /register against a cluster with DCR off -- is gone.
  const clientId = resolveClientId(cluster.clientId);

  const pkce = generatePkcePair();
  const state = generateState();

  // WHICH CALLBACK TRANSPORT, AND WHY IT IS A PROPERTY OF WHERE WE RUN (memql#4623).
  //
  // A loopback listener binds 127.0.0.1 on the machine THIS extension host runs
  // on. Under Remote-SSH, Codespaces or a dev container that is the server,
  // while the browser opens on the USER's machine and redirects to THEIR
  // loopback, where nothing is listening. Nothing detected it: the bind
  // succeeded, openExternal succeeded, and `timeout` stopped being a fallback
  // trigger in memql#4600 -- so the user waited out the full deadline and was
  // then told the browser could not reach 127.0.0.1, which was true and useless.
  //
  // The vscode:// redirect is resolved by the user's own VS Code client and
  // forwarded to this extension across the remote boundary, over the connection
  // that already exists. Both URIs are registered on the cluster
  // (component/identity/builtin_clients.go), so the choice is entirely ours.
  const listener: CallbackTransport = deps.isRemote === true
    ? (deps.awaitUriCallback ?? awaitUriCallbackImpl)({
        redirectUri: WELL_KNOWN_REDIRECT_URI_VSCODE,
        state,
        timeoutMs: deps.timeoutMs ?? DEFAULT_CALLBACK_TIMEOUT_MS,
        signal: deps.signal,
      })
    : await startListener({ timeoutMs: deps.timeoutMs, signal: deps.signal });
  try {
    const authorizeUrl = buildAuthorizeUrl({
      // The cluster's own answer, not a composed path.
      issuer: metadata.issuer,
      authorizationEndpoint: metadata.authorizationEndpoint,
      clientId,
      redirectUri: listener.redirectUri,
      state,
      codeChallenge: pkce.challenge,
      codeChallengeMethod: pkce.method,
    });

    await openInBrowser(authorizeUrl, deps);

    const callback = await listener.waitForCallback();

    if (callback.error !== undefined && callback.error !== "") {
      const detail = callback.errorDescription ?? "";
      throw new AuthFlowError(
        "authorizationDenied",
        detail === ""
          ? `The identity service refused the sign-in: ${callback.error}.`
          : `The identity service refused the sign-in: ${callback.error} -- ${detail}`,
      );
    }
    // Compared BEFORE the code is looked at, let alone sent. See the header.
    if (callback.state !== state) {
      throw new AuthFlowError(
        "stateMismatch",
        "The sign-in callback carried the wrong `state` value, so it did not come from the request this editor made. The authorization code was discarded without being redeemed.",
      );
    }
    const code = (callback.code ?? "").trim();
    if (code === "") {
      throw new AuthFlowError(
        "invalidCallback",
        "The sign-in callback carried neither an authorization code nor an error, so there is nothing to redeem.",
      );
    }

    const tokens = await exchangeAuthorizationCode({
      issuer,
      clientId,
      code,
      redirectUri: listener.redirectUri,
      codeVerifier: pkce.verifier,
      fetch: doFetch,
    });

    const nowSeconds = Math.floor(now() / 1000);
    return {
      ...tokens,
      expiresAtEpochSeconds:
        tokens.expiresInSeconds > 0 ? nowSeconds + tokens.expiresInSeconds : 0,
      clientId,
    };
  } finally {
    // Idempotent: on the success path the wait has already settled and this is
    // a no-op, but a throw anywhere above must not leave a port bound.
    listener.close();
  }
}

interface AuthorizeUrlParts {
  issuer: string;
  /**
   * The authorization endpoint AS THE CLUSTER PUBLISHES IT (memql#4624).
   *
   * Optional so the composed `${issuer}/authorize` remains the answer when a
   * caller has no metadata -- which keeps every existing test and the device
   * path unchanged. When discovery ran, this is what it returned, and taking it
   * is what lets an identity service that is not at `identity.<domain>`, or one
   * that mounts /authorize elsewhere, work at all.
   */
  authorizationEndpoint?: string;
  clientId: string;
  redirectUri: string;
  state: string;
  codeChallenge: string;
  codeChallengeMethod: string;
}

/** buildAuthorizeUrl composes the OAuth 2.1 code-flow authorization URL. */
export function buildAuthorizeUrl(parts: AuthorizeUrlParts): string {
  const endpoint = (parts.authorizationEndpoint ?? "").trim();
  const url = new URL(endpoint === "" ? `${parts.issuer}/authorize` : endpoint);
  url.searchParams.set("response_type", "code");
  url.searchParams.set("client_id", parts.clientId);
  // The PORT-BEARING redirect URI. identity's RFC 8252 matcher reconciles it
  // with the portless one that was registered; sending the portless form
  // instead would send the browser somewhere nothing is listening.
  url.searchParams.set("redirect_uri", parts.redirectUri);
  url.searchParams.set("state", parts.state);
  url.searchParams.set("code_challenge", parts.codeChallenge);
  url.searchParams.set("code_challenge_method", parts.codeChallengeMethod);
  return url.toString();
}

// openInBrowser hands the authorization URL to the editor's browser.
//
// Both failures here are `browserUnavailable` rather than `cancelled`, and the
// difference is load-bearing rather than cosmetic. Nobody declined anything: a
// host that cannot resolve an external URI or cannot launch a browser is a
// headless box, a container with no desktop session, an SSH session with
// nothing to hand the URL to. The device-code fallback (memql#3411) triggers on
// environment limitations and deliberately does NOT trigger on user
// cancellation, so calling this `cancelled` would make the fallback
// contractually obliged to ignore the one failure it exists to rescue.
async function openInBrowser(authorizeUrl: string, deps: AuthFlowDeps): Promise<void> {
  let external: string;
  try {
    external = await deps.resolveExternalUri(authorizeUrl);
  } catch (err) {
    throw new AuthFlowError(
      "browserUnavailable",
      `The sign-in URL could not be resolved into one this host can open (${errorText(err)}). This machine may have no browser available.`,
      { cause: err },
    );
  }
  // BOTH ways of saying no, not just the throw (memql#4618). env.openExternal
  // returns Thenable<boolean> and a host with nothing to launch typically
  // RESOLVES FALSE rather than rejecting -- the sibling file learned this
  // already (deviceCodeUi.ts, 98002a9f) and this one was left behind. Reading
  // only the rejection meant the flow believed a browser had opened and went
  // on to wait out the full callback deadline, so `browserUnavailable` never
  // fired, the device-code fallback never triggered, and the headless user the
  // fallback exists for sat through ten minutes before being told it timed out.
  let opened: boolean | void;
  try {
    opened = await deps.openExternal(external);
  } catch (err) {
    throw new AuthFlowError(
      "browserUnavailable",
      `A browser could not be opened for sign-in (${errorText(err)}). This machine may have no browser available.`,
      { cause: err },
    );
  }
  // `=== false` and nothing looser: a binding that resolves undefined (most of
  // them, and every test double) opened a browser perfectly well, and treating
  // that as a refusal would divert a working sign-in to a device code.
  if (opened === false) {
    throw new AuthFlowError(
      "browserUnavailable",
      "A browser could not be opened for sign-in: this host answered the request to open one with `false`. This machine may have no browser available.",
    );
  }
}

interface ExchangeParts {
  issuer: string;
  clientId: string;
  code: string;
  redirectUri: string;
  codeVerifier: string;
  fetch: FetchLike;
}

type ExchangedTokens = Pick<
  AuthFlowTokens,
  "accessToken" | "refreshToken" | "expiresInSeconds" | "scope"
>;

/**
 * exchangeAuthorizationCode redeems the code at `POST <issuer>/oauth/token`.
 *
 * JSON rather than form-encoded: identity accepts both
 * (component/identity/http/token.go, readTokenRequest) and JSON is what the
 * refresh exchange in src/connection/credentials.ts already sends, so both
 * halves of this extension's token traffic look the same in a capture.
 */
async function exchangeAuthorizationCode(parts: ExchangeParts): Promise<ExchangedTokens> {
  const url = `${parts.issuer}/oauth/token`;
  let response: HttpResponseLike;
  try {
    response = await parts.fetch(url, {
      method: "POST",
      headers: { "content-type": "application/json", accept: "application/json" },
      body: JSON.stringify({
        grant_type: "authorization_code",
        code: parts.code,
        client_id: parts.clientId,
        redirect_uri: parts.redirectUri,
        code_verifier: parts.codeVerifier,
      }),
    });
  } catch (err) {
    throw new AuthFlowError(
      "exchangeRejected",
      `The identity service at ${parts.issuer} could not be reached to redeem the sign-in (${errorText(err)}).`,
      { cause: err },
    );
  }

  const raw = await response.text().catch(() => "");
  if (!response.ok) {
    throw new AuthFlowError(
      "exchangeRejected",
      `${url} returned ${response.status}: ${oauthError(raw)}`,
    );
  }

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

  const accessToken = typeof parsed.access_token === "string" ? parsed.access_token.trim() : "";
  if (accessToken === "") {
    throw new AuthFlowError("exchangeRejected", `${url} returned no access_token.`);
  }
  const refreshToken = typeof parsed.refresh_token === "string" ? parsed.refresh_token.trim() : "";
  const expiresInSeconds =
    typeof parsed.expires_in === "number" && Number.isFinite(parsed.expires_in)
      ? Math.max(0, Math.floor(parsed.expires_in))
      : 0;
  const scope = typeof parsed.scope === "string" && parsed.scope.trim() !== "" ? parsed.scope.trim() : undefined;

  return { accessToken, refreshToken, expiresInSeconds, scope };
}
