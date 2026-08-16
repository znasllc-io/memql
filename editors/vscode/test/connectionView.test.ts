// The connection page, and where the portal is (memql#3742).
//
// This page replaced the topology view, and not because it is nicer. Topology
// -- a pod grid, orphan verdicts, under-replica alarms -- is cluster state,
// which the portal owns and already draws. What no surface answered is the
// question an operator arrives with when a cluster will not come up: WHAT IS
// THIS EDITOR DIALLING, AS WHOM, AND WHAT HAPPENED. Every assertion here is
// about one of those three, and every value is on this side of the boundary --
// clusters.yaml, SecretStorage, the live connection.
//
// The one that matters most is the distinction between "did not answer" and
// "nobody asked". Those are the two possibilities an operator is trying to tell
// apart, and a page that reported an undialled cluster as unreachable would be
// answering the question wrongly with total confidence.

import test from "node:test";
import assert from "node:assert/strict";

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { connectionView, formatExpiry } from "../src/clusters/connectionView.js";
import type { ClusterConfig } from "../src/clusters/model.js";
import { composePortalUrl, portalTarget } from "../src/clusters/portalUrl.js";
import type { ConnectionState } from "../src/connection/manager.js";

const NOW = Date.parse("2026-08-14T12:00:00Z");

function cluster(over: Partial<ClusterConfig> = {}): ClusterConfig {
  return { name: "staging", endpoint: "api.example.com:443", domain: "example.com", ...over };
}

function factOf(facts: readonly { key: string; value: string; note: string }[], key: string) {
  const found = facts.find((f) => f.key === key);
  assert.notEqual(found, undefined, `no fact named ${key}`);
  return found!;
}

const DISCONNECTED: ConnectionState = { status: "disconnected" };

// -----------------------------------------------------------------------------
// what this editor is dialling
// -----------------------------------------------------------------------------

test("an undialled cluster is not reported as unreachable", () => {
  // The distinction the page exists for. "Your cluster is down" and "nobody has
  // asked it" call for completely different next actions.
  const view = connectionView({ cluster: cluster(), state: DISCONNECTED, nowMs: NOW });
  assert.equal(factOf(view.connection, "endpoint").note, "not dialled from this editor");
});

test("a live session says so, and names the node that answered", () => {
  const view = connectionView({
    cluster: cluster(),
    state: { status: "connected", clusterName: "staging", nodeId: "bff-7f4c" },
    nowMs: NOW,
  });
  assert.match(factOf(view.connection, "endpoint").note, /reachable \(node bff-7f4c\)/);
});

test("a failure carries the engine's own message rather than a category", () => {
  const view = connectionView({
    cluster: cluster(),
    state: {
      status: "error",
      clusterName: "staging",
      message: "certificate has expired",
      reason: "unreachable",
    },
    nowMs: NOW,
  });
  assert.match(factOf(view.connection, "endpoint").note, /did not answer: certificate has expired/);
});

test("a session against ANOTHER cluster says nothing about this one", () => {
  const view = connectionView({
    cluster: cluster(),
    state: { status: "connected", clusterName: "prod", nodeId: "bff-1" },
    nowMs: NOW,
  });
  assert.equal(factOf(view.connection, "endpoint").note, "not dialled from this editor");
});

test("the issuer is DERIVED when the entry carries none, and says so", () => {
  // Showing the derived value is the point: it is what a refresh will actually
  // POST to, and the operator is here because something in that chain failed.
  const view = connectionView({ cluster: cluster(), state: DISCONNECTED, nowMs: NOW });
  const issuer = factOf(view.connection, "issuer");
  assert.equal(issuer.value, "https://identity.example.com");
  assert.equal(issuer.note, "derived from the domain");

  const explicit = connectionView({
    cluster: cluster({ issuer: "https://auth.example.com/" }),
    state: DISCONNECTED,
    nowMs: NOW,
  });
  assert.equal(factOf(explicit.connection, "issuer").value, "https://auth.example.com");
  assert.equal(factOf(explicit.connection, "issuer").note, "");
});

test("an unconfigured endpoint is named, not blank", () => {
  const view = connectionView({
    cluster: cluster({ endpoint: "" }),
    state: DISCONNECTED,
    nowMs: NOW,
  });
  assert.equal(factOf(view.connection, "endpoint").value, "not configured");
});

test("a local cluster is told where its uninstall lives", () => {
  const view = connectionView({
    cluster: cluster({ local: true }),
    state: DISCONNECTED,
    nowMs: NOW,
  });
  assert.match(factOf(view.connection, "local").note, /Deployments/);
  // And a remote one is not: there is nothing on this machine to uninstall.
  const remote = connectionView({ cluster: cluster(), state: DISCONNECTED, nowMs: NOW });
  assert.equal(factOf(remote.connection, "local").note, "");
});

