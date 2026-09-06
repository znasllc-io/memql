// What the Clusters tree's context menu actually offers, per row kind.
//
// WHY THIS IS A MANIFEST TEST AND NOT A HOST TEST. The acceptance item behind
// this file reads "a memqlLocalCluster row offers uninstall and a memqlCluster
// row does not", and the obvious place to prove that looks like the Extension
// Development Host. It is not. A host has no API that opens a tree item's
// context menu, and none that reads back the entries the workbench would have
// drawn -- the same wall test-host/index.ts already documents for clicking
// inside a webview. A host test written against this item could only assert
// that both commands are REGISTERED, which is a different and much weaker
// claim: a registered command with a `when` clause that matches nothing is
// invisible in the tree and would pass it.
//
// What decides the question is `contributes.menus` in package.json. The
// workbench evaluates those `when` clauses against the row's contextValue and
// draws what matches, so the clauses ARE the behaviour, and they are ordinary
// data this lane can read. That makes the real failure mode reachable: a clause
// edited to `viewItem == memqlLocalClusters`, or to a contextValue the tree
// stopped setting, silently matches no row and quietly removes the entry from
// the product. Nothing else in the build would notice.
//
// THE CLAUSES ARE EVALUATED, NOT STRING-COMPARED. Pinning the exact `when`
// text would fail on a harmless reordering and -- worse -- would pass on a
// clause that is byte-identical to the one that never matched. So the test
// answers the question the workbench asks: given a row of this contextValue,
// does this entry appear? Positively for the kind that must offer it, and
// negatively for the kind that must not, because "uninstall is restricted to
// local rows" is only half-proved by showing it on a local row.
//
// AND THE ROW MOVED AGAIN (memql#4426). Uninstall, Repair, Rebuild From
// Checkout, Open Local Checkout and Create Deployment were contributed to
// `view/item/context` scoped by the Deployments instance ROW's contextValue.
// That row no longer exists -- the view renders the selected cluster's runs
// flat -- so those five clauses now match nothing and would have vanished from
// the product with every test in this file still green, which is precisely the
// failure its header describes one paragraph up. They are contributed to
// `view/title` instead, scoped by `memql.deploymentsInstance`: a context key
// carrying the SAME three values the row's contextValue carried, because
// `view/title` clauses are evaluated with no `viewItem` in scope. The
// assertions below follow them, and the negative ones stay where they were.
//
// THE GROUP IS PART OF THE CLAIM. Remove is the inline trash can; Uninstall is
// a deliberate reach into the menu and is contributed to `lifecycle`.
// That separation is the whole of the design decision these two commands rest
// on (memql#3476, D1): removing a cluster from the list is routine and
// reversible, taking a k3d cluster, a hosts-file block and a CA off the machine
// is neither, and an operator aiming at the trash can must not be able to hit
// the second one. An `inline` group on Uninstall would put it back under that
// cursor, so it is asserted against by name.
//
// Refs: #4426 #4423 #3479 #3476 #3466

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

// Imported rather than spelled, so this file tracks the CODE and fails only
// when the manifest drifts from it. A hardcoded key would fail spuriously on a
// rename that kept both sides in step, and -- worse -- would keep passing if
// the code stopped publishing the key at all, which is the failure that makes
// a menu entry silently unreachable.
import { CLUSTER_SELECTED_KEY } from "../src/state/connectionContext.js";
import { DEPLOYMENTS_INSTANCE_KEY } from "../src/state/deploymentsCatalog.js";

// dist-test/test/<name>.js is where esbuild.test.js puts this file, so the
// manifest is two levels up. Read at runtime rather than imported: it is the
// SHIPPED artifact that matters here, and a JSON import would be resolved and
// inlined by the bundler into something this test could no longer distinguish
// from a fixture.
const MANIFEST = path.resolve(__dirname, "..", "..", "package.json");

interface MenuEntry {
  command: string;
  when?: string;
  group?: string;
}

interface CommandEntry {
  command: string;
  title: string;
}

interface Manifest {
  contributes: {
    commands: CommandEntry[];
    menus: Record<string, MenuEntry[]>;
  };
}

const manifest = JSON.parse(fs.readFileSync(MANIFEST, "utf8")) as Manifest;
const itemMenu = manifest.contributes.menus["view/item/context"] ?? [];
const titleMenu = manifest.contributes.menus["view/title"] ?? [];

