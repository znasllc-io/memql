// Turning the browser sign-in flow into something a command can drive.
//
// -----------------------------------------------------------------------------
// WHY THIS SITS BETWEEN flow.ts AND extension.ts
// -----------------------------------------------------------------------------
//
// src/auth/flow.ts returns tokens and deliberately owns nothing else -- no
// clusters.yaml, no SecretStorage, no ConnectionManager. src/extension.ts owns
// the editor: progress notifications, cancellation tokens, message boxes. What
// is left in between is a small amount of genuine DECISION-MAKING -- whether a
// cluster can be signed into at all, which of the ten failure kinds deserves a
// red toast versus silence, what gets persisted and in what order -- and none of
// it needs an editor to be true.
//
// So it lives here, free of `vscode` imports
// (cmd/memql-lsp/vscodeimportrule_test.go), where `node --test` can exercise it.
// extension.ts is left as the adapter that binds vscode.env.asExternalUri, a
// CancellationToken and a progress bar to the plain functions below.
//
// -----------------------------------------------------------------------------
// WHY THE TOKEN STORE IS AN INTERFACE HERE
// -----------------------------------------------------------------------------
//
// Persistence is memql#3404's, and it lands on a different branch. Depending on
// a NARROW interface rather than on that module means this file compiles and is
// tested today, and adopting the real implementation is an import change at the
// single place that constructs one (src/extension.ts) rather than an edit
// through every caller.

import type { ClusterConfig } from "../clusters/model.js";
import type { ConnectionErrorReason } from "../connection/manager.js";
import { identityBaseUrlFor } from "../connection/endpoint.js";
import { isAuthFlowError, type AuthFlowErrorKind } from "./errors.js";
import type { AuthFlowTokens } from "./flow.js";

/**
 * What a completed sign-in hands to the store.
 *
 * A subset of AuthFlowTokens on purpose: `clientId` is registry data that
 * belongs in clusters.yaml next to the endpoint, not credential data, so it
 * travels a different path (see ClientIdWriter).
 */
export interface SignInCredentials {
  /** The identity-issued JWT access token to dial the bff with. */
  accessToken: string;
  /** The long-lived token that renews it. "" when the server issued none. */
  refreshToken: string;
  /** Lifetime the server reported, in seconds. 0 when it reported none. */
  expiresInSeconds: number;
  /** Absolute expiry in epoch seconds. 0 when unknown. */
  expiresAtEpochSeconds: number;
}

/**
 * The persistence seam. memql#3404 owns the implementation.
 *
 * Two operations, because a sign-out is not a sign-in with empty strings: the
 * refresh token lives in the editor's secret storage and the access token in
 * the shared clusters.yaml, so forgetting a session touches two places and only
 * the store knows which.
 */
export interface SignInTokenStore {
  persistSignIn(clusterName: string, credentials: SignInCredentials): Promise<void>;
  signOut(clusterName: string): Promise<void>;
}

/** Writes the registered `client_id` back into clusters.yaml. */
export type ClientIdWriter = (clusterName: string, clientId: string) => Promise<void>;

/** Runs the browser flow. Bound to runAuthorizationFlow by the adapter layer. */
export type AuthFlowRunner = (
  cluster: ClusterConfig,
  signal: AbortSignal | undefined,
) => Promise<AuthFlowTokens>;

export interface PerformSignInDeps {
  runFlow: AuthFlowRunner;
  persistClientId: ClientIdWriter;
  store: SignInTokenStore;
  /** Wired to the progress notification's CancellationToken. */
  signal?: AbortSignal;
}

export interface SignInOutcome {
  /** The client_id the flow authorized with. */
  clientId: string;
  /** True when that client_id was newly minted and written to clusters.yaml. */
  clientIdPersisted: boolean;
  /** The access token's reported lifetime, for the confirmation message. */
  expiresInSeconds: number;
}

/**
 * canSignIn reports whether a browser sign-in has anywhere to go.
 *
 * The flow needs an ISSUER, which is a different fact from the endpoint the
 * stream dials -- `identity.<domain>` versus `cockpit.<domain>`. A cluster that
 * names neither an `issuer` nor a `domain` nor a `cockpit.`-prefixed endpoint
 * cannot be signed into, and offering the action anyway would put a button on
 * screen whose only possible outcome is an error toast.
 */
export function canSignIn(cluster: ClusterConfig): boolean {
  return identityBaseUrlFor(cluster) !== undefined;
}

/**
 * signInCanRecover reports whether a failed connection is one a sign-in fixes.
 *
 * `missingCredential`, `credentialExpired` and `wrongTokenClass` are all "the
 * bearer is the problem", and a fresh authorization mints a correct one. The
 * other two are not: `notConfigured` means there is no endpoint to dial (a
 * credential changes nothing), and `unreachable` / `lost` are the cluster
 * itself. Offering "Sign in" for those would send an operator through a browser
 * round trip to arrive at exactly the same failure.
 */
export function signInCanRecover(reason: ConnectionErrorReason): boolean {
  return (
    reason === "missingCredential" ||
    reason === "credentialExpired" ||
    reason === "wrongTokenClass"
  );
}