// -----------------------------------------------------------------------------
// as whom
// -----------------------------------------------------------------------------

test("the identity is what the CONNECTION produced, not what the file claims", () => {
  const view = connectionView({
    cluster: cluster(),
    state: { status: "connected", clusterName: "staging", nodeId: "n" },
    identity: { email: "ada@example.com", role: "owner" },
    nowMs: NOW,
  });
  assert.equal(factOf(view.identity, "signed in as").value, "ada@example.com");
  assert.equal(factOf(view.identity, "role").value, "owner");
});

test("connected with no identity is its own answer, not 'not signed in'", () => {
  // A session the cluster accepted whose access read produced nothing is a
  // different fault from having no credential at all.
  const view = connectionView({
    cluster: cluster(),
    state: { status: "connected", clusterName: "staging", nodeId: "n" },
    nowMs: NOW,
  });
  assert.match(factOf(view.identity, "signed in as").value, /access read produced no identity/);
});

test("a token expiry is a duration, and says it renews itself", () => {
  const view = connectionView({
    cluster: cluster({ token: "eyJhbGciOi.body.sig" }),
    state: DISCONNECTED,
    expiresAtEpochSeconds: Math.floor(NOW / 1000) + 11 * 60,
    nowMs: NOW,
  });
  const token = factOf(view.identity, "token");
  assert.equal(token.value, "expires in 11m");
  // Without this an operator reads a countdown to being logged out. Identity
  // issues 900-second access tokens BY DESIGN.
  assert.match(token.note, /renews automatically/);
});

test("a wrong-class credential is named rather than described", () => {
  // A PAT here fails exactly like a forged token, and it is the one credential
  // problem an operator cannot diagnose from the failure (memql#3383).
  const view = connectionView({
    cluster: cluster({ token: "mql_pat_abcdefghijklmnopqrstuvwxyz" }),
    state: DISCONNECTED,
    nowMs: NOW,
  });
  assert.match(factOf(view.identity, "token").value, /personal access token/);
  assert.match(factOf(view.identity, "token").value, /cannot verify/);
});

test("no stored token, and a stored one with no expiry, read differently", () => {
  const none = connectionView({ cluster: cluster(), state: DISCONNECTED, nowMs: NOW });
  assert.equal(factOf(none.identity, "token").value, "none stored");

  const noExpiry = connectionView({
    cluster: cluster({ token: "eyJhbGciOi.body.sig" }),
    state: DISCONNECTED,
    nowMs: NOW,
  });
  assert.equal(factOf(noExpiry.identity, "token").value, "stored, with no recorded expiry");
});

test("the duration is coarse, and an expired token says so rather than counting down", () => {
  const nowSeconds = Math.floor(NOW / 1000);
  assert.equal(formatExpiry(nowSeconds + 30, NOW), "in under a minute");
  assert.equal(formatExpiry(nowSeconds + 11 * 60, NOW), "in 11m");
  assert.equal(formatExpiry(nowSeconds + 3 * 3600, NOW), "in 3h");
  assert.equal(formatExpiry(nowSeconds + 2 * 86_400, NOW), "in 2d");
  // An expired token and one with four seconds left ask for different things,
  // and neither is a negative number.
  assert.match(formatExpiry(nowSeconds - 60, NOW), /already/);
});

// -----------------------------------------------------------------------------
// where the portal is
// -----------------------------------------------------------------------------

function siteRow(payload: Record<string, unknown>): Row {
  return { id: `v1:platform:site:${String(payload["hostname"])}`, concept: "v1:platform:site", payload };
}

test("the cluster's OWN site row outranks the composed host", () => {
  // Reading the row is what keeps this correct across a move like memql#3711.
  // It is not sufficient on its own, though -- see the composition tests below,
  // which are what a first run actually reaches.
  const target = portalTarget(cluster(), [
    siteRow({ hostname: "shop.example.com", systemOwned: false }),
    siteRow({ hostname: "console.example.com", systemOwned: true }),
  ]);
  assert.equal(target.url, "https://console.example.com/");
  assert.equal(target.fromSiteRow, true);
});

test("systemOwned is what identifies it, not a name", () => {
  // Matching on a name would pick a customer's site the day somebody calls one
  // "portal".
  const target = portalTarget(cluster(), [siteRow({ hostname: "portal.example.com", systemOwned: false })]);
  assert.equal(target.fromSiteRow, false);
  assert.equal(target.url, "https://portal.example.com/");
});

test("the composed portal is its OWN origin, not a path on the api front door", () => {
  // memql#3711 moved the portal to site #1 at portal.<domain>; this composition
  // did not follow until memql#3906. The old address does not 404 -- `/portal/`
  // has no Ingress rule of its own, so it falls through to the `/` h2c
  // catch-all and answers 415, an HTTP/1.1 request handed to a gRPC backend.
  const target = portalTarget(cluster(), []);
  assert.equal(target.url, "https://portal.example.com/");
  assert.equal(target.fromSiteRow, false);
  assert.doesNotMatch(target.url, /\/portal\/$/, "the sub-path form is what 415s");
});

