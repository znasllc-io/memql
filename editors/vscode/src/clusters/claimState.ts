// Is this cluster claimed yet? (znasllc-io/memql#3885)
//
// WHAT WAS WRONG. Clicking a freshly installed cluster offered SIGN IN, and the
// sign-in had nothing to authenticate with -- so the browser flow ran out its
// deadline and fell back to a device code that could not complete either:
//
//   the browser sign-in could not complete (No sign-in callback arrived within
//   600 seconds...). Falling back to a device code.
//   enter code AGXC-QCLZ at https://identity.<domain>/device to finish signing in.
//
// The operator is asked to authorize a device against an account that cannot
// answer. Nothing in that sequence is wrong on its own; the entry point simply
// never asked which of three states the cluster was in.
//
// WHAT CREATES THE OWNER, because this comment used to state the opposite and
// the wrong model is how the next regression gets written (memql#4622). It is
// NOT the first sign-in. The install writes the owner ROW at identity boot --
// `seedBootstrap` creates it from the values the installer seeds -- so a cluster
// this extension installed HAS an owner before anybody opens a browser, and that
// owner holds no passkey and no magic-link identity. THAT is why signing in to a
// fresh install cannot work, and it is also why `/setup` is already sealed there.
// clusters/ownershipRoute.ts carries the three-state model in full; this module
// answers one question of it and defers to that file for the rest.
//
// A cluster with no owner at all is the HAND-ROLLED case -- one brought up
// without those seeded values. There the wizard below is the route, and it is
// the only route, because there is genuinely no account to sign in to.
//
// THE DEADLINE IN THE QUOTE ABOVE IS 600 SECONDS, not the 120 this text used to
// quote: `DEFAULT_CALLBACK_TIMEOUT_MS` has been 600_000 since memql#4600
// (src/auth/loopback.ts), sized for the magic-link round trip. A stale number in
// a quote is how a reader comes to trust the stale model around it.
//
// # The signal
//
// `GET <issuer>/setup` answers the question, and it is the only thing that
// does without a credential:
//
//   200  the ownership wizard RENDERS -- the cluster is UNCLAIMED
//   404  the wizard is SEALED        -- the cluster is CLAIMED
//
// That is not a side effect being read as a signal. The seal is wired to
// `Store.HasOwnerUser` -- "an active user holding the cluster-OWNER role
// exists" -- which memql#3415 chose precisely because an owner user is
// DEFINITIONAL proof the cluster was claimed and is not something a stray row
// produces. The page is pre-auth by necessity: it mints the first owner, so it
// has to be reachable before anyone holds anything.
//
// It also fails in the safe direction on its own terms. memql#3415 made "cannot
// prove this cluster is unclaimed" SEAL rather than serve, so a cluster whose
// state cannot be read answers 404 and we treat it as claimed -- which offers
// sign-in, exactly today's behaviour. A probe that cannot reach the host at all
// returns `unknown` and does the same.
//
// # Why the remedy is the wizard rather than the recovered magic link
//
// memql#3884 recovers the owner's magic link out of the identity pod's log
// during an install and opens it. That is the fast path and it stays. It cannot
// be the ENTRY POINT's answer, though: it needs `kubectl exec` into the
// workload, so it works only for a local cluster this extension installed, and
// only while the log window still holds the line. The operator who dismissed
// the notification, whose link expired, or who is looking at a remote cluster
// has none of that.
//
// `/setup` needs none of it. If it answers 200 the cluster is offering the
// wizard to whoever asks, so opening it is both the correct remedy and one that
// works identically for local and remote.

import { identityBaseUrlFor } from "../connection/endpoint.js";
import type { ClusterConfig } from "./model.js";

/** Binds to `fetch`. Injected so this module is testable without a network. */
export type FetchLike = (
  url: string,
  init?: { method?: string; redirect?: "follow" | "manual" | "error"; signal?: AbortSignal },
) => Promise<{ status: number }>;

/**
 * What we know about a cluster's ownership.
 *
 * `unknown` is a first-class answer, not an error. "I could not ask" and "there
 * is no owner" want opposite remedies, and reporting the first as the second
 * would send an operator to a wizard that is going to 404 at them.
 */
export type ClaimState = "unclaimed" | "claimed" | "unknown";

/** The path whose status code carries the answer. */
export const SETUP_PATH = "/setup";

/**
 * setupUrl composes the wizard URL from a cluster's issuer.
 *
 * Returns "" when the issuer is missing or is not an absolute http(s) URL --
 * there is nothing to probe and nothing to open, and guessing a host from the
 * gRPC endpoint would be inventing an address rather than reading one.
 */
export function setupUrl(issuer: string | undefined): string {
  const raw = (issuer ?? "").trim();
  if (raw === "") return "";
  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch {
    return "";
  }
  if (parsed.protocol !== "https:" && parsed.protocol !== "http:") return "";
  const base = parsed.href.endsWith("/") ? parsed.href.slice(0, -1) : parsed.href;
  return `${base}${SETUP_PATH}`;
}

export interface ProbeDeps {
  fetch: FetchLike;
  signal?: AbortSignal;
}

/**
 * probeClaimState asks the cluster whether it has an owner.
 *
 * REDIRECTS ARE NOT FOLLOWED. The answer is the status code of `/setup` itself,
 * and a host that redirects it somewhere friendly (a login page, an SSO
 * front door) would otherwise be read as whatever that destination answers.
 * A 3xx is `unknown` for the same reason a 500 is: the wizard neither rendered
 * nor sealed, so nothing was established.
 */