/**
 * A row as the workbench sees it when it evaluates a `when` clause: the view
 * the item lives in, and the `contextValue` the TreeItem carries.
 */
type WhenContext = Record<string, string | boolean>;

const REMOTE_ROW: WhenContext = { view: "memqlClusters", viewItem: "memqlCluster" };
const LOCAL_ROW: WhenContext = { view: "memqlClusters", viewItem: "memqlLocalCluster" };
// Where uninstall lives NOW (memql#3742, then memql#4426). Taking a cluster off
// the machine is a Deployments action -- the Clusters view is connections, and
// its rows offer nothing that changes the machine -- and within Deployments it
// is a TITLE menu entry scoped by the selection, because the instance row it
// used to hang off has been replaced by the run timeline.
//
// The three values are the ones `instanceContextValue` produces, unchanged from
// when they labelled a row: the vocabulary moved key, it was not rewritten.
const LOCAL_INSTANCE_SELECTED: WhenContext = {
  view: "memqlDeployments",
  [DEPLOYMENTS_INSTANCE_KEY]: "memqlLocalInstance",
};
const ABSENT_INSTANCE_SELECTED: WhenContext = {
  view: "memqlDeployments",
  [DEPLOYMENTS_INSTANCE_KEY]: "memqlLocalInstanceAbsent",
};
const REMOTE_INSTANCE_SELECTED: WhenContext = {
  view: "memqlDeployments",
  [DEPLOYMENTS_INSTANCE_KEY]: "memqlRemoteInstance",
};
// Nothing selected: the key is unset, which is what every `==` clause fails
// against and what leaves the welcome as the only thing on the view.
const NOTHING_SELECTED: WhenContext = { view: "memqlDeployments" };
// The two Clusters-view instance rows this file used to name, kept so the
// negative assertions still speak the language of the surface they left.
const LOCAL_INSTANCE_ROW: WhenContext = {
  view: "memqlDeployments",
  viewItem: "memqlLocalInstance",
};
const ABSENT_INSTANCE_ROW: WhenContext = {
  view: "memqlDeployments",
  viewItem: "memqlLocalInstanceAbsent",
};

// A recursive-descent evaluator over the fragment of the when-clause grammar
// this manifest uses: `&&`, `||`, `!`, parentheses, and `==` / `!=` against a
// bare word. It is deliberately small and deliberately strict -- an unparseable
// clause throws rather than evaluating to false, because a silent false here
// would be this file reproducing the exact defect it exists to catch.
class WhenParser {
  private readonly tokens: string[];
  private at = 0;

  constructor(clause: string) {
    this.tokens = clause.match(/\(|\)|&&|\|\||==|!=|!|[A-Za-z0-9_.:-]+/g) ?? [];
    if (this.tokens.length === 0) {
      throw new Error(`when clause tokenized to nothing: ${JSON.stringify(clause)}`);
    }
  }

  evaluate(context: WhenContext): boolean {
    const value = this.or(context);
    if (this.at !== this.tokens.length) {
      throw new Error(`trailing tokens in when clause at ${this.tokens[this.at]}`);
    }
    return value;
  }

  private or(context: WhenContext): boolean {
    let value = this.and(context);
    while (this.tokens[this.at] === "||") {
      this.at += 1;
      // Both sides are evaluated: short-circuiting would let a malformed right
      // operand go unnoticed whenever the left one happened to be true.
      const right = this.and(context);
      value = value || right;
    }
    return value;
  }

  private and(context: WhenContext): boolean {
    let value = this.comparison(context);
    while (this.tokens[this.at] === "&&") {
      this.at += 1;
      const right = this.comparison(context);
      value = value && right;
    }
    return value;
  }

  private comparison(context: WhenContext): boolean {
    if (this.tokens[this.at] === "!") {
      this.at += 1;
      return !this.comparison(context);
    }
    if (this.tokens[this.at] === "(") {
      this.at += 1;
      const value = this.or(context);
      if (this.tokens[this.at] !== ")") {
        throw new Error("unbalanced parenthesis in when clause");
      }
      this.at += 1;
      return value;
    }

    const left = this.word();
    const operator = this.tokens[this.at];
    if (operator === "==" || operator === "!=") {
      this.at += 1;
      const right = this.word();
      // The LEFT side names a context key; the RIGHT side is the literal it is
      // being compared to. An unset key compares equal to nothing, which is how
      // a clause naming a contextValue the tree stopped setting stops matching.
      const equal = this.lookup(left, context) === right;
      return operator === "==" ? equal : !equal;
    }
    // Bare identifier in boolean position: `true`, or a context key that is
    // set to a truthy value. Anything else is unset, and unset is false.
    if (left === "true") return true;
    if (left === "false") return false;
    return context[left] === true;
  }

