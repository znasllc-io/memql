// Deciding what bearer a dial actually carries, and keeping it alive.
//
// Two bugs live here, and both were silent in their own way.
//
// -----------------------------------------------------------------------------
// memql#3383 -- the TOKEN CLASS
// -----------------------------------------------------------------------------
//
// The credential field used to be called `pat` and documented as a Personal
// Access Token. A PAT cannot authenticate against a bff and never could: PAT
// verification is a DATABASE LOOKUP wired only into the identity binary
// (app/integrations_identity.go is `//go:build identity`), so on every other
// node `Verifier.verifyPAT` hits `v.pat == nil` and returns "verifier: PAT path
// not wired on this node" BEFORE looking anything up. A valid PAT therefore
// failed exactly like a forged one, with a bare handshake error and no hint
// that the token's CLASS was the problem. The mesh verifies bearers via JWKS,
// so an identity-issued JWT is the only class that can work.
//
// This module refuses a PAT-shaped value by name, without a round trip. It
// deliberately does NOT refuse an unrecognised string: refusing a PAT reports a
// policy the engine actually has, while refusing anything unfamiliar would be
// this module inventing one.
//
// -----------------------------------------------------------------------------
// memql#3385 -- the TOKEN LIFETIME
// -----------------------------------------------------------------------------
//
// identity issues 900-second access tokens (component/identity/config.go,
// DefaultAccessTokenTTLSeconds) and the extension held one forever, so a
// session died every fifteen minutes and the only recovery was hand-editing
// clusters.yaml. `POST <issuer>/oauth/token` with grant_type=refresh_token has
// always returned a fresh pair; this exchanges it.
//
// The exchange runs PROACTIVELY -- a token inside EXPIRY_SKEW_SECONDS of expiry
// is renewed before it is used -- so the operator never sees the broken dial
// that a refresh-on-failure design makes them pay once per token lifetime. The
// SDK's own in-place rotation hook (ConnectionAuth.onTokenExpired) is fed from
// the same place, so a LIVE stream re-auths without reconnecting at all.
//
// -----------------------------------------------------------------------------
// WHERE THE SECRETS LIVE, and why the two halves differ
// -----------------------------------------------------------------------------
//
// clusters.yaml is plaintext and SHARED with the memQL Cockpit, which owns the
// file. That ownership is exactly why the extension cannot simply move all
// credentials into VS Code's SecretStorage: the Cockpit would then read a
// cluster entry with no credential at all, and a registry two tools disagree
// about is worse than one they share. So the split follows the blast radius of
// each secret:
//
//   ACCESS TOKEN (`token:`)  -- stays in the shared file. It is a 15-minute
//     credential, it is what the Cockpit needs to see, and the file is where
//     an operator seeds it. A refreshed one is written straight back so the
//     Cockpit and the extension keep agreeing.
//
//   REFRESH TOKEN -- SecretStorage, keyed per cluster. This is a THIRTY-DAY
//     credential (DefaultRefreshTokenTTLSeconds = 2_592_000) that mints access
//     tokens on demand, which is precisely the thing the issue flagged as
//     unacceptable to leave lying in plaintext. `refresh_token:` in the file is
//     an INGEST path only: the resolver presents it once, and on the first
//     successful exchange it stores the ROTATED token in SecretStorage and
//     DELETES the plaintext key. If SecretStorage is unavailable the plaintext
//     copy is used and left alone -- clearing the only copy of a credential we
//     have nowhere to put would be worse than the exposure.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
// SecretStore is a structural interface that `vscode.SecretStorage` satisfies,
// so the editor's real store is injected by the adapter layer and the tests
// drive a Map.

import type { ClusterConfig } from "../clusters/model.js";
import { identityBaseUrlFor } from "./endpoint.js";

/** The prefix identity stamps on a Personal Access Token (component/identity/pat). */
export const PAT_PREFIX = "mql_pat_";
/** The prefix identity stamps on a worker token (component/identity/workertoken). */
export const WORKER_TOKEN_PREFIX = "mql_wkr_";

/**
 * How close to expiry a token may get before it is renewed rather than used.
 *
 * Two minutes out of a fifteen-minute lifetime: long enough to cover the dial
 * plus clock skew between this machine and the identity service, short enough
 * that it does not burn a refresh-token rotation on every connect.
 */
export const EXPIRY_SKEW_SECONDS = 120;

export type TokenClass = "empty" | "pat" | "workerToken" | "jwt" | "opaque";

/** classifyToken names what KIND of credential a stored string is. */
export function classifyToken(token: string | undefined): TokenClass {
  const value = (token ?? "").trim();
  if (value === "") return "empty";
  if (value.startsWith(PAT_PREFIX)) return "pat";
  if (value.startsWith(WORKER_TOKEN_PREFIX)) return "workerToken";
  return jwtExpirySeconds(value) !== undefined || value.split(".").length === 3 ? "jwt" : "opaque";
}

