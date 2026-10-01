// What the sidebar says when no cluster is selected (memql#4425).
//
// A MANIFEST TEST, for the reason test/clusterMenus.test.ts gives about menus
// and doubly so here: `viewsWelcome` is pure data, the workbench evaluates its
// `when` clause and draws the content itself, and there is no API anywhere --
// host lane included -- that reads back what a view is currently displaying as
// its welcome. The manifest IS the behaviour.
//
// AND THE FAILURE IT GUARDS IS SILENT IN BOTH DIRECTIONS. A welcome keyed on a
// misspelt context key renders permanently, because VS Code treats an unknown
// key as unset and `!unset` is true -- so a connected cluster would show
// "Not connected" over its own data. A welcome keyed on a key nothing publishes
// never renders, and the view is simply blank. Neither breaks a build.
//
// THE FOURTH VIEW IS ASSERTED BY ITS ABSENCE. Runs must NOT gain one of these:
// it lists the developer's own `runs.json`, keeps listing whatever the
// connection is doing, and gates EXECUTION instead (design D2). A welcome keyed
// on `!memql.clusterSelected` there would hide a file the editor did not write.
//
// Refs: #4425 #4423

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import {
  CLUSTER_SELECTED_KEY,
  LOCAL_CLUSTER_PRESENT_KEY,
  NOT_CONNECTED_REFUSAL,
} from "../src/state/connectionContext.js";

// dist-test/test/<name>.js is where esbuild.test.js puts this file, so the
// manifest is two levels up. Read at runtime rather than imported: it is the
// SHIPPED artifact that matters, and a JSON import would be inlined by the
// bundler into something indistinguishable from a fixture.
const MANIFEST = path.resolve(__dirname, "..", "..", "package.json");

interface WelcomeEntry {
  view: string;
  contents: string;
  when?: string;
}

interface Manifest {
  contributes: {
    commands: { command: string }[];
    viewsWelcome: WelcomeEntry[];
    views: Record<string, { id: string }[]>;
  };
}

const manifest = JSON.parse(fs.readFileSync(MANIFEST, "utf8")) as Manifest;
const welcomes = manifest.contributes.viewsWelcome;

/**
 * The views a cluster's data flows into that share one welcome. Runs is
 * deliberately not here, and Deployments has its own (below): it says which
 * act is right, install or select, from whether a local cluster exists.
 */
const GATED = ["memqlConstructs", "memqlData"] as const;

function welcomeFor(view: string): WelcomeEntry[] {
  return welcomes.filter((entry) => entry.view === view && !(entry.when ?? "").startsWith("isWeb")).map(entry => ({ ...entry, when: entry.when?.replace(/^!isWeb && \((.*)\)$/, "$1") }));
}

/** The command ids a welcome's markdown links reach. */
function linkedCommands(contents: string): string[] {
  return [...contents.matchAll(/\(command:([A-Za-z0-9_.]+)\)/g)].map((m) => m[1]);
}

/** The welcome a view shows with no cluster in hand: the one keyed on the selection. */
function noClusterWelcome(view: string): WelcomeEntry[] {
  return welcomeFor(view).filter((entry) => entry.when === `!${CLUSTER_SELECTED_KEY}`);
}

for (const view of GATED) {
  test(`${view} carries a welcome keyed on !${CLUSTER_SELECTED_KEY}`, () => {
    // The clause is asserted whole rather than merely "mentions the key",
    // because `memql.clusterSelected` without the `!` is the same typo class as
    // a misspelling and reads correctly at a glance: it would show the welcome
    // exactly when there IS a cluster.
    assert.equal(noClusterWelcome(view).length, 1, `expected exactly one no-cluster welcome for ${view}`);
  });

  test(`${view}'s welcome opens with the shared sentence and offers Select Cluster`, () => {
    const entry = noClusterWelcome(view)[0];
    assert.ok(
      entry.contents.startsWith("Not connected to a cluster."),
      `${view}'s welcome does not open with the shared refusal: ${entry.contents}`
    );
    // ONE SENTENCE, then links.
    const [sentence] = entry.contents.split("\n");
    assert.ok(
      sentence.length < 80,
      `${view}'s welcome sentence is a paragraph: ${sentence}`
    );
    assert.ok(
      linkedCommands(entry.contents).includes("memql.clusters.select"),
      `${view}'s welcome offers no way to select a cluster`
    );
  });
}

