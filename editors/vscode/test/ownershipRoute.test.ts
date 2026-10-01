import assert from "node:assert/strict";
import { test } from "node:test";

import type { ClaimState } from "../src/clusters/claimState.js";
import {
  ownerSetupPending,
  receiptNamesAnotherCluster,
  resolveOwnershipRoute,
  routeForClaimState,
  type LocalEvidence,
} from "../src/clusters/ownershipRoute.js";

// ownershipRoute.test.ts
//
// THE FIELD FAILURE. On the owner's own machine -- a cluster built with
// `make up`, so no install receipt, and a passkey enrolled long ago -- every
// row click on the signed-out cluster raised a modal claiming "One step left:
// create this cluster's owner passkey", because the old route answered `enrol`
// for ANY local cluster with nothing stored, even after /setup had said
// "claimed". The button then dead-ended in "Re-run the installer".
//
// The rule now: a claimed cluster is signed in to. The owner passkey is an
// OFFER beside Sign in, made only on real evidence of a first run.

function probeReturning(state: ClaimState): () => Promise<ClaimState> {
  return async () => state;
}

test("a cluster the probe says is CLAIMED routes to Sign in, local or not", async () => {
  assert.equal(await resolveOwnershipRoute(probeReturning("claimed")), "signIn");
});

test("an unreachable or unreadable /setup routes to Sign in", async () => {
  // The local mkcert TLS used to make every local cluster answer `unknown`;
  // that is a sign-in, never a guess at a first run.
  assert.equal(await resolveOwnershipRoute(probeReturning("unknown")), "signIn");
});

test("CLAIM is reachable only on a real 200", () => {
  assert.equal(routeForClaimState("unclaimed"), "claim");
  assert.equal(routeForClaimState("claimed"), "signIn");
  assert.equal(routeForClaimState("unknown"), "signIn");
});

test("the owner passkey is offered only for this machine's never-signed-in install", () => {
  const firstRun: LocalEvidence = { local: true, ownerRecorded: true, enrolled: false };
  assert.equal(ownerSetupPending(firstRun), true);
  // The owner's own `make up` machine: no receipt, so no recorded owner.
  assert.equal(ownerSetupPending({ ...firstRun, ownerRecorded: false }), false);
  // Signed in here before (or a credential is stored): the passkey exists.
  assert.equal(ownerSetupPending({ ...firstRun, enrolled: true }), false);
  // A remote cluster has no pod to mint in.
  assert.equal(ownerSetupPending({ ...firstRun, local: false }), false);
});

// ---------------------------------------------------------------------------
// which cluster the receipt is actually about
// ---------------------------------------------------------------------------

test("two domains that DIFFER mean the receipt is about another cluster", () => {
  // `~/.memql/install-receipt.json` is ONE file and the extension holds a LIST
  // of clusters. Reading its owner as a fact about whichever cluster is in hand
  // would route a second local cluster to enrolment naming a stranger's
  // address -- and the mint execs against the CURRENT kubectl context, so the
  // name it carries and the cluster it lands on are chosen independently.
  assert.equal(receiptNamesAnotherCluster("lab.example.com", "memql.localhost"), true);
  assert.equal(receiptNamesAnotherCluster("memql.localhost", "memql.localhost"), false);
});

test("a match survives the spellings an operator actually types", () => {
  // Same tidying normalizeDomain does: a trailing dot and a capital letter name
  // the same cluster, and treating one as a mismatch would send the operator a
  // "different cluster" message about the only cluster they have.
  assert.equal(receiptNamesAnotherCluster(" MemQL.localhost. ", "memql.localhost"), false);
});

test("a MISSING domain is a gap, not a contradiction", () => {
  // The asymmetry is the design. Two known domains that differ is a fact;
  // silence is not, and refusing on it would break the documented case of a
  // cluster with no recorded domain letting the pod answer for itself.
  assert.equal(receiptNamesAnotherCluster("", "memql.localhost"), false);
  assert.equal(receiptNamesAnotherCluster("memql.localhost", ""), false);
  assert.equal(receiptNamesAnotherCluster("", ""), false);
});