/**
 * jwtExpirySeconds reads the `exp` claim, in epoch seconds.
 *
 * READS, not verifies. Verification needs the identity service's JWKS and is
 * the server's job -- this is only ever used to decide whether to renew before
 * dialing, and being wrong costs one unnecessary refresh. Returns undefined for
 * anything that is not a decodable three-segment JWT carrying a numeric `exp`;
 * a missing `exp` is NOT an expiry of zero.
 */
export function jwtExpirySeconds(token: string): number | undefined {
  const parts = token.trim().split(".");
  if (parts.length !== 3) return undefined;
  const payload = parts[1];
  if (payload === undefined || payload === "") return undefined;
  try {
    const decoded = Buffer.from(payload, "base64url").toString("utf8");
    // Buffer.from is lenient about non-base64 input (it skips what it cannot
    // decode), so the JSON parse below is the real gate.
    const claims = JSON.parse(decoded) as Record<string, unknown>;
    const exp = claims.exp;
    return typeof exp === "number" && Number.isFinite(exp) ? exp : undefined;
  } catch {
    return undefined;
  }
}

/** refreshTokenSecretKey is the per-cluster SecretStorage key. */
export function refreshTokenSecretKey(clusterName: string): string {
  return `memql.cluster.refreshToken:${clusterName}`;
}

/**
 * The subset of `vscode.SecretStorage` this module needs.
 *
 * PromiseLike rather than Promise so VS Code's Thenable-returning API satisfies
 * it without a wrapper, and so this file stays importable from plain Node.
 */
export interface SecretStore {
  get(key: string): PromiseLike<string | undefined>;
  store(key: string, value: string): PromiseLike<void>;
  delete(key: string): PromiseLike<void>;
}

export interface HttpRequestInit {
  method: string;
  headers: Record<string, string>;
  body: string;
}

export interface HttpResponseLike {
  ok: boolean;
  status: number;
  text(): Promise<string>;
}

export type FetchLike = (url: string, init: HttpRequestInit) => Promise<HttpResponseLike>;

/** What a refresh writes back to the shared registry. */
export interface PersistedCredential {
  /** The freshly minted access token. */
  token: string;
  /**
   * Whether the plaintext `refresh_token:` key may now be deleted from the
   * file. True only when SecretStorage actually took custody of the rotated
   * token -- otherwise the file holds the only copy.
   */
  clearStoredRefreshToken: boolean;
}

export type PersistFn = (
  clusterName: string,
  update: PersistedCredential,
) => Promise<void>;

export type CredentialFailureReason =
  | "notConfigured"
  | "missingCredential"
  | "wrongTokenClass"
  | "credentialExpired";

export type CredentialResolution =
  | { ok: true; bearer: string }
  | { ok: false; reason: CredentialFailureReason; message: string };

/** The seam ConnectionManager depends on. */
export interface CredentialSource {
  resolve(cluster: ClusterConfig): Promise<CredentialResolution>;
  /**
   * forceRefresh feeds the SDK's in-place re-auth hook
   * (ConnectionAuth.onTokenExpired): a fresh bearer, or null to give up. It
   * ignores the stored access token's expiry entirely -- the caller has already
   * decided the current one is spent.
   */
  forceRefresh(cluster: ClusterConfig): Promise<string | null>;
}

export interface CredentialDeps {
  secrets?: SecretStore;
  fetch?: FetchLike;
  persist?: PersistFn;
  now?: () => number;
}

// -----------------------------------------------------------------------------
// Messages
//
// Exported so the Clusters tree can say the same thing at rest that the
// connection attempt says on failure. An operator who reads two different
// explanations of one condition learns less than one who reads the same
// sentence twice.
// -----------------------------------------------------------------------------

export function wrongTokenClassMessage(clusterName: string, tokenClass: TokenClass): string {
  const what =
    tokenClass === "workerToken"
      ? "a worker token (mql_wkr_...)"
      : "a Personal Access Token (mql_pat_...)";
  return (
    `The credential stored for cluster "${clusterName}" is ${what}, which cannot authenticate against a bff. ` +
    "Mesh nodes verify bearers against the identity service's JWKS feed and have no lookup path for these tokens, " +
    "so they are rejected before any check on the value itself. " +
    "Store an identity-issued JWT access token in the `token` field of clusters.yaml instead."
  );
}

export function missingCredentialMessage(clusterName: string): string {
  return (
    `Cluster "${clusterName}" has no credential. Set \`token\` in clusters.yaml to an identity-issued ` +
    "JWT access token (the `access_token` from POST <identity>/oauth/token), and set `refresh_token` " +
    "alongside it so the extension can renew it as it expires."
  );
}