  private word(): string {
    const token = this.tokens[this.at];
    if (token === undefined || /^(\(|\)|&&|\|\||==|!=|!)$/.test(token)) {
      throw new Error(`expected a word in the when clause, found ${String(token)}`);
    }
    this.at += 1;
    return token;
  }

  private lookup(key: string, context: WhenContext): string | undefined {
    const value = context[key];
    return typeof value === "string" ? value : undefined;
  }
}

function matches(entry: MenuEntry, context: WhenContext): boolean {
  // No `when` means "always", which is how VS Code reads an absent clause.
  if (entry.when === undefined || entry.when === "") return true;
  return new WhenParser(entry.when).evaluate(context);
}

function entriesFor(command: string): MenuEntry[] {
  return itemMenu.filter((entry) => entry.command === command);
}

function titleEntriesFor(command: string): MenuEntry[] {
  return titleMenu.filter((entry) => entry.command === command);
}

// The evaluator is the instrument, so it is calibrated before it is trusted. A
// broken parser that answered false to everything would make every "does not
// offer" assertion below pass for the wrong reason.
test("the when-clause evaluator answers the shapes this manifest uses", () => {
  const clause = "view == memqlClusters && (viewItem == memqlCluster || viewItem == memqlLocalCluster)";
  assert.equal(new WhenParser(clause).evaluate(REMOTE_ROW), true);
  assert.equal(new WhenParser(clause).evaluate(LOCAL_ROW), true);
  assert.equal(
    new WhenParser(clause).evaluate({ view: "memqlRuns", viewItem: "memqlRun" }),
    false,
    "a clause bound to memqlClusters matched a row in another view"
  );
  assert.equal(
    new WhenParser("view == memqlClusters && viewItem == memqlLocalCluster").evaluate(REMOTE_ROW),
    false
  );
  // The typo case this file exists for: a contextValue nothing sets.
  assert.equal(
    new WhenParser("view == memqlClusters && viewItem == memqlLocalClusters").evaluate(LOCAL_ROW),
    false
  );
});

test("uninstall is offered from the Deployments title menu when a local cluster is selected", () => {
  const entries = titleEntriesFor("memql.clusters.uninstall");
  assert.equal(entries.length, 1, "expected exactly one uninstall entry in view/title");
  assert.ok(
    matches(entries[0], LOCAL_INSTANCE_SELECTED),
    `uninstall does not reach a selected local instance: when = ${String(entries[0].when)}`
  );
});

test("uninstall reaches NOTHING but a selected, installed local cluster", () => {
  // The three ways the key can be set, and the one way it can be unset. Each is
  // a machine an uninstall would be wrong on: nothing is installed, the cluster
  // is somebody else's, or no cluster is chosen at all.
  const entries = titleEntriesFor("memql.clusters.uninstall");
  for (const context of [ABSENT_INSTANCE_SELECTED, REMOTE_INSTANCE_SELECTED, NOTHING_SELECTED]) {
    assert.deepEqual(
      entries.filter((entry) => matches(entry, context)),
      [],
      `uninstall reached ${String(context[DEPLOYMENTS_INSTANCE_KEY] ?? "an unselected view")}`
    );
  }
});

test("the five instance actions LEFT view/item/context and none came back", () => {
  // THE DELETION GUARD for memql#4426. The Deployments view renders runs, not
  // instances, so no row in it carries a `memqlLocalInstance` contextValue any
  // more. A clause still scoped to one matches nothing and is invisible in the
  // product -- and, unlike a deleted entry, it LOOKS present in the manifest.
  // That is the exact failure mode this file's header is about, so the move is
  // asserted from the side it left as well as the side it arrived at.
  const moved = [
    "memql.clusters.uninstall",
    "memql.clusters.repair",
    "memql.deployments.rebuildFromCheckout",
    "memql.deployments.openCheckout",
    "memql.deployments.createDeployment",
  ];
  for (const command of moved) {
    for (const row of [LOCAL_INSTANCE_ROW, ABSENT_INSTANCE_ROW]) {
      assert.deepEqual(
        entriesFor(command).filter((entry) => matches(entry, row)),
        [],
        `${command} is still scoped to a Deployments instance row, which no longer exists`
      );
    }
  }
});

