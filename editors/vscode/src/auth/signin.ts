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
 * `clientId` travels WITH the tokens because it is a fact about them: the
 * client the refresh token was issued to, which a refresh must present. The
 * store keeps it beside the refresh token in SecretStorage and never writes it
 * to clusters.yaml, whose `client_id` belongs to whichever tool wrote it
 * (wellKnownClient.ts).
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
  /** The client the tokens were issued to: the editor's own. */
  clientId: string;
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

/** Runs the browser flow. Bound to runAuthorizationFlow by the adapter layer. */
export type AuthFlowRunner = (
  cluster: ClusterConfig,
  signal: AbortSignal | undefined,
) => Promise<AuthFlowTokens>;

/**
 * Which grant a sign-in should run.
 *
 * `auto` is what `MemQL: Sign In` uses. `deviceCode` is the deliberate command
 * (memql#3411) for a user who already knows their host cannot do loopback and
 * should not spend the callback deadline finding out.
 */
export type SignInFlow = "auto" | "deviceCode";

/**
 * The two grants, injected. The editor binds them; this file only chooses.
 */
export interface SignInFlowRunners {
  /**
   * Loopback, FALLING BACK to the device grant when the host proves it cannot
   * do loopback. Named for what it does rather than for loopback alone, because
   * the whole of memql#3515 is that these two are not the same runner and were
   * treated as if they were.
   */
  loopbackWithDeviceFallback: AuthFlowRunner;
  /** The device grant, deliberately, with no loopback attempt. */
  deviceCode: AuthFlowRunner;
}

/**
 * selectSignInRunner picks the grant for a flow.
 *
 * A three-line function with a test, because the three lines are exactly what
 * was wrong. Until memql#3515 the default sign-in ran `runAuthorizationFlow` --
 * loopback ALONE -- while an unreachable, identically-named function two files
 * away ran the fallback, and the verification runbook documented the fallback as
 * not actually reached. Nothing failed: a host that could do loopback signed in
 * fine, and a host that could not waited out the callback deadline and was told
 * it had failed with the remedy sitting unused in the same repository.
 *
 * Pulling the choice out of extension.ts is what makes it assertable at all:
 * everything around it in that file needs a live editor, so the one decision
 * worth pinning was the one nothing could reach.
 */
export function selectSignInRunner(flow: SignInFlow, runners: SignInFlowRunners): AuthFlowRunner {
  return flow === "deviceCode" ? runners.deviceCode : runners.loopbackWithDeviceFallback;
}

export interface PerformSignInDeps {
  runFlow: AuthFlowRunner;
  store: SignInTokenStore;
  /** Wired to the progress notification's CancellationToken. */
  signal?: AbortSignal;
}

export interface SignInOutcome {
  /** The client_id the flow authorized with. */
  clientId: string;
  /** The access token's reported lifetime, for the confirmation message. */
  expiresInSeconds: number;
}

/**
 * canSignIn reports whether a browser sign-in has anywhere to go.
 *
 * The flow needs an ISSUER, which is a different fact from the endpoint the
 * stream dials -- `identity.<domain>` versus `api.<domain>`. A cluster that
 * names neither an `issuer` nor a `domain` nor an `api.`-prefixed endpoint
 * cannot be signed into, and offering the action anyway would put a button on
 * screen whose only possible outcome is an error toast.
 */
export function canSignIn(cluster: ClusterConfig): boolean {
  return identityBaseUrlFor(cluster) !== undefined;
}

/**
 * signInCanRecover reports whether a failed connection is one a sign-in fixes.
 *
 * `missingCredential`, `credentialExpired`, `wrongTokenClass` and
 * `reauthenticationRequired` are all "the bearer is the problem", and a fresh
 * authorization mints a correct one. The last is the common "came back after
 * a while" case -- the stored session was refused and cleared -- and it used
 * to be missing here, so its toast said to sign in and offered no button.
 *
 * The other reasons are not: `notConfigured` means there is no endpoint to
 * dial (a credential changes nothing), and `unreachable` / `lost` are the
 * cluster itself. Offering "Sign in" for those would send an operator through
 * a browser round trip to arrive at exactly the same failure.
 */
export function signInCanRecover(reason: ConnectionErrorReason): boolean {
  return (
    reason === "missingCredential" ||
    reason === "credentialExpired" ||
    reason === "wrongTokenClass" ||
    reason === "reauthenticationRequired"
  );
}

