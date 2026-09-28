// What a cluster's row says and which menus it opens (src/clusters/status.ts).
//
// ONE STATE MACHINE for the row, the page and the menus. The words are the
// person's ("Connected", "Sign in", "Not running", "Can't reach", "Not set
// up"), the version appears only when it tells something, and the contextValue
// carries the flags the menus are gated on.

import test from "node:test";
import assert from "node:assert/strict";

import { factsFrom, type ClusterFacts } from "../src/clusters/facts.js";
import type { ClusterConfig } from "../src/clusters/model.js";
import {
  afterConnect,
  clusterContextValue,
  clusterRowText,
  clusterStatus,
  retriesEndedNotice,
  rowClickAction,
  stateWord,
  versionNote,
} from "../src/clusters/status.js";
import type { ConnectionState } from "../src/connection/manager.js";

const SESSION: ClusterFacts = { session: true, signedIn: true, ownerSetup: false, consoleUrl: "https://os.memql.localhost/" };
const NOTHING: ClusterFacts = { session: false, signedIn: false, ownerSetup: false, consoleUrl: "https://os.memql.localhost/" };
const DISCONNECTED: ConnectionState = { status: "disconnected" };

function cluster(over: Partial<ClusterConfig> = {}): ClusterConfig {
  return { name: "local", displayName: "memql.localhost", endpoint: "api.memql.localhost:443", domain: "memql.localhost", ...over };
}

function error(reason: ConnectionState extends infer S ? (S extends { reason: infer R } ? R : never) : never, retrying?: boolean): ConnectionState {
  return { status: "error", clusterName: "local", reason, message: "detail for the output", ...(retrying ? { retrying } : {}) };
}

test("the live connection decides the state of the cluster it names", () => {
  const c = cluster();
  assert.equal(clusterStatus({ cluster: c, connection: { status: "connected", clusterName: "local", nodeId: "n" }, facts: NOTHING }).state, "connected");
  assert.equal(clusterStatus({ cluster: c, connection: { status: "connecting", clusterName: "local" }, facts: NOTHING }).state, "connecting");
  for (const [reason, why] of [
    ["missingCredential", "missing"],
    ["credentialExpired", "expired"],
    ["reauthenticationRequired", "refused"],
    ["wrongTokenClass", "wrongToken"],
  ] as const) {
    assert.deepEqual(clusterStatus({ cluster: c, connection: error(reason), facts: SESSION }), { state: "signIn", signInReason: why });
  }
  assert.equal(clusterStatus({ cluster: c, connection: error("notConfigured"), facts: SESSION }).state, "notConfigured");
  assert.deepEqual(clusterStatus({ cluster: c, connection: error("unreachable"), facts: SESSION }), { state: "unreachable", lost: false });
  assert.deepEqual(clusterStatus({ cluster: c, connection: error("lost"), facts: SESSION }), { state: "unreachable", lost: true });
});

test("a dropped connection being retried reads as connecting, not as an outage", () => {
  assert.equal(clusterStatus({ cluster: cluster(), connection: error("lost", true), facts: SESSION }).state, "connecting");
});

test("a connection to ANOTHER cluster says nothing about this one", () => {
  const other: ConnectionState = { status: "error", clusterName: "staging", reason: "unreachable", message: "x" };
  assert.equal(clusterStatus({ cluster: cluster(), connection: other, facts: SESSION }).state, "idle");
});

test("at rest, Sign in is claimed only when nothing stored could renew silently", () => {
  // THE BUG: the file had no token, the refresh token was in SecretStorage,
  // and the row said "needs sign-in" for a cluster a click would connect.
  const facts = factsFrom(cluster(), { secretRefresh: "RT", receipt: null, signedInBefore: false, nowMs: Date.now() });
  assert.equal(clusterStatus({ cluster: cluster(), connection: DISCONNECTED, facts }).state, "idle");
  assert.deepEqual(clusterStatus({ cluster: cluster(), connection: DISCONNECTED, facts: NOTHING }), {
    state: "signIn",
    signInReason: "missing",
  });
  assert.deepEqual(
    clusterStatus({ cluster: cluster(), connection: DISCONNECTED, facts: { ...NOTHING, signedIn: true } }),
    { state: "signIn", signInReason: "expired" },
  );
});

