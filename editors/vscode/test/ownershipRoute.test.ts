import assert from "node:assert/strict";
import { test } from "node:test";

import type { ClaimState } from "../src/clusters/claimState.js";
import {
  evidenceSettlesIt,
  isFirstCredentialPending,
  receiptNamesAnotherCluster,
  resolveOwnershipRoute,
  routeForClaimState,
  type LocalEvidence,
  type OwnershipRoute,
} from "../src/clusters/ownershipRoute.js";

// ownershipRoute.test.ts -- memql#3906.
//
// An operator installs a local cluster from the plugin and cannot get in. The
// owner account exists -- `seedBootstrap` made it from the seeded values -- and
// holds no human credential at all, so sign-in has nothing to authenticate
// with. Verified on a live cluster: 21 `v1:identity:identity` rows, every one a
// node_token.
//
// memql#3885's entry point asks `/setup` whether the cluster is claimed. On an
// installer-built cluster that page is sealed (404), and the probe cannot even
// reach it over the local mkcert TLS, so the answer is always `unknown` and
// `unknown` means sign-in. The tests below are mostly about the ORDER the two
// kinds of evidence are consulted in, because that is what was wrong.

const installed: LocalEvidence = { local: true, ownerRecorded: true, credentialMissing: true };

/** A probe that fails the test if it is ever awaited. */
const probeThatMustNotRun: () => Promise<ClaimState> = async () => {
  throw new Error("the cluster was dialled when local evidence already had the answer");
};

function probeReturning(state: ClaimState): () => Promise<ClaimState> {
  return async () => state;
}

test("an installed cluster with no credential routes to ENROL without dialling", async () => {
  // The whole point. The receipt records the owner seedBootstrap was given, so
  // the account is known to exist; no credential is stored, so the only route
  // to a first one is an enrolment link. Nothing about that needs the network,
  // and on this exact cluster the network cannot answer.
  const got = await resolveOwnershipRoute(installed, probeThatMustNotRun);
  assert.equal(got, "enrol" satisfies OwnershipRoute);
});

test("the probe is not called when local evidence settles it", async () => {
  let dialled = 0;
  const got = await resolveOwnershipRoute(installed, async () => {
    dialled += 1;
    return "unknown";
  });
  assert.equal(got, "enrol");
  assert.equal(
    dialled,
    0,
    "a local cluster's /setup cannot be verified over mkcert TLS, so dialling it spends a handshake failure to learn nothing",
  );
});

test("evidenceSettlesIt needs all three, and says so one at a time", () => {
  assert.equal(evidenceSettlesIt(installed), true);
  // Not this machine's cluster: there is no pod here to mint in, so enrolment
  // is somebody else's to issue.
  assert.equal(evidenceSettlesIt({ ...installed, local: false }), false);
  // No recorded owner: nothing names an account to enrol, and the cluster may
  // genuinely have none -- that is the /setup case.
  assert.equal(evidenceSettlesIt({ ...installed, ownerRecorded: false }), false);
  // A credential is already stored. Sign-in is the route; offering to mint a
  // first credential to somebody who holds one is answering the wrong question.
  assert.equal(evidenceSettlesIt({ ...installed, credentialMissing: false }), false);
});

test("a REMOTE cluster falls through to the probe, unchanged", async () => {
  // #3885's mapping applies verbatim. Nothing here can be minted from this
  // machine, so there is no third answer to offer.
  const remote: LocalEvidence = { local: false, ownerRecorded: false, credentialMissing: true };
  assert.equal(await resolveOwnershipRoute(remote, probeReturning("unclaimed")), "claim");
  assert.equal(await resolveOwnershipRoute(remote, probeReturning("claimed")), "signIn");
  assert.equal(await resolveOwnershipRoute(remote, probeReturning("unknown")), "signIn");
});

