// Where a signed-in cluster's tokens live, and how they are taken away again.
//
// -----------------------------------------------------------------------------
// THE SPLIT, AND WHY IT IS NOT THE ONE memql#3404 ASKED FOR
// -----------------------------------------------------------------------------
//
// The issue's acceptance criteria say the ACCESS token goes into SecretStorage
// too, and that no token material is ever written to clusters.yaml. memql#3385
// landed first and decided otherwise for that one half, deliberately:
// clusters.yaml is OWNED and SHARED with the memQL Cockpit, so a cluster entry
// the extension has signed in to but which carries no credential at all reads,
// to the Cockpit, as a cluster nobody has signed in to. A registry two tools
// disagree about is worse than one they share. So the split follows the blast
// radius of each secret rather than the credential's name:
//
//   ACCESS TOKEN   -- clusters.yaml `token:`. Fifteen minutes
//                     (component/identity/config.go,
//                     DefaultAccessTokenTTLSeconds), and it is what the
//                     Cockpit reads.
//   REFRESH TOKEN  -- SecretStorage, keyed per cluster. THIRTY DAYS, and it
//                     mints access tokens on demand. This is the credential
//                     the issue is actually about, and it never touches the
//                     shared file except as an ingest path
//                     (src/connection/credentials.ts takes custody on the
//                     first successful exchange and deletes the plaintext).
//   EXPIRY         -- SecretStorage, beside the refresh token. Redundant for
//                     the JWT the identity service issues today, since `exp`
//                     is inside it; it exists so an OPAQUE access token can
//                     still be renewed proactively rather than only after it
//                     has already failed a dial.
//
// -----------------------------------------------------------------------------
// WHY THERE IS AN INDEX
// -----------------------------------------------------------------------------
//
// SecretStorage cannot be enumerated -- there is `get`, `store` and `delete`,
// and no way to ask what keys exist. So a cluster renamed or deleted in
// clusters.yaml (by this extension, by the Cockpit, or by hand) would leave a
// thirty-day credential behind under a key nothing would ever look at again,
// and nothing could find it to clean it up.
//
// `credentialIndexKey` is that missing enumeration: every write records the
// cluster name, every delete removes it, and `reconcile` sweeps the difference
// between the index and the clusters that actually exist. A rename is handled
// as a MOVE rather than left to the sweep, because being swept would silently
// sign the operator out of a cluster they only renamed.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
// SecretStore is a structural interface `vscode.SecretStorage` satisfies, so
// the editor's real store is injected by the adapter layer and tests drive a
// Map.

import type { ClusterUpdate } from "../clusters/file.js";
import type { ClusterConfig } from "../clusters/model.js";
import type {
  CredentialFailureReason,
  CredentialSource,
} from "../connection/credentials.js";

/**
 * The subset of `vscode.SecretStorage` this extension needs.
 *
 * PromiseLike rather than Promise so VS Code's Thenable-returning API satisfies
 * it without a wrapper, and so this file stays importable from plain Node.
 */
export interface SecretStore {
  get(key: string): PromiseLike<string | undefined>;
  store(key: string, value: string): PromiseLike<void>;
  delete(key: string): PromiseLike<void>;
}

/** refreshTokenSecretKey is the per-cluster SecretStorage key. */
export function refreshTokenSecretKey(clusterName: string): string {
  return `memql.cluster.refreshToken:${clusterName}`;
}

/** accessTokenExpirySecretKey holds the access token's absolute expiry, in epoch seconds. */
export function accessTokenExpirySecretKey(clusterName: string): string {
  return `memql.cluster.tokenExpiry:${clusterName}`;
}

/** credentialIndexKey holds the JSON array of cluster names this store has written for. */
export const credentialIndexKey = "memql.cluster.credentialIndex";

/**
 * A partial write against one entry of clusters.yaml.
 *
 * Binds to `upsertCluster(clustersPath, update)`. The per-field semantics are
 * that function's: `undefined` leaves the on-disk value alone and `""` DELETES
 * the key, which is what makes signing out actually remove the credential
 * rather than only appear to.
 */
export type ClusterWriter = (update: ClusterUpdate) => Promise<void>;

export interface CredentialStoreDeps {
  /** Absent in a host with no SecretStorage; every secret operation then degrades, see below. */
  secrets?: SecretStore;
  writeCluster: ClusterWriter;
}

/** The material a completed sign-in produces. `AuthFlowTokens` satisfies it structurally. */
export interface SignInTokens {
  accessToken: string;
  refreshToken: string;
  /** Absolute expiry in epoch seconds. 0 when the server reported no lifetime. */
  expiresAtEpochSeconds: number;
  clientId: string;
}

