// The browser sign-in flow: register -> authorize -> callback -> exchange.
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
import { AuthFlowError, errorText } from "./errors.js";
import {
  startLoopbackListener,
  type LoopbackListener,
  type LoopbackOptions,
} from "./loopback.js";
import { generatePkcePair, generateState } from "./pkce.js";
import { ensureClientId } from "./register.js";

/** Binds to `vscode.env.asExternalUri`. Takes and returns an absolute URL string. */
export type ExternalUriResolver = (url: string) => string | Promise<string>;

/** Binds to `vscode.env.openExternal`. The return value is ignored. */
export type ExternalOpener = (url: string) => unknown | Promise<unknown>;

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
  /** Overrides the client_name shown on the consent screen. */
  clientName?: string;
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
  /** The client_id this flow authorized with -- existing or freshly minted. */
  clientId: string;
  /** True when clientId was minted by this call and the caller must persist it. */
  clientIdWasRegistered: boolean;
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

  // Registration first: it is the only step that can fail without anything
  // being bound or opened, so failing here costs the operator nothing.
  const { clientId, registered } = await ensureClientId({
    issuer,
    clientId: cluster.clientId,
    clientName: deps.clientName,
    fetch: doFetch,
  });

  const pkce = generatePkcePair();
  const state = generateState();

  const listener = await startListener({ timeoutMs: deps.timeoutMs, signal: deps.signal });
  try {
    const authorizeUrl = buildAuthorizeUrl({
      issuer,
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
      clientIdWasRegistered: registered,
    };
  } finally {
    // Idempotent: on the success path the wait has already settled and this is
    // a no-op, but a throw anywhere above must not leave a port bound.
    listener.close();
  }
}

interface AuthorizeUrlParts {
  issuer: string;
  clientId: string;
  redirectUri: string;
  state: string;
  codeChallenge: string;
  codeChallengeMethod: string;
}

/** buildAuthorizeUrl composes the OAuth 2.1 code-flow authorization URL. */
export function buildAuthorizeUrl(parts: AuthorizeUrlParts): string {
  const url = new URL(`${parts.issuer}/authorize`);
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
  try {
    await deps.openExternal(external);
  } catch (err) {
    throw new AuthFlowError(
      "browserUnavailable",
      `A browser could not be opened for sign-in (${errorText(err)}). This machine may have no browser available.`,
      { cause: err },
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