/**
 * performSignIn runs the flow and persists what came back.
 *
 * IT DOES NOT WRITE clusters.yaml's `client_id`. The id the flow authorized
 * with is the editor's own, and it is persisted with the refresh token it
 * belongs to rather than in a file the Cockpit also writes -- where it would
 * either overwrite the Cockpit's own client or be overwritten by it.
 */
export async function performSignIn(
  cluster: ClusterConfig,
  deps: PerformSignInDeps,
): Promise<SignInOutcome> {
  const tokens = await deps.runFlow(cluster, deps.signal);

  await deps.store.persistSignIn(cluster.name, {
    accessToken: tokens.accessToken,
    refreshToken: tokens.refreshToken,
    expiresInSeconds: tokens.expiresInSeconds,
    expiresAtEpochSeconds: tokens.expiresAtEpochSeconds,
    clientId: tokens.clientId,
  });

  return {
    clientId: tokens.clientId,
    expiresInSeconds: tokens.expiresInSeconds,
  };
}

/** How loudly a failure should be reported. */
export type SignInFailureLevel = "silent" | "warning" | "error";

export interface SignInFailureReport {
  level: SignInFailureLevel;
  /**
   * The one sentence a toast shows: what happened, in the person's words.
   * Empty exactly when `level` is "silent". The fix is the toast's button
   * (clusters/signInRecovery.ts), never a command named in prose.
   */
  message: string;
  /**
   * The full record for the MemQL Connection output: the flow's own
   * explanation, with every protocol detail it carries. Empty when silent.
   */
  detail: string;
  /**
   * Whether running the same command again could plausibly succeed. A UI may
   * offer a retry affordance on true; it must not on false.
   */
  retryable: boolean;
}

/**
 * describeSignInFailure turns a rejection into what the person should see.
 *
 * It branches on `kind`, NEVER on message text. The kinds are the contract
 * errors.ts documents; prose is not, and a UI that pattern-matched on sentences
 * would silently mis-handle the day one of them is reworded.
 *
 * TWO TEXTS, ONE FOR EACH READER. The toast gets one plain sentence naming the
 * cluster by the label its row shows; the flow's own explanation -- endpoints,
 * status codes, OAuth error codes -- goes to the Output channel as `detail`.
 * The toast used to be both, truncated at 140 characters, which cut off the
 * part that said what to do.
 */
export function describeSignInFailure(clusterLabel: string, err: unknown): SignInFailureReport {
  if (!isAuthFlowError(err)) {
    return {
      level: "error",
      message: `Couldn't sign in to ${clusterLabel}.`,
      detail: err instanceof Error ? err.message : String(err),
      retryable: false,
    };
  }
  const level = levelFor(err.kind);
  if (level === "silent") return { level, message: "", detail: "", retryable: false };
  return {
    level,
    message: sentenceFor(err.kind, clusterLabel, err.serverMessage),
    detail: err.message,
    retryable: retryableFor(err.kind),
  };
}

// A user who cancelled already knows. Anything louder than silence there
// teaches an operator to dismiss MemQL toasts without reading them, which costs
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
// a human looking at it, not a second attempt one click away. `clientRefused`
// is not retryable because the cluster refused this editor's client outright,
// and both grants present the same one.
function retryableFor(kind: AuthFlowErrorKind): boolean {
  switch (kind) {
    case "misconfigured":
    case "browserUnavailable":
    case "stateMismatch":
    case "clientRefused":
      return false;
    default:
      return true;
  }
}

function sentenceFor(kind: AuthFlowErrorKind, label: string, server: string | undefined): string {
  switch (kind) {
    case "misconfigured":
      return `${label} has no domain set.`;
    case "registrationFailed":
      return `Couldn't reach the sign-in service for ${label}.`;
    case "bindFailed":
      return "Something on this computer blocked the sign-in.";
    case "timeout":
      return "Sign-in didn't finish in time.";
    case "browserUnavailable":
      return "This window can't open a browser.";
    case "authorizationDenied":
      // The server's own sentence when it gave one: the role-floor refusal
      // names the person's role and who can raise it.
      return server !== undefined ? `${label} declined the sign-in. ${server}` : `${label} declined the sign-in.`;
    case "stateMismatch":
      return "The sign-in response didn't match this request, so it was rejected.";
    case "invalidCallback":
      return "The sign-in response couldn't be read.";
    case "exchangeRejected":
      return `Couldn't finish signing in to ${label}.`;
    case "clientRefused":
      return `${label} doesn't accept sign-in from VS Code. Update it to the current MemQL release.`;
    case "cancelled":
      return "";
  }
}