export function notConfiguredMessage(clusterName: string): string {
  return (
    `Cluster "${clusterName}" is not configured. Set an endpoint, and a \`token\` holding an ` +
    "identity-issued JWT access token."
  );
}

function expiredMessage(clusterName: string, detail: string): string {
  return (
    `The access token for cluster "${clusterName}" has expired and could not be renewed: ${detail} ` +
    "Store a fresh `token` (and `refresh_token`) in clusters.yaml."
  );
}

// -----------------------------------------------------------------------------

const NO_REFRESH_TOKEN =
  "no refresh token is stored for this cluster, so there is nothing to exchange.";

interface RefreshOutcome {
  ok: boolean;
  accessToken?: string;
  error?: string;
}

export class CredentialResolver implements CredentialSource {
  private readonly secrets: SecretStore | undefined;
  private readonly fetch: FetchLike;
  private readonly persist: PersistFn | undefined;
  private readonly now: () => number;

  constructor(deps: CredentialDeps = {}) {
    this.secrets = deps.secrets;
    this.fetch = deps.fetch ?? defaultFetch;
    this.persist = deps.persist;
    this.now = deps.now ?? (() => Date.now());
  }

  async resolve(cluster: ClusterConfig): Promise<CredentialResolution> {
    if (cluster.endpoint.trim() === "") {
      return { ok: false, reason: "notConfigured", message: notConfiguredMessage(cluster.name) };
    }

    const token = (cluster.token ?? "").trim();
    const tokenClass = classifyToken(token);
    if (tokenClass === "pat" || tokenClass === "workerToken") {
      // Refused BEFORE the dial. Letting this through buys a handshake failure
      // that names nothing -- which is the entire complaint in memql#3383.
      return {
        ok: false,
        reason: "wrongTokenClass",
        message: wrongTokenClassMessage(cluster.name, tokenClass),
      };
    }

    const expiry = jwtExpirySeconds(token);
    const nowSeconds = this.now() / 1000;
    const expired = expiry !== undefined && expiry <= nowSeconds;
    const stale = token === "" || (expiry !== undefined && expiry - nowSeconds <= EXPIRY_SKEW_SECONDS);

    if (!stale) return { ok: true, bearer: token };

    const refreshed = await this.exchange(cluster);
    if (refreshed.ok && refreshed.accessToken !== undefined) {
      return { ok: true, bearer: refreshed.accessToken };
    }

    // The refresh failed. Whether that matters depends on what we still hold:
    // a token merely NEARING expiry is still a working credential, and losing a
    // usable connection because the identity service was briefly unreachable
    // would be a regression rather than a fix. A token already past `exp` is
    // not, and neither is no token at all.
    const detail = refreshed.error ?? "the exchange failed.";
    if (token === "") {
      // Nothing stored and nothing to exchange is a cluster nobody has given a
      // credential to -- a different sentence from one whose credential ran out.
      return refreshed.error === NO_REFRESH_TOKEN
        ? { ok: false, reason: "missingCredential", message: missingCredentialMessage(cluster.name) }
        : { ok: false, reason: "credentialExpired", message: expiredMessage(cluster.name, detail) };
    }
    if (expired) {
      return { ok: false, reason: "credentialExpired", message: expiredMessage(cluster.name, detail) };
    }
    return { ok: true, bearer: token };
  }

  async forceRefresh(cluster: ClusterConfig): Promise<string | null> {
    const refreshed = await this.exchange(cluster);
    return refreshed.ok && refreshed.accessToken !== undefined ? refreshed.accessToken : null;
  }

