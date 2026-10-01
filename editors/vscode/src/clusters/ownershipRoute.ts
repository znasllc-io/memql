// What an operator needs NEXT from a cluster they cannot get into (memql#3906).
//
// # Two routes and one offer
//
//   nobody owns it       -> claim: /setup, which mints the first owner
//   anything else        -> sign in
//
// and, beside the route rather than instead of it:
//
//   the owner this machine's install recorded has never signed in here
//                        -> "Create the owner passkey" is OFFERED
//
// # Why "claimed" always means Sign in
//
// This module used to have a third route, `enrol`, and it was chosen for ANY
// local cluster with no credential stored in clusters.yaml -- including after
// the /setup probe had answered "claimed". On the owner's own machine (a
// cluster built with `make up`, so no install receipt, and a passkey enrolled
// long ago) that sent every row click to a modal claiming "One step left:
// create this cluster's owner passkey", whose button dead-ended in "Re-run the
// installer". A cluster that has an owner is one to sign in to. The passkey is
// the install hand-off's step, and elsewhere it is only an OFFER, made when
// the evidence for it is real: this machine's install receipt names the owner
// of THIS cluster, and no sign-in has completed here since.
//
// # Why the probe is asked at all
//
// `GET <issuer>/setup` answers 200 only while the ownership wizard renders --
// the one state in which there is no account to sign in to. `unknown` (an
// unreachable host, a TLS failure) and `claimed` both route to sign in, which
// is what an operator asked for; the wizard is offered only on a real 200.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import type { ClaimState } from "./claimState.js";

/** What the sign-in entry point does: open the ownership wizard, or sign in. */
export type OwnershipRoute = "claim" | "signIn";

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
   */
  ownerRecorded: boolean;
  /**
   * The owner has been in: a sign-in has completed for this cluster on this
   * machine, or a credential for it is stored here.
   */
  enrolled: boolean;
}

/**
 * Whether "Create the owner passkey" may be offered.
 *
 * All three, because each alone is not evidence of a first run: a remote
 * cluster has no pod to mint in, a cluster with no recorded owner has no
 * account to name (the mint refuses it as `noOwner`), and an owner who has
 * signed in here already holds a credential.
 */
export function ownerSetupPending(evidence: LocalEvidence): boolean {
  return evidence.local && evidence.ownerRecorded && !evidence.enrolled;
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
 * The route: the ownership wizard only when the cluster says it has no owner,
 * sign in otherwise.
 *
 * `probe` is a thunk so a caller that already knows can skip the round trip,
 * and so a test can see it was consulted.
 */
export async function resolveOwnershipRoute(probe: () => Promise<ClaimState>): Promise<OwnershipRoute> {
  return routeForClaimState(await probe());
}

/**
 * memql#3885's mapping. `unknown` means sign-in: a cluster whose `/setup` is
 * hidden behind a proxy, or unreachable from this process, must not lose a
 * sign-in that works.
 */
export function routeForClaimState(state: ClaimState): OwnershipRoute {
  return state === "unclaimed" ? "claim" : "signIn";
}
