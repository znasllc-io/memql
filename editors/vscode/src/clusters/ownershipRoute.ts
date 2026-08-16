// What an operator needs NEXT from a cluster they cannot get into (memql#3906).
//
// memql#3885 asked one question at the entry point -- "has anyone claimed this
// cluster?" -- and answered it with `GET <issuer>/setup`. That question is
// right and its answer is sound. It is just not the whole state space, and the
// state it leaves out is the one every installer-built cluster is actually in.
//
// # Three states, not two
//
//   nobody owns it          -> /setup, which mints the first owner
//   owner exists, no
//     credential on this
//     machine               -> an enrolment link, which mints the first passkey
//   credential in hand      -> sign in
//
// `/setup` distinguishes the first from the other two. Nothing unauthenticated
// distinguishes the second from the third, and nothing should: an anonymous way
// to ask "does this account have a passkey" is an enumeration oracle
// (memql#3902 makes the same point from the other side).
//
// # Why the probe cannot be asked first
//
// TWO reasons, and the second was found by running it rather than reading it.
//
//  1. `seedBootstrap` creates the owner from the values the installer seeds, so
//     a cluster this extension installed HAS an owner before an operator opens
//     a browser. `/setup` is gated on `Store.HasOwnerUser` and is therefore
//     sealed -- verified, `GET https://identity.<domain>/setup` -> 404 on a
//     freshly installed cluster. Offering the wizard there is offering a 404.
//
//  2. `probeClaimState` binds to `globalThis.fetch`, and Node's fetch verifies
//     against its OWN bundled roots rather than the OS/NSS store the installer
//     puts the local mkcert CA into. So the probe does not merely answer wrong
//     for a local cluster, it cannot answer at all:
//
//         fetch('https://identity.memql.localhost/setup')
//           -> TypeError: fetch failed
//              cause: UNABLE_TO_VERIFY_LEAF_SIGNATURE
//
//     which `probeClaimState` correctly reports as `unknown`. Every local
//     cluster answers `unknown`, `unknown` maps to sign-in, and sign-in is the
//     one thing that cannot work. The probe was blind on precisely the clusters
//     the feature is for.
//
// # So the local evidence is asked first
//
// The install receipt records the owner `seedBootstrap` was given
// (`recordedOwner`, memql#3904). That is a stronger signal than the probe and a
// cheaper one: it is on this disk, it needs no network, no TLS and no timeout,
// and it is the same value the mint will name. When it is present and no
// credential is stored, the answer is `enrol` and nothing is dialled.
//
// Only when local evidence cannot answer -- a remote cluster, or a local one
// with no receipt -- is the probe consulted, and then #3885's mapping applies
// unchanged. `claim` therefore becomes reachable ONLY on a real 200, which is
// the property that was missing: the wizard is offered when the wizard renders,
// and never on a guess.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import type { ClaimState } from "./claimState.js";

/**
 * What the entry point should offer.
 *
 * The same three words the state table uses, so a reader can hold one model
 * rather than translating between a probe's vocabulary and a UI's.
 */
export type OwnershipRoute = "enrol" | "claim" | "signIn";

/**
 * What this machine knows about the cluster without asking it anything.
 *
 * Three booleans rather than a `ClusterConfig` and a `Receipt`: the decision is
 * the thing worth testing, and passing the whole world in would make every case
 * below need a fixture cluster and a fixture receipt to say one thing.
 */
export interface LocalEvidence {
  /** This machine installed the cluster, so there is a pod to mint in. */
  local: boolean;
  /**
   * The install receipt records the owner `seedBootstrap` bootstrapped, AND
   * that receipt is about this cluster (`receiptCoversCluster`).
   *
   * Presence is the claim being made: an owner ACCOUNT exists. It says nothing
   * about credentials, which is exactly the distinction that matters here.
   */
  ownerRecorded: boolean;
  /** No token or refresh token for this cluster is stored on this machine. */
  credentialMissing: boolean;
}

/**
 * Whether local evidence settles the question on its own.
 *
 * Exported because "was the network dialled" is the behaviour worth asserting,
 * and a caller that wants to decide without holding a probe can ask directly.
 */
