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
// asExternalUri is not decoration on a LOCAL host: it is what lets a URL this
// process composes be opened by the browser the editor can reach.
//
// IT DOES NOT MAKE THIS FLOW WORK UNDER REMOTE-SSH, and this comment used to
// say that it did (memql#4623). The loopback listener binds on the EXTENSION
// HOST -- the remote machine -- and that port goes into the redirect URI.
// asExternalUri is then applied to the AUTHORIZE URL, which is an
// `https://identity...` URL and comes back unchanged, so no tunnel is ever
// created for the callback port. The browser opens on the user's own machine
// and redirects to THEIR 127.0.0.1:PORT, where nothing is listening. Neither
// fallback trigger fires -- the bind succeeded and the browser opened -- so the
// result was a ten-minute spinner followed by advice to run a palette command.
//
// A remote host is therefore refused BEFORE the listener binds, as
// `browserUnavailable`, which is the trigger that routes it to the device-code
// flow. That flow needs no callback at all, which is why it is the right answer
// rather than a consolation: the user reads a code and approves it in whatever
// browser they have.
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
import { discoverIssuer } from "../connection/discovery.js";
import { identityBaseUrlFor } from "../connection/endpoint.js";
import { AuthFlowError, errorText } from "./errors.js";
import {
  startLoopbackListener,
  type LoopbackListener,
  type LoopbackOptions,
} from "./loopback.js";
import { generatePkcePair, generateState } from "./pkce.js";
import { resolveClientId } from "./wellKnownClient.js";

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
   * `vscode.env.remoteName` -- undefined locally, otherwise the remote's kind
   * ("ssh-remote", "dev-container", "codespaces", "wsl"), memql#4623.
   *
   * Injected rather than read, for this module's no-`vscode` rule. Undefined is
   * the local case and is what every existing caller and test supplies.
   */
  remoteName?: string;
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

  // A REMOTE EXTENSION HOST CANNOT RECEIVE THIS CALLBACK (memql#4623).
  //
  // Refused here, before the listener binds and before a browser is opened, so
  // that it lands as `browserUnavailable` -- the kind that routes to the
  // device-code flow, which needs no loopback at all. Both fallback triggers
  // fire before any page could have opened, and this one keeps that invariant:
  // switching cannot orphan a live sign-in because nothing has been opened.
  //
  // The alternative -- registering a `vscode://` redirect URI and receiving the
  // callback through the extension's own URI handler -- is a larger change that
  // needs the redirect registered on the identity server too. It is worth doing
  // and it is not what this is; what this removes is a ten-minute wait that
  // ended by blaming the browser.
  const remote = (deps.remoteName ?? "").trim();
  if (remote !== "") {
    throw new AuthFlowError(
      "browserUnavailable",
      `A browser sign-in cannot complete from a ${remote} window: the sign-in callback would be ` +
        `sent to this editor's own machine, not to the remote host this extension is running on. ` +
        `Signing in with a device code instead -- it needs no callback.`,
    );
  }

  const doFetch = deps.fetch ?? defaultFetch;
  const now = deps.now ?? (() => Date.now());
  const startListener = deps.startListener ?? startLoopbackListener;

  // A local constant lookup, not a network call: identity carries this client
  // compiled in (wellKnownClient.ts). The step that used to be the first thing
  // to fail -- POST /register against a cluster with DCR off -- is gone.
  const clientId = resolveClientId(cluster.clientId);

  // THE PRE-FLIGHT (memql#4624). One round trip, before a browser is opened
  // and before anything parks on a 600-second deadline.
  //
  // Without it, a wrong domain, an unreachable host, a bad certificate, an old
  // cluster and a plain non-MemQL host were indistinguishable: all five cost
  // the full deadline and were then reported as "the browser page was never
  // completed, or it could not reach 127.0.0.1", which is wrong in every one
  // of them. The worst is the old cluster -- one predating the `memql-vscode`
  // built-in client renders an HTML 400 "Unknown client" page that is never
  // redirected, so there is no callback and no OAuth error envelope, just
  // silence for ten minutes.
  //
  // The device path has always failed in one round trip with the real reason
  // (deviceCode.ts). This gives the browser path the same.
  //
  // THE ENDPOINTS ARE TAKEN FROM THE ANSWER, not composed. That is what makes
  // an identity service at a non-conventional host work at all, and it is what
  // lets an operator who pasted the API host be redirected to the real issuer
  // rather than dead-ended.
  //
  // IT FAILS FAST ON "NOBODY ANSWERED" AND DEGRADES ON EVERYTHING ELSE, and
  // the split is the whole care in this change. A host that does not answer
  // cannot complete a sign-in by any route, so stopping here costs nothing and
  // saves ten minutes. But a host that answers WITHOUT an RFC 8414 document is
  // ambiguous: it is either the wrong host, or a cluster old enough to predate
  // the document. Refusing would make this extension unable to sign in to a
  // cluster it can sign in to today -- a regression traded for a diagnosis --
  // so that case carries on with the conventional endpoints, exactly as before
  // this existed. The wrong-host paste is caught earlier and more cheaply, by
  // connectDomainProblem at the moment it is typed.
  const discovered = await discoverIssuer(issuer, (url, init) => doFetch(url, init));
  if (!discovered.ok && discovered.kind === "unreachable") {
    throw new AuthFlowError(
      // This vocabulary's "the identity service could not be reached" --
      // deviceCode.ts uses it for exactly that.
      "registrationFailed",
      `Cannot sign in to "${cluster.name}": ${discovered.message}.`,
    );
  }
  // Everything downstream speaks in terms of an issuer base, and the endpoints
  // are the ones the document names -- so a cluster that publishes a
  // non-default host or path is honoured rather than overwritten. A cluster
  // that published nothing readable keeps the convention.
  const resolvedIssuer = discovered.ok ? discovered.issuer.replace(/\/+$/, "") : issuer;
  const authorizationEndpoint = discovered.ok ? discovered.authorizationEndpoint : undefined;
  const tokenEndpoint = discovered.ok ? discovered.tokenEndpoint : undefined;

  const pkce = generatePkcePair();
  const state = generateState();

  const listener = await startListener({ timeoutMs: deps.timeoutMs, signal: deps.signal });
  try {
    const authorizeUrl = buildAuthorizeUrl({
      issuer: resolvedIssuer,
      authorizationEndpoint,
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
      issuer: resolvedIssuer,
      tokenEndpoint,
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
  /** From the cluster's RFC 8414 document. Absent falls back to `${issuer}/authorize`. */
  authorizationEndpoint?: string;
  clientId: string;
  redirectUri: string;
  state: string;
  codeChallenge: string;
  codeChallengeMethod: string;
}

/** buildAuthorizeUrl composes the OAuth 2.1 code-flow authorization URL. */
export function buildAuthorizeUrl(parts: AuthorizeUrlParts): string {
  // THE DISCOVERED ENDPOINT WINS (memql#4624). Composing `${issuer}/authorize`
  // is the fallback for a caller that did no discovery; a cluster that
  // publishes a different path in its RFC 8414 document means it, and
  // overwriting that with a convention is how a conformant deployment becomes
  // unreachable.
  const url = new URL(
    parts.authorizationEndpoint !== undefined && parts.authorizationEndpoint.trim() !== ""
      ? parts.authorizationEndpoint.trim()
      : `${parts.issuer}/authorize`,
  );
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
  /** From the cluster's RFC 8414 document. Absent falls back to `${issuer}/oauth/token`. */
  tokenEndpoint?: string;
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
  // Same rule as buildAuthorizeUrl: the published endpoint wins (memql#4624).
  const url =
    parts.tokenEndpoint !== undefined && parts.tokenEndpoint.trim() !== ""
      ? parts.tokenEndpoint.trim()
      : `${parts.issuer}/oauth/token`;
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
