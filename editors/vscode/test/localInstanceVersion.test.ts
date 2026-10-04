// The local instance's version when there is no install receipt.
//
// A cluster built with `make up` has no receipt, and the Deployments view said
// "unknown" for it although clusters.yaml records the version the cluster
// reported. The registered entry's version is the fallback.

import test from "node:test";
import assert from "node:assert/strict";

import { localInstance } from "../src/state/deployments.js";

test("with no receipt, the registered entry's recorded version is used", () => {
  const instance = localInstance({
    presence: "installed-healthy",
    receipt: null,
    registered: { name: "local", domain: "memql.localhost", version: "main" },
    connected: false,
  });
  assert.equal(instance.version, "main");
});

test("with neither, there is no version to claim", () => {
  const instance = localInstance({ presence: "installed-healthy", receipt: null, connected: false });
  assert.equal(instance.version, undefined);
});