export function evidenceSettlesIt(evidence: LocalEvidence): boolean {
  return evidence.local && evidence.ownerRecorded && evidence.credentialMissing;
}

/**
 * Whether the install receipt is demonstrably about a DIFFERENT cluster.
 *
 * `~/.memql/install-receipt.json` is a SINGLE file and the extension holds a
 * LIST of clusters, several of which can be local. So "the receipt records an
 * owner" is not on its own a fact about the cluster in hand -- it is a fact
 * about whichever cluster was installed last. Acting on it regardless would
 * route a second local cluster to enrolment naming a stranger's address, and
 * the mint execs against the CURRENT kubectl context, so the account it names
 * and the cluster it lands on are chosen independently.
 *
 * The domain is what ties the two together: it is the one value the installer
 * collects, the receipt stamps it on `--domain`, and the hand-off composes the
 * registry entry from it. Compared case-insensitively and dot-trimmed for the
 * same reason `normalizeDomain` exists -- an operator who typed a trailing dot
 * has named the same cluster.
 *
 * WHAT IT DETECTS IS A CONTRADICTION, NOT AN ABSENCE, and the asymmetry is
 * deliberate. Two known domains that DIFFER is a fact: this receipt is not
 * about this cluster. A missing domain on either side is not -- and refusing
 * there would break the case `identityBaseUrlForCluster` documents, a cluster
 * with no recorded domain deliberately letting the pod's own
 * MEMQL_IDENTITY_BASE_URL answer. Strictness there would cost a working path
 * to guard a state that cannot arise from the installer, which always records
 * both.
 *
 * Phrased as the negative so both callers can share one rule: the route offers
 * enrolment exactly when the mint will accept it, so the editor can never put
 * a button in front of an operator that its own next step refuses.
 */
export function receiptNamesAnotherCluster(clusterDomain: string, receiptDomain: string): boolean {
  const tidy = (value: string): string => value.trim().toLowerCase().replace(/^\.+|\.+$/g, "");
  const cluster = tidy(clusterDomain);
  const receipt = tidy(receiptDomain);
  if (cluster === "" || receipt === "") return false;
  return cluster !== receipt;
}

/**
 * The route, consulting the cluster only when this machine cannot answer.
 *
 * `probe` is a thunk rather than a value so that NOT CALLING IT is observable.
 * That is the load-bearing behaviour: on the cluster this issue is about the
 * probe cannot succeed, and a version that dialled anyway would spend a TLS
 * failure on every sign-in to learn nothing.
 *
 * THE LAST CLAUSE IS NOT A TIE-BREAK, it is the blind-probe case. A LOCAL
 * cluster holding no credential is one this machine can act on, and the probe
 * has just told us nothing about it -- so falling through to a bare sign-in
 * would reproduce memql#3885's original complaint on the one cluster kind that
 * cannot escape it: the browser flow times out and the device code cannot
 * complete. Offering ownership instead costs a dialog whose other button is
 * still "Sign in", and if there turns out to be nothing to enrol against, the
 * mint says which of the two things is missing -- `noOwner` or `otherCluster`
 * -- both of which name a next step. A sign-in timeout names none.
 */
export async function resolveOwnershipRoute(
  evidence: LocalEvidence,
  probe: () => Promise<ClaimState>,
): Promise<OwnershipRoute> {
  if (evidenceSettlesIt(evidence)) return "enrol";
  const probed = routeForClaimState(await probe());
  if (probed === "claim") return "claim";
  return evidence.local && evidence.credentialMissing ? "enrol" : "signIn";
}

/**
 * memql#3885's mapping, unchanged, for the clusters local evidence cannot
 * speak for.
 *
 * `unknown` still means sign-in. It is the previous behaviour, and a cluster
 * whose `/setup` is hidden behind a proxy must not lose a sign-in that works.
 */
export function routeForClaimState(state: ClaimState): OwnershipRoute {
  return state === "unclaimed" ? "claim" : "signIn";
}
