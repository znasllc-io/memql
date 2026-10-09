import assert from "node:assert/strict";
import test from "node:test";
import { readOwnerSetupState } from "../src/clusters/claimState.js";

const cluster = { name: "local", domain: "memql.localhost", endpoint: "api.memql.localhost:443" };
for (const state of ["unclaimed", "claimed"] as const) {
  test(`the explicit setup state reports ${state}`, async () => {
    assert.equal(await readOwnerSetupState(cluster, async (url, init) => {
      assert.equal(url, "https://identity.memql.localhost/auth/setup/state");
      assert.equal(init.redirect, "manual");
      assert.equal(new Headers(init.headers).get("accept"), "application/json");
      return { status: 200, text: async () => JSON.stringify({ state }) };
    }), state);
  });
}
test("status codes and malformed bodies cannot establish ownership", async () => {
  for (const status of [301, 302, 307, 308, 404, 500]) {
    assert.equal(await readOwnerSetupState(cluster, async () => ({ status, text: async () => '{"state":"claimed"}' })), "unknown");
  }
  for (const body of ["<html>setup</html>", "{}", "null", '{"state":"other"}']) {
    assert.equal(await readOwnerSetupState(cluster, async () => ({ status: 200, text: async () => body })), "unknown");
  }
});
test("TLS and network failures remain unknown", async () => {
  assert.equal(await readOwnerSetupState(cluster, async () => { throw new Error("unable to verify first certificate"); }), "unknown");
});
