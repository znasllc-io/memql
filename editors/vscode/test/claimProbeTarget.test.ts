// Which field the entry point feeds the claim probe (memql#4620).
//
// THE PURE FUNCTIONS WERE ALREADY TESTED AND THE FEATURE WAS STILL DEAD.
// claimState.test.ts drives `setupUrl` and `probeClaimState` with an issuer and
// they answer correctly. `src/extension.ts` passed them `cluster.issuer`, and
// NOTHING in this extension ever writes that field: the connect form's
// registration shape has no such key (state/addCluster.ts) and the install path
// omits it deliberately and says why (install/handoff.ts). So the argument was
// `undefined` on every cluster the editor can produce, `setupUrl` returned "",
// `probeClaimState` short-circuited to `unknown`, and `routeForClaimState` maps
// `unknown` to sign-in -- the `claim` branch, which is the whole of memql#3885,
// could not be reached in the shipped product.
//
// An operator connecting to a genuinely unclaimed remote cluster was therefore
// sent to authenticate against an account that does not exist: the exact dead
// end #3885 exists to prevent, with the fix present in the tree and unreachable.
//
// So these cases are about the ARGUMENT, not the answer. Every one of them
// passes a cluster carrying NO `issuer`, because that is every cluster.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import {
  CLAIM_PROBE_TIMEOUT_MS,
  claimProbeSignal,
  probeClaimStateForCluster,
  setupUrlForCluster,
  type ClaimState,
  type FetchLike,
} from "../src/clusters/claimState.js";
import type { ClusterConfig } from "../src/clusters/model.js";

function cluster(overrides: Partial<ClusterConfig> = {}): ClusterConfig {
  return { name: "staging", endpoint: "api.example.com:443", ...overrides };
}

// -----------------------------------------------------------------------------
// the wizard URL
// -----------------------------------------------------------------------------

test("a cluster registered with only a domain still names a wizard URL", () => {
  // THE REGRESSION, stated as one assertion. This is what the connect form
  // writes -- name, endpoint, domain, and no issuer -- and it used to compose "".
  const got = setupUrlForCluster(cluster({ domain: "example.com" }));
  assert.equal(got, "https://identity.example.com/setup");
});

test("an installed cluster names one too, from the endpoint alone", () => {
  // installedClusterEntry writes name/domain/endpoint/local and no issuer. Even
  // stripped of the domain, `api.<host>` implies its `identity.` sibling.
  const got = setupUrlForCluster(cluster({ endpoint: "api.memql.localhost:443" }));
  assert.equal(got, "https://identity.memql.localhost/setup");
});

test("an explicit issuer still wins, and its trailing slash does not double up", () => {
  // The one field that IS authoritative when present: what the cockpit writes
  // from the cluster's discovery document, for an identity service that is not
  // at identity.<domain>.
  const got = setupUrlForCluster(
    cluster({ domain: "example.com", issuer: "https://sso.example.com/" }),
  );
  assert.equal(got, "https://sso.example.com/setup");
});

test('a cluster naming no identity service composes "", not a guess', () => {
  // An IP-literal endpoint names no DNS sibling and there is no domain, so
  // there is nothing to probe and nothing to open. "" keeps setupUrl's contract.
  assert.equal(setupUrlForCluster(cluster({ endpoint: "[::1]:50051" })), "");
});

// -----------------------------------------------------------------------------
// the probe
// -----------------------------------------------------------------------------

test("the probe reaches a domain-only cluster's /setup and reads its answer", async () => {
  const asked: string[] = [];
  const fetchOk: FetchLike = async (url) => {
    asked.push(url);
    return { status: 200 };
  };
  const got = await probeClaimStateForCluster(cluster({ domain: "example.com" }), {
    fetch: fetchOk,
  });
  assert.equal(got, "unclaimed" satisfies ClaimState);
  assert.deepEqual(
    asked,
    ["https://identity.example.com/setup"],
    "the probe must ask the identity service the cluster names, not the field nothing writes",
  );
});