export async function probeClaimState(
  issuer: string | undefined,
  deps: ProbeDeps,
): Promise<ClaimState> {
  const url = setupUrl(issuer);
  if (url === "") return "unknown";

  let status: number;
  try {
    const res = await deps.fetch(url, { method: "GET", redirect: "manual", signal: deps.signal });
    status = res.status;
  } catch {
    // Unreachable host, TLS failure, aborted probe. Not evidence about
    // ownership.
    return "unknown";
  }

  if (status === 200) return "unclaimed";
  if (status === 404) return "claimed";
  return "unknown";
}

// -----------------------------------------------------------------------------
// THE CLUSTER-FACING HALF (memql#4620)
// -----------------------------------------------------------------------------
//
// THE TWO FUNCTIONS ABOVE TAKE AN ISSUER, AND THE EXTENSION HAD NO ISSUER TO
// GIVE THEM. `src/extension.ts` passed `cluster.issuer` -- a field NOTHING in
// this extension ever writes. The connect form's registration shape has no such
// key (state/addCluster.ts, `ClusterRegistration`), and the install path omits
// it deliberately and says why (install/handoff.ts: `identityBaseUrlFor` derives
// the host, so storing one would override the operator's own discovery document
// later). So the argument was `undefined` on every cluster the extension can
// produce, `setupUrl` returned "", `probeClaimState` short-circuited to
// `unknown`, and `routeForClaimState` mapped that to sign-in. The `claim` branch
// -- the whole of memql#3885's fix -- was unreachable in the shipped product,
// and an operator connecting to a genuinely unclaimed remote cluster was sent to
// authenticate against an account that does not exist. The exact dead end the
// issue exists to prevent, present in the tree and never taken.
//
// WHY WRAPPERS RATHER THAN A FIXED CALL SITE. "Which field names the identity
// service" is a decision, and `identityBaseUrlFor` is where it is already made
// -- issuer when the cockpit wrote one, else `identity.<domain>`, else the
// `api.<host>` endpoint's sibling. Leaving the composition inline in
// `extension.ts` leaves it where no test can reach it (the file imports
// `vscode`), which is precisely how a one-word argument stayed wrong. These two
// take a CLUSTER, so the entry point has no field left to choose.

/**
 * The wizard URL for a cluster, or "" when nothing names its identity service.
 *
 * "" keeps `setupUrl`'s contract: there is nothing to probe and nothing to open.
 * It now means "this cluster names no identity service at all" rather than "this
 * cluster has no `issuer` key", which was true of every cluster.
 */
export function setupUrlForCluster(cluster: ClusterConfig): string {
  return setupUrl(identityBaseUrlFor(cluster));
}

/** probeClaimState, pointed at the identity service the CLUSTER names. */
export function probeClaimStateForCluster(
  cluster: ClusterConfig,
  deps: ProbeDeps,
): Promise<ClaimState> {
  return probeClaimState(identityBaseUrlFor(cluster), deps);
}

/**
 * How long the probe may take before the answer is `unknown`.
 *
 * A BOUND, NOT A TUNING KNOB. The probe is one unauthenticated GET on the path
 * to a sign-in the operator has already asked for, and `unknown` is a
 * first-class answer that costs them nothing but the claim branch -- so the
 * question is only ever "how long may a sign-in wait to learn this", and the
 * answer is "not long". Node's fetch binds undici's 300-SECOND headers timeout
 * by default: with no signal at all, a cluster whose host does not route hung
 * `MemQL: Sign In` for five minutes before the command did anything, and until
 * memql#4620 it did so before any progress notification existed, so there was
 * no spinner and nothing to cancel.
 */
export const CLAIM_PROBE_TIMEOUT_MS = 5_000;

/**
 * The signal the probe runs under: the deadline above, plus the caller's own
 * cancellation when it has one.
 *
 * TWO SOURCES, ONE SIGNAL, because both mean the same thing to `probeClaimState`
 * -- a rejected fetch, caught, answered `unknown`. The caller's is the progress
 * notification's cancellation token, so an operator who does not want to wait
 * out even five seconds does not have to.
 */
export function claimProbeSignal(cancel?: AbortSignal): AbortSignal {
  const deadline = AbortSignal.timeout(CLAIM_PROBE_TIMEOUT_MS);
  return cancel === undefined ? deadline : AbortSignal.any([deadline, cancel]);
}

// WHAT THE ENTRY POINT DOES WITH THIS ANSWER LIVES NEXT DOOR (memql#3906).
//
// `entryActionFor` used to be here, mapping unclaimed -> claim and everything
// else -> sign-in. That mapping survives verbatim as `routeForClaimState` in
// clusters/ownershipRoute.ts; it moved because it is one branch of a three-way
// decision rather than the whole of one, and leaving a second function that
// answers two thirds of the same question is how the two come to disagree.
//
// This module's job stops at reading the status code.
//
// ONE LIMIT WORTH KNOWING BEFORE TRUSTING AN ANSWER FROM HERE. `probeClaimState`
// binds to the caller's `fetch`, and Node's verifies against its OWN bundled
// roots rather than the OS/NSS store the local installer puts its mkcert CA
// into. So a cluster on the machine that installed it answers `unknown` --
// UNABLE_TO_VERIFY_LEAF_SIGNATURE, caught above -- not because its ownership is
// unreadable but because the handshake never completed. That is why
// ownershipRoute.ts asks the install receipt first and reaches this only for
// the clusters local evidence cannot speak for.