test("at rest, no address is Not set up and a personal access token is Sign in", () => {
  assert.equal(clusterStatus({ cluster: cluster({ endpoint: "" }), connection: DISCONNECTED, facts: SESSION }).state, "notConfigured");
  assert.deepEqual(clusterStatus({ cluster: cluster({ token: "mql_pat_x" }), connection: DISCONNECTED, facts: SESSION }), {
    state: "signIn",
    signInReason: "wrongToken",
  });
});

test("the state words are the person's, and local and remote outages differ", () => {
  const local = cluster({ local: true });
  assert.equal(stateWord({ state: "connected" }, local, false), "Connected");
  assert.equal(stateWord({ state: "connecting" }, local, false), "Connecting");
  assert.equal(stateWord({ state: "signIn" }, local, false), "Sign in");
  assert.equal(stateWord({ state: "unreachable" }, local, false), "Not running");
  assert.equal(stateWord({ state: "unreachable" }, cluster(), false), "Can't reach");
  assert.equal(stateWord({ state: "notConfigured" }, local, false), "Not set up");
  assert.equal(stateWord({ state: "idle" }, local, false), "", "an idle cluster not in use has nothing to say");
  assert.equal(stateWord({ state: "idle" }, local, true), "Not connected", "the cluster in use is marked");
});

test("the version is on the row only when it tells something", () => {
  const listing = { tags: ["v0.21.0", "v0.19.1"], fetchedAt: 1 };
  assert.equal(versionNote(cluster({ version: "v0.19.1" }), listing), "v0.21.0 available");
  assert.equal(versionNote(cluster({ version: "v0.21.0" }), listing), "");
  assert.equal(versionNote(cluster({ version: "main" }), listing), "", "a branch cannot be compared");
  assert.equal(versionNote(cluster({ version: "v0.18.0" }), undefined), "Needs an update", "older than this extension expects");
  assert.equal(versionNote(cluster(), listing), "");
});

test("the row text: state and version joined by a middle dot, a short tooltip, a spoken label", () => {
  const listing = { tags: ["v0.21.0"], fetchedAt: 1 };
  const text = clusterRowText(cluster({ version: "v0.19.1" }), { state: "connected" }, true, listing);
  assert.equal(text.description, "Connected · v0.21.0 available");
  assert.deepEqual(text.tooltip.split("\n"), ["Connected.", "api.memql.localhost", "Version v0.19.1 · v0.21.0 available"]);
  assert.equal(text.accessibilityLabel, "memql.localhost, Connected, in use");
});