test("the welcomes and the runs refusal say the same first words", () => {
  // The shared sentence, checked from the manifest side. `NOT_CONNECTED_REFUSAL`
  // is what `memql.runs.execute` refuses with, and an operator meeting both in
  // one session must recognise them as one message rather than two policies.
  const opening = NOT_CONNECTED_REFUSAL.split(".")[0];
  for (const view of GATED) {
    assert.ok(noClusterWelcome(view)[0].contents.startsWith(opening));
  }
  for (const entry of deploymentsUnselected()) {
    assert.ok(entry.contents.startsWith(opening), entry.contents);
  }
});

/** The Deployments welcomes shown with no cluster selected. */
function deploymentsUnselected(): WelcomeEntry[] {
  return welcomeFor("memqlDeployments").filter((entry) => (entry.when ?? "").startsWith(`!${CLUSTER_SELECTED_KEY}`));
}

test("with no cluster selected, Deployments says one line and the right act", () => {
  // TWO WELCOMES, ONE LINE EACH, split on whether a local cluster exists: the
  // old one offered Install on a machine that already had one.
  const entries = deploymentsUnselected();
  assert.equal(entries.length, 2);
  const present = entries.find((e) => e.when === `!${CLUSTER_SELECTED_KEY} && memql.localClusterPresent`);
  const absent = entries.find((e) => e.when === `!${CLUSTER_SELECTED_KEY} && !memql.localClusterPresent`);
  assert.ok(present && absent, "the two welcomes are keyed on whether a local cluster is present");
  // A local cluster that is here but not selected -- or not even in the list,
  // which Select Cluster cannot reach -- opens on its own page, whose primary
  // is Connect, Reconnect or Sign in as its state asks.
  assert.deepEqual(linkedCommands(present.contents), ["memql.deployments.open", "memql.clusters.select"]);
  assert.deepEqual(linkedCommands(absent.contents), ["memql.deployments.createDeployment", "memql.clusters.select"]);
  for (const entry of entries) assert.equal(entry.contents.split("\n")[0], "Not connected to a cluster.");
});

test("a selected cluster whose history cannot be read says what it needs, never nothing", () => {
  // An empty tree under a selected cluster is either "no history yet" or "sign
  // in to see it", and the welcome says which rather than leaving a blank view.
  const signIn = welcomeFor("memqlDeployments").find((e) => (e.when ?? "").includes("memql.connectionState == signIn"));
  assert.ok(signIn, "no sign-in welcome");
  assert.ok((signIn.when ?? "").startsWith(CLUSTER_SELECTED_KEY));
  assert.deepEqual(linkedCommands(signIn.contents), ["memql.deployments.signIn"]);
  const empty = welcomeFor("memqlDeployments").find((e) => (e.when ?? "").includes("memql.connectionState == connected"));
  assert.equal(empty?.contents, "No history yet.");
});

test("every connection state that can leave a selected cluster's history empty has a welcome line", () => {
  // A blank view under a heading is the one thing a disconnected read must
  // never look like. The history of a cluster this editor cannot read is
  // empty for a reason, and the welcome says which.
  const states = ["connected", "signIn", "connecting", "unreachable", "notConfigured"];
  for (const state of states) {
    const entry = welcomeFor("memqlDeployments").find(
      (e) => e.when === `${CLUSTER_SELECTED_KEY} && memql.connectionState == ${state}`,
    );
    assert.ok(entry, `no welcome for ${state}`);
    assert.equal(entry.contents.split("\n")[0]!.length <= 40, true, `${state}: more than one short line`);
  }
});

test("only the Deployments welcome carries the install entry point", () => {
  // WHERE THE `local` ROW'S JOB WENT (design D4): the Deployments welcome, the
  // Clusters welcome and the view title menu. Constructs and Data do NOT get
  // it: neither is where an operator would look to install a cluster.
  assert.ok(
    deploymentsUnselected().some((entry) => linkedCommands(entry.contents).includes("memql.deployments.createDeployment")),
    "the Deployments welcome lost the install entry point"
  );
  // Constructs and Data offer what a person with no cluster in hand can do
  // about it -- select one, or add one when there is none to select (a
  // Select Cluster alone opened an empty picker on a machine with no
  // clusters). Neither is where somebody looks to install a cluster.
  for (const view of ["memqlConstructs", "memqlData"] as const) {
    assert.deepEqual(
      linkedCommands(noClusterWelcome(view)[0].contents),
      ["memql.clusters.select", "memql.clusters.add"],
      `${view}'s no-cluster welcome offers the wrong acts`
    );
  }
});