/**
 * ClusterCredentialStore is the secret half of a cluster's credentials.
 *
 * EVERY METHOD SWALLOWS SecretStorage FAILURES. On Linux the backing keyring
 * can be absent or locked, and a store that throws there would turn "your
 * keyring is locked" into "you cannot connect" -- so a failed write is reported
 * as custody-not-taken (the caller then leaves the plaintext copy where it is,
 * because destroying the only copy of a credential we have nowhere to put is
 * strictly worse than the exposure) and a failed read is reported as nothing
 * stored.
 */
export class ClusterCredentialStore {
  constructor(private readonly secrets?: SecretStore) {}

  /** available reports whether there is anywhere to put a secret at all. */
  get available(): boolean {
    return this.secrets !== undefined;
  }

  async readRefreshToken(clusterName: string): Promise<string | undefined> {
    const value = await this.read(refreshTokenSecretKey(clusterName));
    return value === "" ? undefined : value;
  }

  /** writeRefreshToken returns true only when SecretStorage actually took custody. */
  async writeRefreshToken(clusterName: string, refreshToken: string): Promise<boolean> {
    if (this.secrets === undefined || refreshToken.trim() === "") return false;
    const stored = await this.write(refreshTokenSecretKey(clusterName), refreshToken.trim());
    if (stored) await this.index(clusterName);
    return stored;
  }

  /**
   * readExpiry returns the stored absolute expiry in epoch seconds.
   *
   * Only consulted for a credential whose own `exp` cannot be read -- an
   * opaque access token. A stored value that is not a finite number is treated
   * as nothing stored rather than as an expiry of zero, which would renew a
   * perfectly good token on every connect.
   */
  async readExpiry(clusterName: string): Promise<number | undefined> {
    const raw = await this.read(accessTokenExpirySecretKey(clusterName));
    if (raw === undefined || raw === "") return undefined;
    const parsed = Number(raw);
    return Number.isFinite(parsed) ? parsed : undefined;
  }

  async writeExpiry(clusterName: string, expiresAtEpochSeconds: number): Promise<void> {
    if (this.secrets === undefined) return;
    if (!Number.isFinite(expiresAtEpochSeconds) || expiresAtEpochSeconds <= 0) return;
    const stored = await this.write(
      accessTokenExpirySecretKey(clusterName),
      String(Math.floor(expiresAtEpochSeconds)),
    );
    if (stored) await this.index(clusterName);
  }

  /** clear removes every secret held for one cluster, and de-indexes it. */
  async clear(clusterName: string): Promise<void> {
    if (this.secrets === undefined) return;
    await this.remove(refreshTokenSecretKey(clusterName));
    await this.remove(accessTokenExpirySecretKey(clusterName));
    await this.deindex(clusterName);
  }

  /**
   * rename MOVES a cluster's secrets to a new name.
   *
   * A rename is not a sign-out: the operator renamed a cluster, they did not
   * revoke anything. Leaving the secrets under the old key would both orphan a
   * thirty-day credential and force a fresh browser sign-in for a cluster that
   * is already authorized.
   */
  async rename(previousName: string, newName: string): Promise<void> {
    if (this.secrets === undefined) return;
    if (previousName === newName) return;
    const refreshToken = await this.readRefreshToken(previousName);
    const expiry = await this.readExpiry(previousName);
    if (refreshToken !== undefined) await this.writeRefreshToken(newName, refreshToken);
    if (expiry !== undefined) await this.writeExpiry(newName, expiry);
    await this.clear(previousName);
  }

  /**
   * reconcile deletes the secrets of every indexed cluster that no longer
   * exists, and returns the names swept.
   *
   * This is the catch-all for a deletion this extension never saw: the Cockpit
   * writes clusters.yaml too, and an operator can edit it by hand. Call it
   * whenever the file has been re-read.
   */
  async reconcile(liveClusterNames: readonly string[]): Promise<string[]> {
    if (this.secrets === undefined) return [];
    const live = new Set(liveClusterNames);
    const indexed = await this.readIndex();
    const orphans = indexed.filter((name) => !live.has(name));
    for (const name of orphans) {
      await this.remove(refreshTokenSecretKey(name));
      await this.remove(accessTokenExpirySecretKey(name));
    }
    if (orphans.length > 0) {
      await this.writeIndex(indexed.filter((name) => live.has(name)));
    }
    return orphans;
  }

  /** indexedClusters is the set of names this store believes it holds secrets for. */
  async indexedClusters(): Promise<string[]> {
    return this.readIndex();
  }

  // ---------------------------------------------------------------------------