  // exchange runs the refresh_token grant and takes custody of the rotated
  // token. Every failure is returned as a sentence, never thrown: the caller's
  // decision (fail, or carry on with a still-valid token) depends on it.
  private async exchange(cluster: ClusterConfig): Promise<RefreshOutcome> {
    const refreshToken = await this.loadRefreshToken(cluster);
    if (refreshToken === undefined) return { ok: false, error: NO_REFRESH_TOKEN };

    const base = identityBaseUrlFor(cluster);
    if (base === undefined) {
      return {
        ok: false,
        error:
          "no identity service URL is known for this cluster -- set `issuer` (or `domain`) in clusters.yaml.",
      };
    }

    const body: Record<string, string> = {
      grant_type: "refresh_token",
      refresh_token: refreshToken,
    };
    const clientId = (cluster.clientId ?? "").trim();
    if (clientId !== "") body.client_id = clientId;

    let response: HttpResponseLike;
    try {
      response = await this.fetch(`${base}/oauth/token`, {
        method: "POST",
        headers: { "content-type": "application/json", accept: "application/json" },
        body: JSON.stringify(body),
      });
    } catch (err) {
      return {
        ok: false,
        error: `the identity service at ${base} could not be reached (${errorText(err)}).`,
      };
    }

    const raw = await response.text().catch(() => "");
    if (!response.ok) {
      return { ok: false, error: `${base}/oauth/token returned ${response.status}: ${oauthError(raw)}` };
    }

    let parsed: { access_token?: unknown; refresh_token?: unknown };
    try {
      parsed = JSON.parse(raw) as typeof parsed;
    } catch {
      return { ok: false, error: `${base}/oauth/token returned a body that is not JSON.` };
    }
    const accessToken = typeof parsed.access_token === "string" ? parsed.access_token.trim() : "";
    if (accessToken === "") {
      return { ok: false, error: `${base}/oauth/token returned no access_token.` };
    }

    // identity ROTATES the refresh token on every exchange
    // (component/identity/refresh), so keeping the presented one would make the
    // next refresh fail with invalid_grant and drop the operator right back
    // into hand-editing the file.
    const rotated =
      typeof parsed.refresh_token === "string" && parsed.refresh_token.trim() !== ""
        ? parsed.refresh_token.trim()
        : refreshToken;

    let custodyTaken = false;
    if (this.secrets !== undefined) {
      try {
        await this.secrets.store(refreshTokenSecretKey(cluster.name), rotated);
        custodyTaken = true;
      } catch {
        // A SecretStorage that refuses a write must not cost us the connection;
        // it only means the plaintext copy stays where it is.
      }
    }

    if (this.persist !== undefined) {
      try {
        await this.persist(cluster.name, {
          token: accessToken,
          clearStoredRefreshToken: custodyTaken,
        });
      } catch {
        // The dial can still proceed on the in-memory token; the next connect
        // simply refreshes again.
      }
    }

    return { ok: true, accessToken };
  }

  // loadRefreshToken prefers SecretStorage and falls back to the file's ingest
  // key. Custody is taken on a successful exchange (see `exchange`), not here:
  // copying the secret before we know it works would leave two live copies if
  // the exchange then failed.
  private async loadRefreshToken(cluster: ClusterConfig): Promise<string | undefined> {
    if (this.secrets !== undefined) {
      try {
        const stored = (await this.secrets.get(refreshTokenSecretKey(cluster.name)))?.trim();
        if (stored !== undefined && stored !== "") return stored;
      } catch {
        // Fall through to the file.
      }
    }
    const plain = (cluster.refreshToken ?? "").trim();
    return plain === "" ? undefined : plain;
  }
}

/**
 * A resolver with no refresh capability: it classifies and checks expiry, and
 * that is all. The default so that a ConnectionManager built without deps still
 * refuses a PAT by name rather than dialing with it.
 */
export const staticCredentials: CredentialSource = new CredentialResolver();

/**
 * The real network. Exported so the sign-in flow (src/auth/) reaches the SAME
 * identity service through the same seam rather than growing a second one --
 * both talk to `POST <issuer>/oauth/token`, and a test that stubs one should
 * not be able to miss the other.
 */
export function defaultFetch(url: string, init: HttpRequestInit): Promise<HttpResponseLike> {
  return fetch(url, init) as unknown as Promise<HttpResponseLike>;
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// oauthError turns a failure body into the sentence an operator can act on.
//
// It reads `message` AS WELL AS RFC 6749 §5.2's `error_description`, because
// the identity service emits the former. Verified against a live cluster:
//
//   POST https://identity.local.znas.io/oauth/token
//   {"error":"invalid_grant","errorId":"ERR-1845ef",
//    "message":"refresh token is no longer valid"}
//
// (component/identity/http/magic_link.go, writeJSONError -- error / message /
// errorId). Reading only the RFC spelling would have reduced that to the bare
// code "invalid_grant" and thrown away the only human-readable half.
//
// The errorId is carried through when present: it is the correlation id in the
// identity service's own logs, which is exactly what an operator needs to hand
// over when the message alone is not enough.
//
// Exported for src/auth/, whose /register and authorization_code failures come
// back through the very same writeJSONError shape.
export function oauthError(raw: string): string {
  try {
    const parsed = JSON.parse(raw) as {
      error?: unknown;
      error_description?: unknown;
      message?: unknown;
      errorId?: unknown;
    };
    const description = firstString(parsed.error_description, parsed.message);
    const code = firstString(parsed.error);
    const errorId = firstString(parsed.errorId);
    const head =
      description !== "" && code !== ""
        ? `${code} -- ${description}`
        : description !== ""
          ? description
          : code;
    if (head !== "") return errorId === "" ? head : `${head} (${errorId})`;
  } catch {
    // Not JSON; fall through.
  }
  return raw.trim() === "" ? "no detail" : raw.trim();
}

function firstString(...candidates: unknown[]): string {
  for (const candidate of candidates) {
    if (typeof candidate === "string" && candidate.trim() !== "") return candidate.trim();
  }
  return "";
}