test("every action the instance row offered is reachable from the title menu", () => {
  // "Every action reachable before is reachable after" is an acceptance item of
  // memql#4426, and this is it as an assertion rather than as a claim in a PR
  // body. The pairing is the one the old rows had: an ABSENT local cluster
  // could only be created; an INSTALLED one could also be repaired, rebuilt,
  // opened at its checkout and uninstalled.
  const installed = [
    "memql.clusters.uninstall",
    "memql.clusters.repair",
    "memql.deployments.rebuildFromCheckout",
    "memql.deployments.openCheckout",
  ];
  for (const command of installed) {
    assert.ok(
      titleEntriesFor(command).some((entry) => matches(entry, LOCAL_INSTANCE_SELECTED)),
      `${command} is unreachable: it left the instance row and did not arrive in the title menu`
    );
  }
  assert.ok(
    titleEntriesFor("memql.deployments.createDeployment").some((entry) =>
      matches(entry, ABSENT_INSTANCE_SELECTED)
    ),
    "create deployment is unreachable on a machine with nothing installed"
  );
});

test("opening the instance page is offered whenever a cluster is selected", () => {
  // The route the instance ROW used to be: its `command` opened the page. With
  // the row gone the only way back to it is this entry, so it is gated on the
  // connection key alone -- every selected cluster has an instance page, local
  // or remote.
  const entries = titleEntriesFor("memql.deployments.open");
  assert.equal(entries.length, 1, "expected exactly one open-instance entry in view/title");
  for (const context of [
    LOCAL_INSTANCE_SELECTED,
    ABSENT_INSTANCE_SELECTED,
    REMOTE_INSTANCE_SELECTED,
  ]) {
    assert.ok(
      matches(entries[0], { ...context, [CLUSTER_SELECTED_KEY]: true }),
      `the instance page is unreachable for ${String(context[DEPLOYMENTS_INSTANCE_KEY])}`
    );
  }
  assert.equal(
    matches(entries[0], NOTHING_SELECTED),
    false,
    "the instance page is offered with no cluster selected"
  );
});

test("uninstall is NOT offered on any Clusters row", () => {
  // THE MOVE, asserted from the side it left (memql#3742). Clusters is
  // connections: removing a row there takes the connection and leaves the
  // cluster running, and an uninstall beside it is the one action whose
  // presence makes that distinction unreadable.
  for (const row of [LOCAL_ROW, REMOTE_ROW]) {
    assert.deepEqual(
      entriesFor("memql.clusters.uninstall").filter((entry) => matches(entry, row)),
      [],
      `uninstall reached ${String(row["viewItem"])}`
    );
  }
});

test("uninstall is NOT offered on a machine with nothing installed", () => {
  assert.deepEqual(
    entriesFor("memql.clusters.uninstall").filter((entry) => matches(entry, ABSENT_INSTANCE_ROW)),
    [],
    "uninstall reached the absent-instance row -- there is nothing to remove"
  );
});

test("repair moved with it, and to the same place", () => {
  // Repair is in TWO title menus and that is deliberate rather than a
  // duplicate: the Clusters view has carried it since memql#3742 (group
  // 1_manage) because that is where an operator with a broken cluster looks
  // first, and Deployments carries it beside the rest of the instance
  // lifecycle. Both are asserted so neither can be dropped as "the other one
  // has it".
  const entries = titleEntriesFor("memql.clusters.repair");
  assert.ok(
    entries.some((entry) => matches(entry, LOCAL_INSTANCE_SELECTED)),
    "repair does not reach a selected local instance"
  );
  assert.ok(
    entries.some((entry) => matches(entry, { view: "memqlClusters" })),
    "repair left the Clusters title menu"
  );
  const deployments = entries.filter((entry) => (entry.when ?? "").includes("memqlDeployments"));
  assert.equal(deployments.length, 1, "expected exactly one Deployments repair entry");
  for (const context of [ABSENT_INSTANCE_SELECTED, REMOTE_INSTANCE_SELECTED, NOTHING_SELECTED]) {
    assert.equal(
      matches(deployments[0], context),
      false,
      String(context[DEPLOYMENTS_INSTANCE_KEY] ?? "nothing selected")
    );
  }
});