test("a cluster naming no identity service is unknown WITHOUT a dial", async () => {
  // `unknown` was the old answer for every cluster; it must survive as the
  // answer for the one case that genuinely cannot be asked -- and asking a
  // composed-from-nothing URL would be inventing an address.
  let dialled = 0;
  const got = await probeClaimStateForCluster(cluster({ endpoint: "[::1]:50051" }), {
    fetch: async () => {
      dialled += 1;
      return { status: 200 };
    },
  });
  assert.equal(got, "unknown" satisfies ClaimState);
  assert.equal(dialled, 0);
});

test("the caller's signal reaches the fetch", async () => {
  // The probe accepted a `signal` from the first commit and the entry point
  // passed none, which is the other half of #4620: with no signal Node's fetch
  // binds undici's 300-second headers timeout.
  const signal = claimProbeSignal();
  let seen: AbortSignal | undefined;
  await probeClaimStateForCluster(cluster({ domain: "example.com" }), {
    fetch: async (_url, init) => {
      seen = init?.signal;
      return { status: 404 };
    },
    signal,
  });
  assert.equal(seen, signal);
});

// -----------------------------------------------------------------------------
// the deadline
// -----------------------------------------------------------------------------

test("the probe's deadline is far under undici's default headers timeout", () => {
  // The number that matters is not 5000 exactly; it is that a sign-in cannot be
  // held up for anything an operator would read as a hang. undici's default is
  // 300_000, which is what the entry point inherited by passing no signal.
  assert.ok(CLAIM_PROBE_TIMEOUT_MS > 0);
  assert.ok(
    CLAIM_PROBE_TIMEOUT_MS <= 10_000,
    `a claim probe may not hold a sign-in for ${CLAIM_PROBE_TIMEOUT_MS}ms`,
  );
});

test("a fresh deadline has not fired, and a cancelled caller aborts at once", () => {
  assert.equal(claimProbeSignal().aborted, false);

  const cancelled = new AbortController();
  cancelled.abort();
  assert.equal(
    claimProbeSignal(cancelled.signal).aborted,
    true,
    "the progress notification's Cancel must reach the probe, not just the deadline",
  );
});

test("cancelling the caller after the fact still aborts the probe's signal", () => {
  const cancel = new AbortController();
  const signal = claimProbeSignal(cancel.signal);
  assert.equal(signal.aborted, false);
  cancel.abort();
  assert.equal(signal.aborted, true);
});

// -----------------------------------------------------------------------------
// and the entry point cannot reach the raw pair again
// -----------------------------------------------------------------------------
//
// SCANNED FROM SOURCE, for the reason authWiring.test.ts states: the defect was
// never a wrong answer. Both functions below are correct and covered; what was
// wrong was which value one call site handed them, and `src/extension.ts`
// imports `vscode`, so no behavioural test under `node --test` can reach it.
//
// The cluster-taking wrappers make the argument a TYPE rather than a choice.
// This is the second wall: re-importing the issuer-taking pair into the adapter
// is how the choice comes back.

test("src/extension.ts calls the cluster-taking claim helpers, never the raw pair", () => {
  const source = fs.readFileSync(
    path.resolve(__dirname, "..", "..", "src", "extension.ts"),
    "utf8",
  );
  for (const raw of ["setupUrl", "probeClaimState"]) {
    // `\(` after the name, so `setupUrlForCluster(` / `probeClaimStateForCluster(`
    // do not match: it is the ISSUER-taking spelling being ruled out.
    assert.equal(
      new RegExp(`\\b${raw}\\s*\\(`).test(source),
      false,
      `src/extension.ts calls ${raw}(...) directly. It takes an ISSUER, and nothing ` +
        `in this extension writes cluster.issuer -- that is memql#4620. Call ` +
        `${raw}ForCluster instead, which derives the identity service from the cluster.`,
    );
  }
});