/**
 * performSignIn runs the flow and persists what came back.
 *
 * ORDERING IS DELIBERATE: the `client_id` is written BEFORE the tokens. It is
 * registration data rather than a credential -- a dynamically registered client
 * that is minted and then forgotten leaves an orphan registration on the
 * identity service and re-registers on every sign-in. The tokens can be re-
 * earned by signing in again; a lost client_id cannot be recovered at all. So
 * the unrecoverable half is committed first, and a token-store failure
 * propagates with the registration already safe.
 */
export async function performSignIn(
  cluster: ClusterConfig,
  deps: PerformSignInDeps,
): Promise<SignInOutcome> {
  const tokens = await deps.runFlow(cluster, deps.signal);

  // Written when the flow minted one, and ALSO when it differs from what the
  // file holds -- the second condition costs one write and closes the case
  // where a stale client_id was reconciled somewhere upstream.
  const stored = (cluster.clientId ?? "").trim();
  const shouldPersistClientId = tokens.clientIdWasRegistered || stored !== tokens.clientId;
  if (shouldPersistClientId) {
    await deps.persistClientId(cluster.name, tokens.clientId);
  }

  await deps.store.persistSignIn(cluster.name, {
    accessToken: tokens.accessToken,
    refreshToken: tokens.refreshToken,
    expiresInSeconds: tokens.expiresInSeconds,
    expiresAtEpochSeconds: tokens.expiresAtEpochSeconds,
  });

  return {
    clientId: tokens.clientId,
    clientIdPersisted: shouldPersistClientId,
    expiresInSeconds: tokens.expiresInSeconds,
  };
}

/** How loudly a failure should be reported. */
export type SignInFailureLevel = "silent" | "warning" | "error";

export interface SignInFailureReport {
  level: SignInFailureLevel;
  /** The sentence to show. Empty exactly when `level` is "silent". */
  message: string;
  /**
   * Whether running the same command again could plausibly succeed. A UI may
   * offer a retry affordance on true; it must not on false.
   */
  retryable: boolean;
}

/**
 * describeSignInFailure turns a rejection into what the operator should see.
 *
 * It branches on `kind`, NEVER on message text. The kinds are the contract
 * errors.ts documents; prose is not, and a UI that pattern-matched on sentences
 * would silently mis-handle the day one of them is reworded.
 *
 * The flow's own message already says WHAT happened, and it says it well -- so
 * this adds the half the flow deliberately does not know: what to do next,
 * which depends on the editor's surfaces (the Sign In command, clusters.yaml)
 * rather than on the protocol.
 */
export function describeSignInFailure(clusterName: string, err: unknown): SignInFailureReport {
  if (!isAuthFlowError(err)) {
    return {
      level: "error",
      message: `memQL: signing in to "${clusterName}" failed: ${err instanceof Error ? err.message : String(err)}`,
      retryable: false,
    };
  }
  const advice = adviceFor(err.kind);
  return {
    level: levelFor(err.kind),
    message:
      levelFor(err.kind) === "silent"
        ? ""
        : `memQL: signing in to "${clusterName}" failed. ${err.message}${advice === "" ? "" : ` ${advice}`}`,
    retryable: retryableFor(err.kind),
  };
}

// A user who cancelled already knows. Anything louder than silence there
// teaches an operator to dismiss memQL toasts without reading them, which costs
// us the ones that matter. A timeout is a warning rather than an error because
// nothing is broken -- a page was left unfinished.
function levelFor(kind: AuthFlowErrorKind): SignInFailureLevel {
  switch (kind) {
    case "cancelled":
      return "silent";
    case "timeout":
      return "warning";
    default:
      return "error";
  }
}

// `retryable` says whether the SAME command run again could work. It is not a
// judgement about severity: `stateMismatch` is the most serious kind here and
// is marked not-retryable precisely because a forged or replayed callback wants
// a human looking at it, not a second attempt one click away.
function retryableFor(kind: AuthFlowErrorKind): boolean {
  switch (kind) {
    case "misconfigured":
    case "browserUnavailable":
    case "stateMismatch":
      return false;
    default:
      return true;
  }
}

function adviceFor(kind: AuthFlowErrorKind): string {
  switch (kind) {
    case "misconfigured":
      return 'Edit the cluster ("memQL: Edit Cluster") and set its domain, or add an `issuer` to clusters.yaml.';
    case "registrationFailed":
      return "Nothing was opened and no credential changed. Retry once the identity service is reachable and accepting dynamic client registration.";
    case "bindFailed":
      return "This is a local machine problem -- a firewall or sandbox preventing a loopback listener -- not a problem with the cluster.";
    case "timeout":
      return 'Run "memQL: Sign In" again and finish the page in your browser.';
    case "browserUnavailable":
      return "This host cannot open a browser. Sign in on a machine that can and put the resulting `token` (and `refresh_token`) into clusters.yaml.";
    case "authorizationDenied":
      return "No credential was issued and nothing stored changed.";
    case "stateMismatch":
      return "The authorization code was discarded without being redeemed. Something replayed or forged the callback -- do not simply retry until you know what.";
    case "invalidCallback":
      return "Retrying starts a fresh sign-in; if it recurs, the identity service is returning a callback this editor cannot read.";
    case "exchangeRejected":
      return "The authorization code is spent either way, so a retry runs a whole new sign-in.";
    case "cancelled":
      return "";
  }
}
