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
// Runs still lists saved workspace files without a connection. Its welcome
// appears only when the tree is empty, as with every VS Code view.
//
// Refs: #4425 #4423

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import {
  CLUSTER_SELECTED_KEY,
  LOCAL_CLUSTER_STATE_KEY,
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

// Normal-state assertions assume owner setup has completed. Pending setup is
// checked separately against the unmodified manifest below.
// The workbench renders welcomes only when the tree is empty.
function welcomeFor(view: string): WelcomeEntry[] {
  return welcomes.filter((entry) => entry.view === view && !(entry.when ?? "").startsWith("isWeb")).map(entry => ({ ...entry, when: entry.when?.replace(/ && !memql\.ownerSetupPending$/, "").replace(/^!isWeb && \((.*)\)$/, "$1") }));
}

/** The command ids a welcome's markdown links reach. */
function linkedCommands(contents: string): string[] {
  return [...contents.matchAll(/\(command:([A-Za-z0-9_.]+)\)/g)].map((m) => m[1]);
}

/** The welcome a view shows with no cluster in hand: the one keyed on the selection. */
function noClusterWelcome(view: string): WelcomeEntry[] {
  return welcomeFor(view).filter((entry) => entry.when === `!${CLUSTER_SELECTED_KEY}`);
}

for (const view of ["memqlDeployments", "memqlConstructs", "memqlData", "memqlRuns"]) {
  test(`${view} has one quiet empty state without setup actions`, () => {
    const entries = noClusterWelcome(view);
    assert.equal(entries.length, 1);
    assert.deepEqual(linkedCommands(entries[0].contents), []);
    assert.ok(entries[0].contents.length < 60);
  });
}

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

test("Clusters is the only unselected view that offers setup", () => {
  const setupCommands = ["memql.deployments.createDeployment", "memql.clusters.add", "memql.clusters.select", "memql.clusters.repair"];
  for (const entry of welcomes) {
    if (entry.view === "memqlClusters" || entry.when?.includes("memql.clusterSelected &&")) continue;
    assert.ok(!linkedCommands(entry.contents).some((cmd) => setupCommands.includes(cmd)), entry.view);
  }
});

test("sidebar action labels describe the same lifecycle state as the installer", () => {
  const entries = welcomeFor("memqlClusters");
  const expected = [
    ["absent", "Install Local Cluster", "memql.deployments.createDeployment"],
    ["install-incomplete", "Continue Setup", "memql.clusters.repair"],
    ["installed-healthy", "Connect to Local Cluster", "memql.clusters.connectLocal"],
    ["installed-unreachable", "Review Local Cluster", "memql.clusters.add"],
    ["present-unreceipted", "Review Local Cluster", "memql.clusters.add"],
  ];
  for (const [state, label, command] of expected) {
    const matches = entries.filter(e => e.when === `${LOCAL_CLUSTER_STATE_KEY} == ${state}`);
    assert.equal(matches.length, 1, state);
    const content = matches[0].contents;
    assert.ok(content.includes(`[${label}](command:${command})`), content);
    if (state !== "installed-healthy") assert.ok(!linkedCommands(content).includes("memql.clusters.connectLocal"));
    if (state !== "absent") assert.ok(!linkedCommands(content).includes("memql.deployments.createDeployment"));
    assert.doesNotMatch(content, /Add Another Cluster/);
  }
  const checking = entries.find(e => e.when === `!isWeb && !${LOCAL_CLUSTER_STATE_KEY}`);
  assert.ok(checking);
  assert.deepEqual(linkedCommands(checking.contents), []);
});

test("Runs says why it is empty: no saved runs, or no folder to save them in", () => {
  // It used to be a blank panel with two icons in either case.
  const entries = welcomeFor("memqlRuns");
  const byWhen = new Map(entries.map((entry) => [entry.when, entry]));
  const noRuns = byWhen.get("memql.clusterSelected && (workbenchState != empty)");
  const noFolder = byWhen.get("memql.clusterSelected && (workbenchState == empty)");
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

for (const view of ["memqlDeployments", "memqlConstructs", "memqlData", "memqlRuns"]) {
  test(`${view} offers no competing action while the owner passkey is pending`, () => {
    const entries = welcomes.filter(e => e.view === view);
    const pending = entries.filter(e => e.when === "memql.ownerSetupPending");
    assert.equal(pending.length, 1);
    assert.deepEqual(linkedCommands(pending[0].contents), []);
    assert.match(pending[0].contents, /owner passkey.*Clusters/);
    for (const normal of entries.filter(e => !pending.includes(e))) {
      assert.match(normal.when ?? "", /!memql\.ownerSetupPending/);
    }
  });
}