  private async read(key: string): Promise<string | undefined> {
    if (this.secrets === undefined) return undefined;
    try {
      return (await this.secrets.get(key))?.trim();
    } catch {
      return undefined;
    }
  }

  private async write(key: string, value: string): Promise<boolean> {
    if (this.secrets === undefined) return false;
    try {
      await this.secrets.store(key, value);
      return true;
    } catch {
      return false;
    }
  }

  private async remove(key: string): Promise<void> {
    if (this.secrets === undefined) return;
    try {
      await this.secrets.delete(key);
    } catch {
      // A delete that fails leaves a secret behind; the next reconcile retries.
    }
  }

  private async readIndex(): Promise<string[]> {
    const raw = await this.read(credentialIndexKey);
    if (raw === undefined || raw === "") return [];
    try {
      const parsed: unknown = JSON.parse(raw);
      return Array.isArray(parsed) ? parsed.filter((v): v is string => typeof v === "string") : [];
    } catch {
      return [];
    }
  }

  private async writeIndex(names: readonly string[]): Promise<void> {
    await this.write(credentialIndexKey, JSON.stringify([...new Set(names)]));
  }

  private async index(clusterName: string): Promise<void> {
    const current = await this.readIndex();
    if (current.includes(clusterName)) return;
    await this.writeIndex([...current, clusterName]);
  }

  private async deindex(clusterName: string): Promise<void> {
    const current = await this.readIndex();
    if (!current.includes(clusterName)) return;
    await this.writeIndex(current.filter((name) => name !== clusterName));
  }
}

// -----------------------------------------------------------------------------
// The three operations the rest of the extension performs
// -----------------------------------------------------------------------------

/**
 * persistSignIn stores everything a completed sign-in produced.
 *
 * The refresh token goes to SecretStorage and the plaintext key in
 * clusters.yaml is cleared in the same write. When there is no SecretStorage --
 * a locked or absent keyring -- the refresh token is written to the file
 * instead: it is the ingest path src/connection/credentials.ts already reads,
 * and a sign-in that silently discarded the thirty-day credential would leave
 * the operator re-authorizing through a browser every fifteen minutes.
 *
 * `client_id` is written unconditionally rather than only when the flow
 * registered a fresh one. It is not a secret (it is a public OAuth client
 * identifier), the write is already happening for the access token, and
 * re-writing the same value is a no-op -- whereas skipping it leaves a cluster
 * whose registration this extension performed but never recorded, which
 * re-registers on every sign-in.
 */
export async function persistSignIn(
  deps: CredentialStoreDeps,
  clusterName: string,
  tokens: SignInTokens,
): Promise<void> {
  const store = new ClusterCredentialStore(deps.secrets);
  const refreshToken = tokens.refreshToken.trim();
  const custodyTaken = await store.writeRefreshToken(clusterName, refreshToken);
  await store.writeExpiry(clusterName, tokens.expiresAtEpochSeconds);

  await deps.writeCluster({
    name: clusterName,
    token: tokens.accessToken.trim(),
    // "" DELETES the key. Written only when SecretStorage holds the token, or
    // when there is no refresh token at all -- otherwise the file is the only
    // copy and clearing it would destroy it.
    refreshToken: custodyTaken || refreshToken === "" ? "" : refreshToken,
    ...(tokens.clientId.trim() === "" ? {} : { clientId: tokens.clientId.trim() }),
  });
}

/**
 * signOut forgets a cluster's credentials completely.
 *
 * Both halves of the split, in one call: the SecretStorage entries are deleted
 * and de-indexed, and the file's `token` / `refresh_token` keys are removed so
 * the Cockpit sees the same signed-out cluster this extension does. The cluster
 * entry itself survives -- signing out is not deleting a cluster.
 *
 * Dropping the live connection and refreshing the tree are the CALLER's, since
 * both are editor-side (memql#3403).
 */
export async function signOut(
  deps: CredentialStoreDeps,
  clusterName: string,
): Promise<void> {
  await new ClusterCredentialStore(deps.secrets).clear(clusterName);
  await deps.writeCluster({ name: clusterName, token: "", refreshToken: "" });
}

/**
 * renameClusterCredentials moves a cluster's secrets when it is renamed.
 *
 * Call it from the edit flow ALONGSIDE the file write, not instead of it: the
 * file write moves the access token (it lives on the renamed node), this moves
 * the refresh token and the expiry.
 */
export async function renameClusterCredentials(
  deps: Pick<CredentialStoreDeps, "secrets">,
  previousName: string,
  newName: string,
): Promise<void> {
  await new ClusterCredentialStore(deps.secrets).rename(previousName, newName);
}

