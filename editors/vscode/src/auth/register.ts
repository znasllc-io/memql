// RFC 7591 dynamic client registration.
//
// -----------------------------------------------------------------------------
// WHY THE REGISTERED REDIRECT URI HAS NO PORT
// -----------------------------------------------------------------------------
//
// The loopback listener takes whatever ephemeral port the kernel hands it
// (loopback.ts), so the redirect_uri the browser actually comes back to is
// `http://127.0.0.1:54321/callback` -- a different number every sign-in. A
// registration that pinned one port would therefore be wrong on the second
// attempt, and registering a range is not a thing RFC 7591 offers.
//
// RFC 8252 §7.3 exists for precisely this, and identity implements it: a
// registered loopback redirect URI WITH NO EXPLICIT PORT matches an incoming
// URI on any port as long as the scheme, host and path agree
// (component/identity/config.go, matchesLoopbackAnyPort -- `r.Port() != ""`
// returns false, i.e. a registered URI that carries a port opts OUT of the
// exception and goes back to exact-match). So the value below is deliberately
// portless, and adding a port to it would silently break every callback.
//
// -----------------------------------------------------------------------------
// WHY THIS RUNS AT MOST ONCE PER CLUSTER
// -----------------------------------------------------------------------------
//
// Each POST mints a new client_id and persists a row
// (component/identity/http/register.go -> Store.CreateOAuthClient). Registering
// on every sign-in would leave a trail of orphaned clients that nothing ever
// revokes, so ensureClientId() only calls out when the cluster carries no
// client_id -- the caller persists the returned one into
// ClusterConfig.clientId (#3404) and no later sign-in registers again.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import {
  defaultFetch,
  oauthError,
  type FetchLike,
  type HttpResponseLike,
} from "../connection/credentials.js";
import { AuthFlowError, errorText } from "./errors.js";
import { CALLBACK_PATH, LOOPBACK_HOST } from "./loopback.js";

/**
 * The redirect URI this extension registers. PORTLESS on purpose -- see the
 * header. The path must equal loopback.ts's CALLBACK_PATH, because the RFC 8252
 * matcher compares paths exactly.
 */
export const REGISTERED_REDIRECT_URI = `http://${LOOPBACK_HOST}${CALLBACK_PATH}`;

/** What the consent screen shows as the name of the app asking for access. */
export const DEFAULT_CLIENT_NAME = "memQL for VS Code";

export interface RegisterOptions {
  /** The identity service base URL -- where /register, /authorize and /oauth/token live. */
  issuer: string;
  /** Shown to the user on the consent screen. Defaults to DEFAULT_CLIENT_NAME. */
  clientName?: string;
  /** Injected for tests. Defaults to the real network. */
  fetch?: FetchLike;
}

export interface EnsureClientIdOptions extends RegisterOptions {
  /** The cluster's stored client_id. When non-empty, NOTHING is registered. */
  clientId?: string;
}

export interface EnsureClientIdResult {
  clientId: string;
  /** True when this call minted the id, i.e. the caller must persist it. */
  registered: boolean;
}

/**
 * registerPublicClient self-registers as an RFC 7591 PUBLIC client and returns
 * the minted client_id.
 *
 * Public because this extension ships to every user's machine: a client_secret
 * baked into it would be a secret in name only, and identity refuses anything
 * but `token_endpoint_auth_method: "none"` for exactly that reason
 * (component/identity/http/register.go). PKCE, not a secret, is what binds the
 * authorization code to the process that asked for it.
 */
export async function registerPublicClient(options: RegisterOptions): Promise<string> {
  const issuer = normalizeIssuer(options.issuer);
  if (issuer === "") {
    throw new AuthFlowError(
      "misconfigured",
      "No identity service URL is known for this cluster, so there is nowhere to register -- set `issuer` (or `domain`) in clusters.yaml.",
    );
  }
  const doFetch = options.fetch ?? defaultFetch;
  const url = `${issuer}/register`;
  const body = {
    client_name: options.clientName?.trim() || DEFAULT_CLIENT_NAME,
    redirect_uris: [REGISTERED_REDIRECT_URI],
    grant_types: ["authorization_code", "refresh_token"],
    response_types: ["code"],
    token_endpoint_auth_method: "none",
  };

  let response: HttpResponseLike;
  try {
    response = await doFetch(url, {
      method: "POST",
      headers: { "content-type": "application/json", accept: "application/json" },
      body: JSON.stringify(body),
    });
  } catch (err) {
    throw new AuthFlowError(
      "registrationFailed",
      `The identity service at ${issuer} could not be reached to register this editor as an OAuth client (${errorText(err)}).`,
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

  let parsed: { client_id?: unknown };
  try {
    parsed = JSON.parse(raw) as typeof parsed;
  } catch {
    throw new AuthFlowError("registrationFailed", `${url} returned a body that is not JSON.`);
  }
  const clientId = typeof parsed.client_id === "string" ? parsed.client_id.trim() : "";
  if (clientId === "") {
    throw new AuthFlowError("registrationFailed", `${url} returned no client_id.`);
  }
  return clientId;
}

/**
 * ensureClientId returns the client_id to authorize with, registering one only
 * if the cluster has none.
 *
 * `registered` tells the caller whether the value is new and therefore needs
 * persisting -- the flow does not write clusters.yaml itself (#3404 owns that).
 */
export async function ensureClientId(
  options: EnsureClientIdOptions,
): Promise<EnsureClientIdResult> {
  const existing = (options.clientId ?? "").trim();
  if (existing !== "") return { clientId: existing, registered: false };
  return { clientId: await registerPublicClient(options), registered: true };
}

/** normalizeIssuer trims whitespace and any trailing slashes, matching identityBaseUrlFor. */
export function normalizeIssuer(issuer: string | undefined): string {
  return (issuer ?? "").trim().replace(/\/+$/, "");
}
