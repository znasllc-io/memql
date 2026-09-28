// The cluster page (src/clusters/connectionView.ts): one cluster's state and
// the acts legal from it, on the kit's anatomy.
//
// THE FIELD FAILURE this pins. The old page said "CREDENTIAL: ... has no
// credential", "did not answer" and "connected, but the access read produced
// no identity" at the same moment, for a cluster nothing had dialled, and drew
// all five buttons in every state. Now the action bar carries the one state,
// "Signed in as" appears only while connected, and an illegal act is absent.

import test from "node:test";
import assert from "node:assert/strict";

import { clusterPage, versionMeta, type ClusterPageInput } from "../src/clusters/connectionView.js";
import type { ClusterFacts } from "../src/clusters/facts.js";
import type { ClusterConfig } from "../src/clusters/model.js";
import type { ConnectionState } from "../src/connection/manager.js";

const SESSION: ClusterFacts = { session: true, signedIn: true, ownerSetup: false, consoleUrl: "https://os.memql.localhost/" };
const NOTHING: ClusterFacts = { session: false, signedIn: false, ownerSetup: false, consoleUrl: "https://os.memql.localhost/" };
const CONNECTED: ConnectionState = { status: "connected", clusterName: "local", nodeId: "bff-0" };

function cluster(over: Partial<ClusterConfig> = {}): ClusterConfig {
  return {
    name: "local",
    displayName: "memql.localhost",
    endpoint: "api.memql.localhost:443",
    domain: "memql.localhost",
    version: "main",
    ...over,
  };
}

function input(over: Partial<ClusterPageInput> = {}): ClusterPageInput {
  return {
    clusterName: "local",
    cluster: cluster(),
    facts: SESSION,
    connection: { status: "disconnected" },
    identity: "loading",
    consoleUrl: "https://os.memql.localhost/",
    ...over,
  };
}

/** The acts on the action bar, in order, as `act:label`. */
function bar(html: string): string[] {
  return [...html.matchAll(/data-act="([^"]+)"[^>]*>(?:<span[^>]*><\/span>)?([^<]+)<\/button>/g)].map(
    (m) => `${m[1]}:${m[2]}`,
  );
}

