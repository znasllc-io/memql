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
   * The install receipt records the owner `seedBootstrap` bootstrapped.
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
 * The route, consulting the cluster only when this machine cannot answer.
 *
 * `probe` is a thunk rather than a value so that NOT CALLING IT is observable.
 * That is the load-bearing behaviour: on the cluster this issue is about the
 * probe cannot succeed, and a version that dialled anyway would spend a TLS
 * failure on every sign-in to learn nothing.
 */
export async function resolveOwnershipRoute(
  evidence: LocalEvidence,
  probe: () => Promise<ClaimState>,
): Promise<OwnershipRoute> {
  if (evidenceSettlesIt(evidence)) return "enrol";
  return routeForClaimState(await probe());
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