// -----------------------------------------------------------------------------
// The deletion guards (memql#3742). A deletion nothing guards grows back.
// -----------------------------------------------------------------------------

test("the topology view is gone from the manifest, command and menus alike", () => {
  // `memql.cluster.open` opened an 894-line webview drawing a pod grid, orphan
  // verdicts and under-replica alarms -- cluster state, which the portal owns
  // and already draws. Two surfaces answering one question diverge on the day
  // the second one ships.
  assert.equal(
    manifest.contributes.commands.some((c) => c.command === "memql.cluster.open"),
    false,
    "memql.cluster.open is contributed again"
  );
  for (const [menu, entries] of Object.entries(manifest.contributes.menus)) {
    assert.deepEqual(
      entries.filter((e) => e.command === "memql.cluster.open"),
      [],
      `memql.cluster.open reappeared in ${menu}`
    );
  }
});

test("the Clusters context menu changes nothing on the machine", () => {
  // Clusters is CONNECTIONS. Anything that installs, repairs or removes an
  // artifact from this machine belongs to Deployments, and the whole point of
  // the split is that a row in one view cannot do the other's work.
  const machineActions = [
    "memql.clusters.uninstall",
    "memql.clusters.repair",
    "memql.deployments.createDeployment",
  ];
  for (const row of [LOCAL_ROW, REMOTE_ROW]) {
    const offered = itemMenu
      .filter((entry) => machineActions.includes(entry.command))
      .filter((entry) => matches(entry, row))
      .map((entry) => entry.command);
    assert.deepEqual(offered, [], `${String(row["viewItem"])} offers ${offered.join(", ")}`);
  }
});

test("taking ownership reaches a LOCAL cluster row (memql#3906)", () => {
  // The gap this closes. `memql.clusters.takeOwnership` was contributed to the
  // palette only, so an operator who closed the install wizard -- or whose run
  // predated it -- had no route to the one action that gives a bootstrapped
  // owner its first credential. The cluster sits in front of them in the tree
  // and right-clicking it offered sign-in, which cannot work.
  const entries = entriesFor("memql.clusters.takeOwnership");
  assert.ok(entries.length > 0, "no take-ownership entry in view/item/context");
  assert.ok(
    entries.some((entry) => matches(entry, LOCAL_ROW)),
    "take ownership does not reach a memqlLocalCluster row"
  );
});

test("taking ownership is NOT offered on a remote cluster", () => {
  // Minting needs `kubectl exec` into the identity pod, so it can only be done
  // from the machine hosting the cluster. `mintOwnershipLink` refuses a remote
  // one by name (`notLocal`); offering the action anyway would put a button in
  // front of an operator whose only outcome is that refusal.
  assert.deepEqual(
    entriesFor("memql.clusters.takeOwnership").filter((entry) => matches(entry, REMOTE_ROW)),
    [],
    "take ownership reached a remote cluster, where there is no pod to mint in"
  );
});

test("remove is offered on both row kinds", () => {
  const entries = entriesFor("memql.clusters.remove");
  assert.ok(entries.length > 0, "no remove entry in view/item/context");
  assert.ok(
    entries.some((entry) => matches(entry, LOCAL_ROW)),
    "remove does not reach a memqlLocalCluster row"
  );
  assert.ok(
    entries.some((entry) => matches(entry, REMOTE_ROW)),
    "remove does not reach a memqlCluster row"
  );
});

test("remove is the inline action and uninstall is not", () => {
  const remove = entriesFor("memql.clusters.remove");
  assert.ok(
    remove.every((entry) => (entry.group ?? "").startsWith("inline")),
    "remove left the inline group -- the trash can beside a row is the routine action"
  );

  const uninstall = entriesFor("memql.clusters.uninstall");
  assert.ok(
    uninstall.every((entry) => !(entry.group ?? "").startsWith("inline")),
    "uninstall is contributed inline -- an irreversible action must not sit under the cursor aimed at Remove"
  );
});

test("both commands the menu names are declared", () => {
  const declared = new Set(manifest.contributes.commands.map((entry) => entry.command));
  for (const command of [
    "memql.clusters.remove",
    "memql.clusters.uninstall",
    "memql.clusters.repair",
    "memql.clusters.connection",
    "memql.clusters.openConsole",
  ]) {
    assert.ok(declared.has(command), `${command} appears in a menu but is not a contributed command`);
  }
});