test("no tooltip shouts a prefix, names an internal field, or points at a channel", () => {
  for (const status of [
    { state: "signIn", signInReason: "missing" },
    { state: "signIn", signInReason: "expired" },
    { state: "signIn", signInReason: "wrongToken" },
    { state: "unreachable", lost: true },
    { state: "unreachable", lost: false },
    { state: "notConfigured" },
    { state: "idle" },
  ] as const) {
    const text = clusterRowText(cluster(), status, false, undefined);
    assert.doesNotMatch(text.tooltip, /CREDENTIAL|ERROR:|Endpoint:|token`|refresh_token|output channel|--/, text.tooltip);
    assert.ok(text.tooltip.split("\n").length <= 3, text.tooltip);
  }
});

test("the contextValue carries the state and the flags the menus need", () => {
  const facts: ClusterFacts = { session: false, signedIn: false, ownerSetup: true, consoleUrl: "https://os.memql.localhost/" };
  assert.equal(
    clusterContextValue(cluster({ local: true }), { state: "signIn" }, facts, true),
    "memqlCluster;signIn;local;ownerSetup;os;inUse",
  );
  assert.equal(clusterContextValue(cluster(), { state: "idle" }, SESSION, false), "memqlCluster;idle;signedIn;os");
  // Never an owner-passkey offer on a cluster this editor is connected to.
  assert.equal(
    clusterContextValue(cluster({ local: true }), { state: "connected" }, { ...facts, signedIn: true }, true),
    "memqlCluster;connected;local;signedIn;os;inUse",
  );
  assert.equal(clusterContextValue(cluster(), { state: "notConfigured" }, { ...NOTHING, consoleUrl: "" }, false), "memqlCluster;notConfigured");
});

test("a dial refused on an untrusted certificate is not a stopped cluster", () => {
  const untrusted: ConnectionState = {
    status: "error",
    clusterName: "local",
    reason: "unreachable",
    message: "unable to verify the first certificate",
  };
  const local = cluster({ local: true });
  const status = clusterStatus({ cluster: local, connection: untrusted, facts: SESSION });
  assert.deepEqual(status, { state: "unreachable", lost: false, untrusted: true });
  assert.equal(stateWord(status, local, false), "Can't reach", "not \"Not running\": it answered");
  assert.equal(clusterRowText(local, status, false, undefined).tooltip.split("\n")[0], "This computer doesn't trust the cluster's certificate.");
});

test("a row click on the connected cluster opens its page instead of redialling", () => {
  // Clicking the green row used to tear the live session down and dial again,
  // dropping every session-defined construct with it.
  const c = cluster();
  assert.equal(rowClickAction(c, { status: "connected", clusterName: "local", nodeId: "n" }, SESSION), "openPage");
  assert.equal(rowClickAction(c, { status: "connecting", clusterName: "local" }, SESSION), "openPage");
});

test("a row click on a cluster nothing can sign in opens its page, never a dial or a modal", () => {
  assert.equal(rowClickAction(cluster(), { status: "disconnected" }, NOTHING), "openPage");
  // Even while another cluster is connected: that connection is kept.
  assert.equal(
    rowClickAction(cluster(), { status: "connected", clusterName: "staging", nodeId: "n" }, NOTHING),
    "openPage",
  );
});

test("a row click on a cluster with a stored session connects", () => {
  assert.equal(rowClickAction(cluster(), { status: "disconnected" }, SESSION), "connect");
  assert.equal(
    rowClickAction(cluster(), { status: "error", clusterName: "local", reason: "unreachable", message: "x" }, SESSION),
    "connect",
    "Retry after an outage dials again",
  );
});

test("after a click's connect: credential or address problems open the page, outages get one line", () => {
  assert.equal(afterConnect("local", { status: "connected", clusterName: "local", nodeId: "n" }), "done");
  assert.equal(afterConnect("local", { status: "error", clusterName: "other", reason: "unreachable", message: "x" }), "done");
  for (const reason of ["missingCredential", "credentialExpired", "reauthenticationRequired", "wrongTokenClass", "notConfigured"] as const) {
    assert.equal(afterConnect("local", { status: "error", clusterName: "local", reason, message: "x" }), "openPage", reason);
  }
  assert.equal(afterConnect("local", { status: "error", clusterName: "local", reason: "unreachable", message: "x" }), "notice");
});

test("the end of a run of retries is announced once, with the right fix", () => {
  const retrying: ConnectionState = { status: "error", clusterName: "local", reason: "lost", message: "x", retrying: true };
  const gaveUp: ConnectionState = { status: "error", clusterName: "local", reason: "unreachable", message: "x" };
  const refused: ConnectionState = { status: "error", clusterName: "local", reason: "reauthenticationRequired", message: "x" };
  assert.equal(retriesEndedNotice(retrying, gaveUp), "reconnect");
  assert.equal(retriesEndedNotice(retrying, refused), "signIn");
  assert.equal(retriesEndedNotice(retrying, { status: "connected", clusterName: "local", nodeId: "n" }), undefined);
  assert.equal(retriesEndedNotice(retrying, retrying), undefined, "still retrying: nothing to say yet");
  assert.equal(retriesEndedNotice(gaveUp, gaveUp), undefined, "an ordinary failure is not the end of retries");
});