test("an api.<domain> endpoint implies its portal sibling", () => {
  assert.equal(
    composePortalUrl({ name: "x", endpoint: "api.lab.example.com:50051" }),
    // Same derivation identityBaseUrlFor makes for identity.<domain>: the
    // endpoint IS the api front door, so stripping that label names the domain.
    // The port is dropped -- the portal is served over 443 whatever the gRPC
    // endpoint names.
    "https://portal.lab.example.com/",
  );
  // The shared endpoint parser, not a second `split(":")`: a scheme-prefixed
  // endpoint used to compose `https://https/portal/`.
  assert.equal(
    composePortalUrl({ name: "x", endpoint: "https://api.lab.example.com:443" }),
    "https://portal.lab.example.com/",
  );
});

test("an endpoint that names no api front door composes nothing", () => {
  // The portal is a DIFFERENT host from the gRPC endpoint now, so there is no
  // host left to reuse. Opening the endpoint's own hostname would point a
  // browser at an address nobody nominated.
  assert.equal(composePortalUrl({ name: "x", endpoint: "grpc.lab.example.com:50051" }), "");
  assert.equal(composePortalUrl({ name: "x", endpoint: "[::1]:50051" }), "");
});

test("nothing to compose from yields nothing, not https:///", () => {
  assert.equal(composePortalUrl({ name: "x", endpoint: "" }), "");
  assert.equal(portalTarget({ name: "x", endpoint: "" }, []).url, "");
});

// -----------------------------------------------------------------------------
// The recorded version (memql#3995)
// -----------------------------------------------------------------------------
//
// The page says the version WORD-FOR-WORD where the tree row can only hint at
// it. In particular it says "unknown" out loud: on the row that would be noise
// on every cluster, and here it is the answer to a question the operator asked
// by opening the page.

const RELEASES = { tags: ["v0.18.0", "v0.17.0"], fetchedAt: 1000 };

test("the page always carries a version fact, even with nothing recorded", () => {
  const view = connectionView({
    cluster: cluster(),
    state: DISCONNECTED,
    nowMs: NOW,
    listing: RELEASES,
  });
  const fact = factOf(view.connection, "version");
  assert.match(fact.value, /unknown/i);
});

test("a recorded version behind the newest release offers the upgrade", () => {
  const view = connectionView({
    cluster: cluster({ version: "v0.17.0" }),
    state: DISCONNECTED,
    nowMs: NOW,
    listing: RELEASES,
  });
  const fact = factOf(view.connection, "version");
  assert.equal(fact.value, "v0.17.0");
  assert.match(fact.note, /v0\.18\.0/, "the note names the release that is available");
});

test("a current cluster's note says so without offering anything", () => {
  const view = connectionView({
    cluster: cluster({ version: "v0.18.0" }),
    state: DISCONNECTED,
    nowMs: NOW,
    listing: RELEASES,
  });
  const fact = factOf(view.connection, "version");
  assert.equal(fact.value, "v0.18.0");
  assert.match(fact.note, /newest release/i);
});

test("a DISCONNECTED cluster reports its version -- no session required", () => {
  // The property this whole surface exists for: the motivating incident
  // happened on a session that had just been severed.
  const view = connectionView({
    cluster: cluster({ version: "v0.17.0" }),
    state: DISCONNECTED,
    nowMs: NOW,
    listing: RELEASES,
  });
  assert.equal(factOf(view.connection, "version").value, "v0.17.0");
});

test("an unfetched listing leaves the version shown and claims nothing", () => {
  const view = connectionView({
    cluster: cluster({ version: "v0.17.0" }),
    state: DISCONNECTED,
    nowMs: NOW,
    listing: undefined,
  });
  const fact = factOf(view.connection, "version");
  assert.equal(fact.value, "v0.17.0");
  assert.doesNotMatch(fact.note, /available/i);
  assert.doesNotMatch(fact.note, /newest release/i);
});

test("a version that is not a release is shown verbatim and not compared", () => {
  const view = connectionView({
    cluster: cluster({ version: "0.15.0-1737072000" }),
    state: DISCONNECTED,
    nowMs: NOW,
    listing: RELEASES,
  });
  const fact = factOf(view.connection, "version");
  assert.equal(fact.value, "0.15.0-1737072000");
  assert.match(fact.note, /does not name a release/i);
});

test("the page works with no listing supplied at all", () => {
  // Activation does no network work, so the first render of this page happens
  // before anything has been fetched.
  const view = connectionView({ cluster: cluster({ version: "v0.17.0" }), state: DISCONNECTED, nowMs: NOW });
  assert.equal(factOf(view.connection, "version").value, "v0.17.0");
});