/** The kit escapes an apostrophe as a numeric entity; read it back as text. */
function text(html: string): string {
  return html.replace(/&#39;/g, "'").replace(/&amp;/g, "&");
}

/** The state word the bar leads with. */
function word(html: string): string {
  return text(/<span class="mq-actbar-word">([^<]*)<\/span>/.exec(html)?.[1] ?? "");
}

/** The fact labels, in order. */
function factLabels(html: string): string[] {
  return [...html.matchAll(/<dt>([^<]+)<\/dt>/g)].map((m) => m[1]);
}

test("connected: Sign out, Disconnect, and Open MemQL OS as the one button", () => {
  const page = clusterPage(input({ connection: CONNECTED, identity: { email: "ada@example.com", role: "owner" } }));
  assert.equal(word(page.actions), "Connected");
  assert.deepEqual(bar(page.actions), ["signOut:Sign out", "disconnect:Disconnect", "openConsole:Open MemQL OS"]);
  assert.match(page.actions, /data-act="openConsole"[^>]*data-tone="primary"|data-tone="primary"[^>]*data-act="openConsole"/);
  assert.deepEqual(factLabels(page.body), ["Address", "MemQL OS", "Signed in as"]);
  assert.match(page.body, /ada@example\.com · owner/);
});

test("connected with no MemQL OS address: the act is absent, not disabled", () => {
  const page = clusterPage(input({ connection: CONNECTED, consoleUrl: "", identity: { email: "a@b.c", role: "" } }));
  assert.deepEqual(bar(page.actions), ["signOut:Sign out", "disconnect:Disconnect"]);
  assert.ok(!factLabels(page.body).includes("MemQL OS"));
});

test("while the account is read, its value is a skeleton, never a sentence", () => {
  const page = clusterPage(input({ connection: CONNECTED, identity: "loading" }));
  assert.match(page.body, /class="mq-skel"/);
  assert.doesNotMatch(page.body, /Loading\.\.\.|access read/);
});

test("an account that could not be read says Unknown", () => {
  const page = clusterPage(input({ connection: CONNECTED, identity: "unavailable" }));
  assert.match(page.body, /<dt>Signed in as<\/dt><dd data-muted="true">Unknown<\/dd>/);
});

test("a cluster needing sign-in: Sign in is the one button, with a code beside it", () => {
  const page = clusterPage(input({ facts: NOTHING }));
  assert.equal(word(page.actions), "Not signed in");
  assert.deepEqual(bar(page.actions), ["signInWithCode:Sign in with a code", "signIn:Sign in"]);
});

test("THE CONTRADICTION: an error state never says who is signed in, or that the address did not answer", () => {
  // The owner's screenshot: a credential refused before any dial, and the page
  // claiming both "did not answer" and "connected, but ... no identity".
  const refused: ConnectionState = {
    status: "error",
    clusterName: "local",
    reason: "missingCredential",
    message: "Cluster \"local\" has no credential.",
  };
  const page = clusterPage(input({ connection: refused, identity: "unavailable" }));
  assert.equal(word(page.actions), "Not signed in");
  assert.ok(!factLabels(page.body).includes("Signed in as"));
  const all = page.head + page.body + page.actions;
  assert.doesNotMatch(all, /did not answer|access read|CREDENTIAL|none stored|--/);
});

test("an ended session says so, with the same two acts", () => {
  const expired: ConnectionState = { status: "error", clusterName: "local", reason: "reauthenticationRequired", message: "x" };
  const page = clusterPage(input({ connection: expired }));
  assert.equal(word(page.actions), "Your session ended");
  assert.deepEqual(bar(page.actions), ["signInWithCode:Sign in with a code", "signIn:Sign in"]);
});

test("a first run on this machine also offers the owner passkey", () => {
  const page = clusterPage(input({ cluster: cluster({ local: true }), facts: { ...NOTHING, ownerSetup: true } }));
  assert.deepEqual(bar(page.actions), [
    "takeOwnership:Create owner passkey",
    "signInWithCode:Sign in with a code",
    "signIn:Sign in",
  ]);
});

test("connecting: busy, and Cancel is the only act", () => {
  const page = clusterPage(input({ connection: { status: "connecting", clusterName: "local" } }));
  assert.equal(word(page.actions), "Connecting");
  assert.match(page.actions, /data-tone="busy"/);
  assert.deepEqual(bar(page.actions), ["cancel:Cancel"]);
});

test("a local cluster that does not answer offers Repair beside Retry", () => {
  const down: ConnectionState = { status: "error", clusterName: "local", reason: "unreachable", message: "ECONNREFUSED" };
  const page = clusterPage(input({ cluster: cluster({ local: true }), connection: down }));
  assert.equal(word(page.actions), "Not running");
  assert.deepEqual(bar(page.actions), ["showDetails:Show details", "repair:Repair", "connect:Retry"]);
  assert.doesNotMatch(page.actions, /ECONNREFUSED/, "the raw reason is in the output, behind Show details");
});

test("a remote cluster that does not answer offers Retry, not Repair", () => {
  const down: ConnectionState = { status: "error", clusterName: "local", reason: "unreachable", message: "x" };
  const page = clusterPage(input({ connection: down }));
  assert.equal(word(page.actions), "Can't reach");
  assert.deepEqual(bar(page.actions), ["showDetails:Show details", "connect:Retry"]);
});

test("a dropped connection that stopped retrying says so", () => {
  const lost: ConnectionState = { status: "error", clusterName: "local", reason: "lost", message: "x" };
  assert.equal(word(clusterPage(input({ connection: lost })).actions), "Connection lost");
});

test("no address: Edit is the one act, and the head does not offer it twice", () => {
  const page = clusterPage(input({ cluster: cluster({ endpoint: "" }) }));
  assert.equal(word(page.actions), "Not set up");
  assert.deepEqual(bar(page.actions), ["edit:Edit"]);
  assert.deepEqual(bar(page.head), ["remove:Remove from list"]);
  assert.match(page.body, /<dt>Address<\/dt><dd data-muted="true">Not set<\/dd>/);
});

test("signed in but not connected: Connect, and Sign out beside it", () => {
  const page = clusterPage(input());
  assert.equal(word(page.actions), "Not connected");
  assert.deepEqual(bar(page.actions), ["signOut:Sign out", "connect:Connect"]);
});

test("the head names the cluster by its label and carries Edit and Remove from list", () => {
  const page = clusterPage(input());
  assert.equal(page.title, "memql.localhost");
  assert.match(page.head, /<h1 class="mq-title">memql\.localhost<\/h1>/);
  assert.match(page.head, /<span class="mq-head-meta">main<\/span>/);
  assert.deepEqual(bar(page.head), ["edit:Edit", "remove:Remove from list"]);
});

test("the version meta names a newer release when there is one", () => {
  const listing = { tags: ["v0.21.0"], fetchedAt: 1 };
  assert.equal(versionMeta(cluster({ version: "v0.19.1" }), listing), "v0.19.1 · v0.21.0 available");
  assert.equal(versionMeta(cluster({ version: "" }), listing), "");
});

test("a local cluster says it is installed on this computer; a remote one does not", () => {
  assert.ok(factLabels(clusterPage(input({ cluster: cluster({ local: true }) })).body).includes("Installed"));
  assert.ok(!factLabels(clusterPage(input()).body).includes("Installed"));
});

test("a sign-in in flight is its own state, with Cancel and, when offered, a code", () => {
  const waiting = clusterPage(input({ facts: NOTHING, signingIn: { phase: "waiting", codeOffered: true } }));
  assert.equal(word(waiting.actions), "Signing in");
  assert.match(waiting.actions, /Waiting for you in the browser/);
  assert.deepEqual(bar(waiting.actions), ["useCode:Use a code instead", "cancel:Cancel"]);

  const opening = clusterPage(input({ facts: NOTHING, signingIn: { phase: "opening", codeOffered: false } }));
  assert.deepEqual(bar(opening.actions), ["cancel:Cancel"]);
});

test("a page's screen is the cluster, so a state change patches rather than repaints", () => {
  const a = clusterPage(input());
  const b = clusterPage(input({ connection: CONNECTED, identity: { email: "a@b.c", role: "owner" } }));
  assert.equal(a.screen, b.screen);
  assert.equal(a.screen, "cluster:local");
});

test("before the facts are read, the body is the shape of the facts", () => {
  const page = clusterPage(input({ facts: undefined }));
  assert.match(page.body, /class="mq-skeleton" data-shape="facts"/);
  assert.equal(page.actions, "");
});

test("a cluster no longer in the list says so, with Close", () => {
  const page = clusterPage(input({ cluster: undefined }));
  assert.match(page.body, /This cluster is no longer in your list\./);
  assert.deepEqual(bar(page.body), ["close:Close"]);
});

test("an unreadable cluster list says so, with Open file", () => {
  const page = clusterPage(input({ registryError: "clusters.yaml is malformed: line 3" }));
  assert.match(text(page.body), /Can't read your cluster list\./);
  assert.doesNotMatch(page.body, /line 3/, "the parser's words are for the file, not the page");
  assert.deepEqual(bar(page.body), ["openFile:Open file"]);
});

test("every state's bar holds at most three acts and at most one button", () => {
  // kit.actionBar throws otherwise; rendering every state is the check.
  const states: ConnectionState[] = [
    { status: "disconnected" },
    CONNECTED,
    { status: "connecting", clusterName: "local" },
    { status: "error", clusterName: "local", reason: "missingCredential", message: "x" },
    { status: "error", clusterName: "local", reason: "unreachable", message: "x" },
    { status: "error", clusterName: "local", reason: "lost", message: "x", retrying: true },
    { status: "error", clusterName: "local", reason: "notConfigured", message: "x" },
  ];
  for (const connection of states) {
    for (const facts of [SESSION, NOTHING, { ...NOTHING, ownerSetup: true }]) {
      const page = clusterPage(input({ cluster: cluster({ local: true }), connection, facts }));
      assert.ok(bar(page.actions).length <= 3);
    }
  }
});

test("an untrusted certificate on a local cluster leads with Repair", () => {
  const untrusted: ConnectionState = {
    status: "error",
    clusterName: "local",
    reason: "unreachable",
    message: "self-signed certificate in certificate chain",
  };
  const local = clusterPage(input({ cluster: cluster({ local: true }), connection: untrusted }));
  assert.equal(word(local.actions), "Certificate not trusted");
  assert.deepEqual(bar(local.actions), ["showDetails:Show details", "connect:Retry", "repair:Repair"]);
  const remote = clusterPage(input({ connection: untrusted }));
  assert.deepEqual(bar(remote.actions), ["showDetails:Show details", "connect:Retry"]);
});
