import assert from "node:assert/strict";
import { test } from "node:test";

import {
  probeClaimState,
  setupUrl,
  type ClaimState,
  type FetchLike,
} from "../src/clusters/claimState.js";

// claimState.test.ts -- znasllc-io/memql#3885.
//
// Clicking a freshly installed cluster offered SIGN IN. A cluster is claimed by
// its FIRST sign-in -- that sign-in is what creates the owner -- so the browser
// flow timed out and fell back to a device code that could not complete either:
// the operator was asked to authorize a device against an account that did not
// exist.
//
// The fix turns on one status code, so these tests are mostly about that status
// code meaning what we think it means, and about the two ways of being wrong
// costing different amounts.

function fetchReturning(status: number): FetchLike {
  return async () => ({ status });
}

const fetchThrowing: FetchLike = async () => {
  throw new Error("ECONNREFUSED");
};

test("200 from /setup means the cluster is UNCLAIMED", async () => {
  const got = await probeClaimState("https://identity.memql.localhost", {
    fetch: fetchReturning(200),
  });
  assert.equal(got, "unclaimed" satisfies ClaimState);
});

test("404 from /setup means the cluster is CLAIMED", async () => {
  // The seal is wired to Store.HasOwnerUser -- an active user holding the
  // cluster-OWNER role. memql#3415 chose that signal because an owner user is
  // definitional proof the cluster was claimed and is not something a stray row
  // produces.
  const got = await probeClaimState("https://identity.memql.localhost", {
    fetch: fetchReturning(404),
  });
  assert.equal(got, "claimed" satisfies ClaimState);
});

test("an unreachable cluster is UNKNOWN, not unclaimed", async () => {
  // "I could not ask" and "there is no owner" want opposite remedies. Reporting
  // the first as the second sends an operator to a wizard that is going to 404
  // at them.
  const got = await probeClaimState("https://identity.memql.localhost", { fetch: fetchThrowing });
  assert.equal(got, "unknown" satisfies ClaimState);
});

test("a redirect is UNKNOWN -- the answer is /setup's own status", async () => {
  // A host that redirects /setup somewhere friendly (a login page, an SSO front
  // door) would otherwise be read as whatever that destination answers. The
  // wizard neither rendered nor sealed, so nothing was established.
  for (const status of [301, 302, 307, 308]) {
    const got = await probeClaimState("https://identity.memql.localhost", {
      fetch: fetchReturning(status),
    });
    assert.equal(got, "unknown", `status ${status} should not establish ownership`);
  }
});

test("a server error is UNKNOWN", async () => {
  const got = await probeClaimState("https://identity.memql.localhost", {
    fetch: fetchReturning(500),
  });
  assert.equal(got, "unknown" satisfies ClaimState);
});

test("the probe does not follow redirects", async () => {
  let sawRedirectMode = "";
  const fetch: FetchLike = async (_url, init) => {
    sawRedirectMode = init?.redirect ?? "";
    return { status: 404 };
  };
  await probeClaimState("https://identity.memql.localhost", { fetch });
  assert.equal(
    sawRedirectMode,
    "manual",
    "following a redirect would read the destination's status as this cluster's ownership",
  );
});

test("a local cluster's own TLS is what the probe cannot get past", async () => {
  // NOT A HYPOTHETICAL. `globalThis.fetch` verifies against Node's bundled
  // roots, and the local installer puts its mkcert CA in the OS/NSS store, so
  // a real probe of a real local cluster throws exactly this:
  //
  //   fetch failed / cause: UNABLE_TO_VERIFY_LEAF_SIGNATURE
  //
  // Reporting it as `unknown` is right -- nothing about ownership was
  // established -- but it means this module cannot answer for the clusters the
  // installer builds, which is why clusters/ownershipRoute.ts asks the install
  // receipt before it asks the network.
  const tlsFailure: FetchLike = async () => {
    const err = new TypeError("fetch failed");
    (err as { cause?: unknown }).cause = { code: "UNABLE_TO_VERIFY_LEAF_SIGNATURE" };
    throw err;
  };
  assert.equal(await probeClaimState("https://identity.memql.localhost", { fetch: tlsFailure }), "unknown");
});

test("setupUrl composes the wizard from the issuer", () => {
  assert.equal(
    setupUrl("https://identity.memql.localhost"),
    "https://identity.memql.localhost/setup",
  );
  // A trailing slash must not produce a double slash -- the path is compared by
  // the router, not normalised by it.
  assert.equal(
    setupUrl("https://identity.memql.localhost/"),
    "https://identity.memql.localhost/setup",
  );
});

test("a missing or non-absolute issuer yields no URL, and no probe", async () => {
  // There is nothing to probe and nothing to open. Guessing a host from the
  // gRPC endpoint would be inventing an address rather than reading one.
  for (const issuer of [undefined, "", "   ", "identity.memql.localhost", "ftp://x/y"]) {
    assert.equal(setupUrl(issuer), "", `issuer ${String(issuer)} should compose no URL`);
  }
  let called = false;
  const fetch: FetchLike = async () => {
    called = true;
    return { status: 200 };
  };
  const got = await probeClaimState(undefined, { fetch });
  assert.equal(got, "unknown");
  assert.equal(called, false, "a cluster with no issuer must not be probed at a guessed address");
});