test("the Clusters welcome says whether a local cluster is already here", () => {
  // It is the SELECTOR: the one view that must say something useful when
  // there is no cluster to select, so it is never keyed on the connection --
  // a user with no clusters at all could never reach the offer to add one.
  // It IS keyed on what is on this machine: a local cluster that is running
  // but not in the list (removed from it, or built with `make up`) is one
  // click from connected, and leading with "Install" there would offer to
  // build a second one over it.
  const entries = welcomeFor("memqlClusters");
  assert.equal(entries.length, 2);
  const none = entries.find((e) => e.when === `!${LOCAL_CLUSTER_PRESENT_KEY}`);
  const present = entries.find((e) => e.when === LOCAL_CLUSTER_PRESENT_KEY);
  assert.ok(none !== undefined && present !== undefined, "one welcome for each answer of the presence key");
  for (const entry of entries) {
    assert.ok(!(entry.when ?? "").includes(CLUSTER_SELECTED_KEY), "the Clusters welcome is keyed on the connection");
  }

  assert.ok(none.contents.startsWith("No clusters yet.\n"));
  assert.deepEqual(linkedCommands(none.contents), ["memql.deployments.createDeployment", "memql.clusters.add"]);

  assert.ok(present.contents.startsWith("A local cluster is running on this computer.\n"));
  assert.deepEqual(linkedCommands(present.contents), ["memql.clusters.connectLocal", "memql.clusters.add"]);

  // One line and its buttons: no doctrine, no retired names.
  for (const entry of entries) {
    const [line] = entry.contents.split("\n");
    assert.ok(line.length < 60, `the welcome is a paragraph: ${line}`);
    assert.doesNotMatch(entry.contents, /portal/i);
  }
});

test("Runs has no connection-gated welcome", () => {
  // THE EXCEPTION, asserted as an absence. Runs lists `runs.json` from the
  // workspace -- files a developer wrote and a repository can ship -- and a
  // welcome keyed on the connection would replace them with a message about a
  // cluster. It gates the Run act instead.
  for (const entry of welcomeFor("memqlRuns")) {
    assert.ok(
      !(entry.when ?? "").includes(CLUSTER_SELECTED_KEY) && !(entry.when ?? "").includes("memql.connect"),
      "the Runs view grew a connection-gated welcome"
    );
  }
});

test("Runs says why it is empty: no saved runs, or no folder to save them in", () => {
  // It used to be a blank panel with two icons in either case.
  const entries = welcomeFor("memqlRuns");
  const byWhen = new Map(entries.map((entry) => [entry.when, entry]));
  const noRuns = byWhen.get("workbenchState != empty");
  const noFolder = byWhen.get("workbenchState == empty");
  assert.ok(noRuns !== undefined, "no welcome for a folder with no saved runs");
  assert.ok(noFolder !== undefined, "no welcome for a window with no folder");
  assert.deepEqual(linkedCommands(noRuns.contents), ["memql.runs.open"]);
  assert.deepEqual(linkedCommands(noFolder.contents), ["vscode.openFolder"]);
});

test("every welcome names a view that exists and a command that is contributed", () => {
  // A welcome for a view id nothing declares is dead data, and a link to an
  // uncontributed command renders as a link that does nothing when pressed --
  // both invisible without this.
  const views = new Set(
    Object.values(manifest.contributes.views).flat().map((entry) => entry.id)
  );
  // Plus the editor's own command a welcome links to.
  const commands = new Set([...manifest.contributes.commands.map((entry) => entry.command), "vscode.openFolder"]);
  for (const entry of welcomes) {
    assert.ok(views.has(entry.view), `welcome for unknown view ${entry.view}`);
    for (const command of linkedCommands(entry.contents)) {
      assert.ok(commands.has(command), `${entry.view}'s welcome links uncontributed ${command}`);
    }
  }
});


test("browser welcome connects without offering a native installation", () => {
  const browser = welcomes.filter(entry => entry.view === "memqlClusters" && entry.when?.startsWith("isWeb"));
  assert.equal(browser.length, 1);
  assert.deepEqual(linkedCommands(browser[0].contents), ["memql.clusters.add"]);
  for (const entry of welcomes.filter(entry => entry.contents.includes("Install Local Cluster"))) assert.match(entry.when ?? "", /!isWeb/);
});