/**
 * reconcileClusterCredentials sweeps secrets belonging to clusters that are
 * gone, and returns the names swept.
 *
 * Safe to call on every read of clusters.yaml: it only ever deletes secrets for
 * names the file no longer carries.
 */
export async function reconcileClusterCredentials(
  deps: Pick<CredentialStoreDeps, "secrets">,
  liveClusterNames: readonly string[],
): Promise<string[]> {
  return new ClusterCredentialStore(deps.secrets).reconcile(liveClusterNames);
}

// -----------------------------------------------------------------------------
// Refresh once, retry once
// -----------------------------------------------------------------------------

/**
 * isUnauthorizedError reports whether a failure is the server refusing the
 * credential, as distinct from the cluster being unreachable.
 *
 * It matches on TEXT, because there is nothing else to match on. The credential
 * is presented on the WebSocket upgrade (sdk/ts/src/client/wsauth.ts), and a
 * rejected upgrade surfaces through `ws` as a plain
 * `Error: Unexpected server response: 401` -- no status field, no error code,
 * no cause chain. A dedicated error class would have to be introduced in the
 * SDK and plumbed through the upgrade path to do better.
 *
 * The consequence of a false positive is one wasted refresh followed by the
 * same failure; of a false negative, the opaque error this exists to prevent.
 * So the match is deliberately generous.
 */
export function isUnauthorizedError(err: unknown): boolean {
  const text = err instanceof Error ? `${err.name}: ${err.message}` : String(err);
  return /\b(401|403)\b/.test(text) || /unauthori[sz]ed|unauthenticated|permission denied/i.test(text);
}

export type AuthenticatedAttempt<T> = (bearer: string) => Promise<T>;

export type AuthenticatedRunResult<T> =
  | { ok: true; value: T; refreshed: boolean }
  | { ok: false; reason: CredentialFailureReason; message: string };

export interface AuthenticatedRunDeps {
  credentials: CredentialSource;
  /** Supply to have a terminal rejection clear the stored tokens. */
  store?: CredentialStoreDeps;
}

/**
 * runAuthenticated resolves a bearer, runs `attempt` with it, and -- if the
 * cluster answers 401 -- refreshes ONCE and retries ONCE.
 *
 * The retry is bounded at one on purpose. A 401 that survives a freshly minted
 * access token is not a stale-token problem, and retrying past that point is
 * how a client ends up spinning against an identity service issuing tokens the
 * cluster will not accept. The second failure instead CLEARS the stored tokens
 * and reports `reauthenticationRequired`, so the next action starts a clean
 * sign-in rather than replaying a dead credential.
 *
 * Any non-401 failure is rethrown untouched: it is the cluster's problem, not
 * the credential's, and swallowing it here would relabel "the cluster is down"
 * as "sign in again".
 */
export async function runAuthenticated<T>(
  cluster: ClusterConfig,
  deps: AuthenticatedRunDeps,
  attempt: AuthenticatedAttempt<T>,
): Promise<AuthenticatedRunResult<T>> {
  const resolved = await deps.credentials.resolve(cluster);
  if (!resolved.ok) return { ok: false, reason: resolved.reason, message: resolved.message };

  try {
    return { ok: true, value: await attempt(resolved.bearer), refreshed: false };
  } catch (err) {
    if (!isUnauthorizedError(err)) throw err;

    const bearer = await deps.credentials.forceRefresh(cluster);
    if (bearer === null) {
      await clearAfterRejection(deps, cluster.name);
      return {
        ok: false,
        reason: "reauthenticationRequired",
        message: reauthenticationMessage(cluster.name, errorText(err)),
      };
    }

    try {
      return { ok: true, value: await attempt(bearer), refreshed: true };
    } catch (retryErr) {
      if (!isUnauthorizedError(retryErr)) throw retryErr;
      await clearAfterRejection(deps, cluster.name);
      return {
        ok: false,
        reason: "reauthenticationRequired",
        message: reauthenticationMessage(cluster.name, errorText(retryErr)),
      };
    }
  }
}

/** The sentence an operator reads when their session cannot be renewed. */
export function reauthenticationMessage(clusterName: string, detail: string): string {
  return (
    `The credentials for cluster "${clusterName}" are no longer accepted and could not be renewed ` +
    `(${detail}). The stored tokens have been cleared -- sign in to "${clusterName}" again.`
  );
}

async function clearAfterRejection(
  deps: AuthenticatedRunDeps,
  clusterName: string,
): Promise<void> {
  if (deps.store === undefined) return;
  try {
    await signOut(deps.store, clusterName);
  } catch {
    // The report below is the point; a file that could not be written does not
    // change what the operator has to do next.
  }
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
