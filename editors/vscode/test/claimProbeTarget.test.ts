import test from "node:test";
import assert from "node:assert/strict";
import { CLAIM_PROBE_TIMEOUT_MS, claimProbeSignal, readOwnerSetupState } from "../src/clusters/claimState.js";
import type { ClusterConfig } from "../src/clusters/model.js";

for (const [cluster, expected] of [
  [{ name: "test", domain: "example.com", endpoint: "api.example.com:443" }, "https://identity.example.com/auth/setup/state"],
  [{ name: "test", endpoint: "api.memql.localhost:443" }, "https://identity.memql.localhost/auth/setup/state"],
  [{ name: "test", endpoint: "api.example.com:443", issuer: "https://sso.example.com/" }, "https://sso.example.com/auth/setup/state"],
] satisfies [ClusterConfig, string][]) {
  test(`owner setup probe derives ${expected}`, async () => {
    const asked: string[] = [];
    assert.equal(await readOwnerSetupState(cluster, async url => {
      asked.push(url);
      return { status: 200, text: async () => '{"state":"unclaimed"}' };
    }), "unclaimed");
    assert.deepEqual(asked, [expected]);
  });
}
test("a cluster with no identity address is unknown without a network call", async () => {
  assert.equal(await readOwnerSetupState({ name: "test", endpoint: "[::1]:50051" }, async () => {
    assert.fail("must not guess an identity host");
  }), "unknown");
});
test("owner-state requests have a short deadline and propagate cancellation", async () => {
  assert.ok(CLAIM_PROBE_TIMEOUT_MS > 0 && CLAIM_PROBE_TIMEOUT_MS <= 10_000);
  const cancel = new AbortController();
  const signal = claimProbeSignal(cancel.signal);
  assert.equal(signal.aborted, false);
  await readOwnerSetupState({ name: "test", domain: "example.com", endpoint: "api.example.com:443" }, async (_url, init) => {
    cancel.abort();
    assert.equal(init.signal?.aborted, true);
    assert.equal(signal.aborted, true);
    throw new Error("cancelled");
  }, cancel.signal);
});
