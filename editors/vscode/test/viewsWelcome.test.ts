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

/** The three views a cluster's data flows into. Runs is deliberately not here. */
const GATED = ["memqlDeployments", "memqlConstructs", "memqlData"] as const;

function welcomeFor(view: string): WelcomeEntry[] {
  return welcomes.filter((entry) => entry.view === view);
}

/** The command ids a welcome's markdown links reach. */
function linkedCommands(contents: string): string[] {
  return [...contents.matchAll(/\(command:([A-Za-z0-9_.]+)\)/g)].map((m) => m[1]);
}

for (const view of GATED) {
  test(`${view} carries a welcome keyed on !${CLUSTER_SELECTED_KEY}`, () => {
    const entries = welcomeFor(view);
    assert.equal(entries.length, 1, `expected exactly one welcome for ${view}`);
    // The clause is asserted whole rather than merely "mentions the key",
    // because `memql.clusterSelected` without the `!` is the same typo class as
    // a misspelling and reads correctly at a glance: it would show the welcome
    // exactly when there IS a cluster.
    assert.equal(entries[0].when, `!${CLUSTER_SELECTED_KEY}`, `${view}'s welcome clause`);
  });

  test(`${view}'s welcome opens with the shared sentence and offers Select Cluster`, () => {
    const entry = welcomeFor(view)[0];
    assert.ok(
      entry.contents.startsWith("Not connected."),
      `${view}'s welcome does not open with the shared refusal: ${entry.contents}`
    );
    // ONE SENTENCE, then links. The three read the same because they describe
    // the same condition; only the noun changes ("its deployments", "its
    // constructs", "its data"), which is the "consistent message that varies a
    // bit per view" the epic was filed for.
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
    assert.ok(welcomeFor(view)[0].contents.startsWith(opening));
  }
});

test("only the Deployments welcome carries the install entry point", () => {
  // WHERE THE `local` ROW'S JOB WENT (design D4). The Deployments view used to
  // render a `local` row even on a machine with nothing installed, purely so an
  // operator had somewhere to start. That row is gone; this link is one of the
  // three places its job moved to, and the other two are the Clusters welcome
  // and the view title menu.
  //
  // Constructs and Data do NOT get it: neither is where an operator would look
  // to install a cluster, and a third copy of the offer would make the
  // consistent sentence three different sentences.
  assert.ok(
    linkedCommands(welcomeFor("memqlDeployments")[0].contents).includes(
      "memql.deployments.createDeployment"
    ),
    "the Deployments welcome lost the install entry point"
  );
  for (const view of ["memqlConstructs", "memqlData"] as const) {
    assert.deepEqual(
      linkedCommands(welcomeFor(view)[0].contents),
      ["memql.clusters.select"],
      `${view}'s welcome offers more than selecting a cluster`
    );
  }
});

test("the Clusters welcome is unchanged and stays unconditional", () => {
  // It is the SELECTOR: the one view that must say something useful when there
  // is no cluster to select, so it is keyed on nothing and renders whenever its
  // tree is empty. Gating it on `!memql.clusterSelected` would be circular --
  // a user with no clusters at all could never reach the offer to add one.
  const entries = welcomeFor("memqlClusters");
  assert.equal(entries.length, 1);
  assert.equal(entries[0].when, undefined, "the Clusters welcome grew a when clause");
  assert.deepEqual(linkedCommands(entries[0].contents).sort(), [
    "memql.clusters.add",
    "memql.deployments.createDeployment",
  ]);
});

test("Runs has no connection-gated welcome", () => {
  // THE EXCEPTION, asserted as an absence. Runs lists `runs.json` from the
  // workspace -- files a developer wrote and a repository can ship -- and a
  // welcome keyed on the connection would replace them with a message about a
  // cluster. It gates `memql.runs.execute` instead.
  for (const entry of welcomeFor("memqlRuns")) {
    assert.ok(
      !(entry.when ?? "").includes(CLUSTER_SELECTED_KEY),
      "the Runs view grew a connection-gated welcome"
    );
  }
});

test("every welcome names a view that exists and a command that is contributed", () => {
  // A welcome for a view id nothing declares is dead data, and a link to an
  // uncontributed command renders as a link that does nothing when pressed --
  // both invisible without this.
  const views = new Set(
    Object.values(manifest.contributes.views).flat().map((entry) => entry.id)
  );
  const commands = new Set(manifest.contributes.commands.map((entry) => entry.command));
  for (const entry of welcomes) {
    assert.ok(views.has(entry.view), `welcome for unknown view ${entry.view}`);
    for (const command of linkedCommands(entry.contents)) {
      assert.ok(commands.has(command), `${entry.view}'s welcome links uncontributed ${command}`);
    }
  }
});
