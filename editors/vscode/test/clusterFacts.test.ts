// What this machine knows about a cluster's sign-in (src/clusters/facts.ts).
//
// THE FIELD FAILURE. A row said "needs sign-in" -- and a click raised a sign-in
// or a passkey modal -- for a cluster whose refresh token sat in SecretStorage,
// because the decision read clusters.yaml alone. The facts read both places.

import test from "node:test";
import assert from "node:assert/strict";

import { factsFrom, gatherClusterFacts, signedInKey } from "../src/clusters/facts.js";
import type { ClusterConfig } from "../src/clusters/model.js";
import type { Receipt } from "../src/install/receipt.js";

const NOW_MS = 1_800_000_000_000;

function jwt(expSeconds: number): string {
  const b64 = (v: unknown): string => Buffer.from(JSON.stringify(v)).toString("base64url");
  return `${b64({ alg: "RS256" })}.${b64({ sub: "u", exp: expSeconds })}.sig`;
}

function cluster(over: Partial<ClusterConfig> = {}): ClusterConfig {
  return { name: "local", endpoint: "api.memql.localhost:443", domain: "memql.localhost", ...over };
}

function receipt(ownerEmail: string, domain = "memql.localhost"): Receipt {
  return {
    version: 1,
    graph: "install",
    startedAt: "",
    updatedAt: "",
    entries: [
      {
        stepId: "seedBootstrap",
        script: "install.seedBootstrap",
        receipt: "bootstrap",
        preExisting: false,
        params: { "owner-email": ownerEmail, domain },
        result: {},
        changed: true,
        recordedAt: "",
      },
    ],
  };
}

const none = { secretRefresh: "", receipt: null, signedInBefore: false, nowMs: NOW_MS };

test("a refresh token in SecretStorage is a session, whatever the file says", () => {
  const facts = factsFrom(cluster(), { ...none, secretRefresh: "RT" });
  assert.equal(facts.session, true, "the row would send the person through a browser for nothing");
  assert.equal(facts.signedIn, true);
});

test("a refresh token in the file, or a live access token, is a session too", () => {
  assert.equal(factsFrom(cluster({ refreshToken: "RT" }), none).session, true);
  assert.equal(factsFrom(cluster({ token: jwt(NOW_MS / 1000 + 600) }), none).session, true);
});

test("an expired access token with nothing to renew it is not a session, but is signed in", () => {
  const facts = factsFrom(cluster({ token: jwt(NOW_MS / 1000 - 60) }), none);
  assert.equal(facts.session, false);
  assert.equal(facts.signedIn, true, "Sign out still has something to end");
});

test("nothing stored anywhere is neither", () => {
  const facts = factsFrom(cluster(), none);
  assert.equal(facts.session, false);
  assert.equal(facts.signedIn, false);
});

test("a personal access token is never a session, even beside a refresh token", () => {
  assert.equal(factsFrom(cluster({ token: "mql_pat_x" }), { ...none, secretRefresh: "RT" }).session, false);
});

test("the owner passkey is offered only on this machine's first run", () => {
  const local = cluster({ local: true });
  assert.equal(factsFrom(local, { ...none, receipt: receipt("ada@example.com") }).ownerSetup, true);
  // The owner's own `make up` cluster: no receipt.
  assert.equal(factsFrom(local, none).ownerSetup, false);
  // Signed in here before, or a credential stored now.
  assert.equal(factsFrom(local, { ...none, receipt: receipt("ada@example.com"), signedInBefore: true }).ownerSetup, false);
  assert.equal(factsFrom(local, { ...none, receipt: receipt("ada@example.com"), secretRefresh: "RT" }).ownerSetup, false);
  // A receipt about another cluster names another cluster's owner.
  assert.equal(factsFrom(local, { ...none, receipt: receipt("ada@example.com", "lab.example.com") }).ownerSetup, false);
  // A remote cluster has no pod here to mint in.
  assert.equal(factsFrom(cluster(), { ...none, receipt: receipt("ada@example.com") }).ownerSetup, false);
});

test("MemQL OS is composed from the domain, and absent without one", () => {
  assert.equal(factsFrom(cluster(), none).consoleUrl, "https://os.memql.localhost/");
  assert.equal(factsFrom(cluster({ domain: undefined, endpoint: "10.0.0.4:443" }), none).consoleUrl, "");
});

test("gathering reads SecretStorage and, for a local cluster only, the receipt", async () => {
  let receiptReads = 0;
  const deps = {
    readRefreshToken: async (name: string) => (name === "local" ? "RT" : undefined),
    readReceipt: async () => {
      receiptReads += 1;
      return receipt("ada@example.com");
    },
    signedInBefore: () => false,
    now: () => NOW_MS,
  };
  assert.equal((await gatherClusterFacts(cluster(), deps)).session, true);
  assert.equal(receiptReads, 0, "a remote cluster has no receipt to read");
  await gatherClusterFacts(cluster({ local: true }), deps);
  assert.equal(receiptReads, 1);
});

test("a source that fails reads as nothing there, never as a rejection", async () => {
  const facts = await gatherClusterFacts(cluster({ local: true }), {
    readRefreshToken: async () => {
      throw new Error("keyring locked");
    },
    readReceipt: async () => {
      throw new Error("unreadable");
    },
    signedInBefore: () => false,
    now: () => NOW_MS,
  });
  assert.equal(facts.session, false);
  assert.equal(facts.ownerSetup, false);
});

test("a completed sign-in is remembered by slot and domain", () => {
  assert.equal(signedInKey(cluster()), "local|memql.localhost");
  assert.equal(signedInKey(cluster({ domain: " MemQL.localhost " })), "local|memql.localhost");
});