test("a LOCAL cluster with nothing stored is never dropped into a bare sign-in", async () => {
  // The blind-probe case, and the one that would otherwise reproduce #3885's
  // original complaint on the only cluster kind that cannot escape it: local
  // TLS defeats the probe, so `unknown` is all it will ever say, and sign-in
  // times out then falls back to a device code that cannot complete.
  //
  // Offering ownership costs a dialog whose other button is still "Sign in".
  // If there is nothing to enrol against, the mint names which of the two
  // things is missing; a timeout names none.
  const local: LocalEvidence = { local: true, ownerRecorded: false, credentialMissing: true };
  assert.equal(await resolveOwnershipRoute(local, probeReturning("unknown")), "enrol");
  assert.equal(await resolveOwnershipRoute(local, probeReturning("claimed")), "enrol");
  // A cluster whose wizard genuinely RENDERS is still sent there: /setup is the
  // right answer when there is no owner at all, and a verified 200 says so.
  assert.equal(await resolveOwnershipRoute(local, probeReturning("unclaimed")), "claim");
});

test("a local cluster that holds a credential is left alone", async () => {
  const local: LocalEvidence = { local: true, ownerRecorded: false, credentialMissing: false };
  assert.equal(await resolveOwnershipRoute(local, probeReturning("unknown")), "signIn");
  assert.equal(await resolveOwnershipRoute(local, probeReturning("claimed")), "signIn");
});

test("CLAIM is reachable only on a real 200", () => {
  // The property that was missing. `/setup` is sealed on every installer-built
  // cluster, so a route that offered it on anything less than a rendered page
  // was offering a 404. `unknown` is not evidence of anything.
  assert.equal(routeForClaimState("unclaimed"), "claim");
  for (const state of ["claimed", "unknown"] satisfies ClaimState[]) {
    assert.notEqual(routeForClaimState(state), "claim", `${state} must not offer the wizard`);
  }
});

test("a stored credential never routes to enrol, however the cluster answers", async () => {
  // Holding a credential is the strongest evidence there is that this operator
  // is already in. Whatever /setup says, minting them a FIRST credential is not
  // the thing they need.
  const held: LocalEvidence = { local: true, ownerRecorded: true, credentialMissing: false };
  for (const state of ["unclaimed", "claimed", "unknown"] satisfies ClaimState[]) {
    const got = await resolveOwnershipRoute(held, probeReturning(state));
    assert.notEqual(got, "enrol", `a held credential must not be answered with enrolment (${state})`);
  }
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

test("an installed cluster is never sent to the sealed wizard", async () => {
  // The regression this exists to prevent. Before the ordering change, an
  // installer-built cluster reached the probe, and any answer other than 404
  // -- a proxy's 200, a captive portal -- would have opened /setup at it.
  for (const state of ["unclaimed", "claimed", "unknown"] satisfies ClaimState[]) {
    const got = await resolveOwnershipRoute(installed, probeReturning(state));
    assert.equal(got, "enrol", `local evidence must win over a ${state} probe`);
  }
});

// ---------------------------------------------------------------------------
// a missing FIRST credential is not a fault (memql#3909)
// ---------------------------------------------------------------------------

test("an installed cluster with no credential is an offer, not an error", () => {
  // THE REPORTED BUG. `completeInstallHandoff` selects the cluster it just
  // registered, the selection command dials, and `runAuthenticated` resolves
  // credentials BEFORE touching the network -- so a cluster built thirty
  // seconds ago resolved as `missingCredential` every single time. A
  // successful install ended in a red toast telling the operator to hand-edit
  // clusters.yaml with a JWT, offering Sign in, which cannot work.
  assert.equal(isFirstCredentialPending("missingCredential", "enrol"), true);
});

test("a REAL auth failure keeps reading as one", () => {
  // "Create this cluster's owner passkey" would be a confidently wrong remedy for every one of
  // these: the credential exists and is expired, revoked, of a class the mesh
  // rejects, or the cluster is simply down.
  for (const reason of [
    "credentialExpired",
    "reauthenticationRequired",
    "wrongTokenClass",
    "notConfigured",
    "unreachable",
    "lost",
  ]) {
    assert.equal(
      isFirstCredentialPending(reason, "enrol"),
      false,
      `${reason} must not be answered with "create the owner passkey"`,
    );
  }
});

test("a cluster this machine cannot enrol keeps the plain error", () => {
  // `enrol` is what carries the checks that there is a pod here to mint in and
  // an owner account to mint against. Without it the offer would be a button
  // whose only outcome is a refusal -- which is the thing the tree menu guard
  // refuses to ship for the same reason.
  assert.equal(isFirstCredentialPending("missingCredential", "signIn"), false);
  assert.equal(isFirstCredentialPending("missingCredential", "claim"), false);
});
