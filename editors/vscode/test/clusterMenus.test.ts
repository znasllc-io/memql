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
import { CLUSTER_SELECTED_KEY, LOCAL_CLUSTER_PRESENT_KEY } from "../src/state/connectionContext.js";
import {
  DEPLOYMENTS_HAS_BRANCH_KEY,
  DEPLOYMENTS_HAS_CHECKOUT_KEY,
  DEPLOYMENTS_INSTANCE_KEY,
} from "../src/state/deploymentsCatalog.js";

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

// Cluster rows, as clusters/status.ts `clusterContextValue` spells them:
// `memqlCluster;<state>[;local][;signedIn][;ownerSetup][;os][;inUse]`.
function row(contextValue: string): WhenContext {
  return { view: "memqlClusters", viewItem: contextValue };
}
const REMOTE_ROW = row("memqlCluster;idle;signedIn;os");
const LOCAL_ROW = row("memqlCluster;idle;local;signedIn;os;inUse");
const SIGN_IN_ROW = row("memqlCluster;signIn;local;os");
const FIRST_RUN_ROW = row("memqlCluster;signIn;local;ownerSetup;os");
const CONNECTED_ROW = row("memqlCluster;connected;signedIn;os;inUse");
const CONNECTED_NO_OS_ROW = row("memqlCluster;connected;signedIn;inUse");
const CONNECTING_ROW = row("memqlCluster;connecting;signedIn;os;inUse");
const UNREACHABLE_ROW = row("memqlCluster;unreachable;local;signedIn;os");
const NOT_CONFIGURED_ROW = row("memqlCluster;notConfigured");
const EVERY_ROW = [
  REMOTE_ROW,
  LOCAL_ROW,
  SIGN_IN_ROW,
  FIRST_RUN_ROW,
  CONNECTED_ROW,
  CONNECTED_NO_OS_ROW,
  CONNECTING_ROW,
  UNREACHABLE_ROW,
  NOT_CONFIGURED_ROW,
];
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
// The same, with a checkout recorded on a branch: what Rebuild, Open checkout
// and Pull and rebuild need to have something to act on.
const LOCAL_WITH_CHECKOUT: WhenContext = {
  ...LOCAL_INSTANCE_SELECTED,
  [DEPLOYMENTS_HAS_CHECKOUT_KEY]: true,
  [DEPLOYMENTS_HAS_BRANCH_KEY]: true,
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
// this manifest uses: `&&`, `||`, `!`, parentheses, `==` / `!=` against a bare
// word, and `=~` against a /regex/ literal (the Clusters rows' contextValue
// carries flags, matched by pattern). It is deliberately small and
// deliberately strict -- an unparseable clause throws rather than evaluating
// to false, because a silent false here would be this file reproducing the
// exact defect it exists to catch.
class WhenParser {
  private readonly tokens: string[];
  private at = 0;

  constructor(clause: string) {
    this.tokens = clause.match(/\/(?:\\.|[^/])*\/[a-z]*|\(|\)|&&|\|\||==|!=|=~|!|[A-Za-z0-9_.:-]+/g) ?? [];
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
    if (operator === "=~") {
      this.at += 1;
      const literal = this.tokens[this.at] ?? "";
      const parsed = /^\/((?:\\.|[^/])*)\/([a-z]*)$/.exec(literal);
      if (parsed === null) throw new Error(`expected a /regex/ after =~, found ${literal}`);
      this.at += 1;
      return new RegExp(parsed[1], parsed[2]).test(this.lookup(left, context) ?? "");
    }
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

/** The inline acts a row shows. */
function inlineCommands(r: WhenContext): string[] {
  return itemMenu
    .filter((entry) => (entry.group ?? "").startsWith("inline") && matches(entry, r))
    .map((entry) => entry.command);
}

/** The context-menu acts a row shows (everything that is not inline). */
function contextCommands(r: WhenContext): string[] {
  return itemMenu
    .filter((entry) => !(entry.group ?? "").startsWith("inline") && matches(entry, r))
    .map((entry) => entry.command);
}

function titleEntriesFor(command: string): MenuEntry[] {
  return titleMenu.filter((entry) => entry.command === command);
}

// The evaluator is the instrument, so it is calibrated before it is trusted. A
// broken parser that answered false to everything would make every "does not
// offer" assertion below pass for the wrong reason.
test("the when-clause evaluator answers the shapes this manifest uses", () => {
  const legacy = row("memqlLegacy");
  const clause = "view == memqlClusters && (viewItem == memqlLegacy || viewItem == memqlOther)";
  assert.equal(new WhenParser(clause).evaluate(legacy), true);
  assert.equal(
    new WhenParser(clause).evaluate({ view: "memqlRuns", viewItem: "memqlLegacy" }),
    false,
    "a clause bound to memqlClusters matched a row in another view"
  );
  // The regex form the Clusters rows use, both ways.
  const signIn = "view == memqlClusters && viewItem =~ /^memqlCluster;signIn(;|$)/";
  assert.equal(new WhenParser(signIn).evaluate(SIGN_IN_ROW), true);
  assert.equal(new WhenParser(signIn).evaluate(CONNECTED_ROW), false);
  // The typo case this file exists for: a pattern nothing sets.
  assert.equal(new WhenParser("view == memqlClusters && viewItem =~ /^memqlClusters;/").evaluate(LOCAL_ROW), false);
  assert.throws(() => new WhenParser("viewItem =~ notaregex").evaluate(LOCAL_ROW));
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
    "memql.deployments.changeVersion",
  ];
  for (const command of installed) {
    assert.ok(
      titleEntriesFor(command).some((entry) => matches(entry, LOCAL_INSTANCE_SELECTED)),
      `${command} is unreachable: it left the instance row and did not arrive in the title menu`
    );
  }
  // The three that act on the checkout, where there is one.
  for (const command of [
    "memql.deployments.rebuildFromCheckout",
    "memql.deployments.openCheckout",
    "memql.deployments.updateAndRebuild",
  ]) {
    assert.ok(
      titleEntriesFor(command).some((entry) => matches(entry, LOCAL_WITH_CHECKOUT)),
      `${command} is unreachable with a checkout recorded`
    );
  }
  assert.ok(
    titleEntriesFor("memql.deployments.createDeployment").some((entry) =>
      matches(entry, ABSENT_INSTANCE_SELECTED)
    ),
    "create deployment is unreachable on a machine with nothing installed"
  );
});

test("acts on the checkout are withheld where no checkout is recorded", () => {
  // The old menu offered Rebuild and Open checkout for every installed local
  // cluster, and without a checkout the click landed silently on the overview
  // or a dead-end toast.
  for (const command of [
    "memql.deployments.rebuildFromCheckout",
    "memql.deployments.openCheckout",
    "memql.deployments.updateAndRebuild",
  ]) {
    assert.deepEqual(
      titleEntriesFor(command).filter((entry) => matches(entry, LOCAL_INSTANCE_SELECTED)),
      [],
      `${command} is offered with no checkout recorded`
    );
  }
  // A checkout pinned to a tag has nothing to pull.
  assert.deepEqual(
    titleEntriesFor("memql.deployments.updateAndRebuild").filter((entry) =>
      matches(entry, { ...LOCAL_WITH_CHECKOUT, [DEPLOYMENTS_HAS_BRANCH_KEY]: false })
    ),
    [],
  );
});

test("the Deployments title bar has no Refresh: the view keeps itself current", () => {
  assert.deepEqual(titleEntriesFor("memql.deployments.refresh"), []);
});

test("Sign in sits on the Deployments title bar exactly when the selected cluster needs it", () => {
  const entries = titleEntriesFor("memql.deployments.signIn");
  assert.equal(entries.length, 1);
  assert.equal(entries[0].group?.startsWith("navigation"), true, "sign-in is an inline act");
  assert.ok(matches(entries[0], { view: "memqlDeployments", "memql.connectionState": "signIn" }));
  assert.equal(matches(entries[0], { view: "memqlDeployments", "memql.connectionState": "connected" }), false);
});

test("opening the cluster page is offered whenever a cluster is selected", () => {
  // The route the instance ROW used to be: its `command` opened the page. With
  // the row gone the only way back to it is this entry, so it is gated on the
  // connection key alone -- every selected cluster has a page, local or
  // remote. It opens the SELECTED cluster; "Open Local Deployment" stays in the
  // palette and now always opens the local one, as its title says.
  const entries = titleEntriesFor("memql.deployments.openCluster");
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
  for (const r of EVERY_ROW) {
    assert.deepEqual(
      entriesFor("memql.clusters.uninstall").filter((entry) => matches(entry, r)),
      [],
      `uninstall reached ${String(r["viewItem"])}`
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
  // Only where there is a local cluster to repair: a title act on a machine
  // with none would be offered and then refused.
  assert.ok(
    entries.some((entry) => matches(entry, { view: "memqlClusters", [LOCAL_CLUSTER_PRESENT_KEY]: true })),
    "repair left the Clusters title menu"
  );
  assert.equal(
    entries.some((entry) => matches(entry, { view: "memqlClusters" })),
    false,
    "repair is offered in the Clusters title with no local cluster on this machine"
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
  for (const r of EVERY_ROW) {
    const offered = itemMenu
      .filter((entry) => machineActions.includes(entry.command))
      .filter((entry) => matches(entry, r))
      .map((entry) => entry.command);
    assert.deepEqual(offered, [], `${String(r["viewItem"])} offers ${offered.join(", ")}`);
  }
});

test("creating the owner passkey is offered exactly on a first-run row", () => {
  // The flag is set only for a local cluster whose install receipt names its
  // owner and that nobody has signed in to here (clusters/ownershipRoute.ts).
  // The owner's own `make up` cluster -- no receipt, passkey long enrolled --
  // used to be offered it, and the walk dead-ended in "Re-run the installer".
  const entries = entriesFor("memql.clusters.takeOwnership");
  assert.ok(entries.some((entry) => matches(entry, FIRST_RUN_ROW)), "not offered on a first run");
  for (const r of EVERY_ROW.filter((x) => x !== FIRST_RUN_ROW)) {
    assert.equal(entries.some((entry) => matches(entry, r)), false, `offered on ${String(r["viewItem"])}`);
  }
});

test("a row needing sign-in carries ONE inline act, Sign in", () => {
  assert.deepEqual(inlineCommands(SIGN_IN_ROW), ["memql.clusters.signIn"]);
  assert.deepEqual(inlineCommands(FIRST_RUN_ROW), ["memql.clusters.signIn"]);
});

test("a connected row carries ONE inline act, Open MemQL OS, when it has an address", () => {
  assert.deepEqual(inlineCommands(CONNECTED_ROW), ["memql.clusters.openConsole"]);
  assert.deepEqual(inlineCommands(CONNECTED_NO_OS_ROW), []);
});

test("every other row carries no inline act", () => {
  for (const r of [REMOTE_ROW, LOCAL_ROW, CONNECTING_ROW, UNREACHABLE_ROW, NOT_CONFIGURED_ROW]) {
    assert.deepEqual(inlineCommands(r), [], String(r["viewItem"]));
  }
});

test("the context menu offers only what is legal in the row's state", () => {
  const menu = (r: WhenContext): string[] => contextCommands(r);
  // Sign out only when something is stored to end; Disconnect only when
  // something is connected; sign-in only when it is needed.
  assert.ok(menu(CONNECTED_ROW).includes("memql.clusters.signOut"));
  assert.ok(menu(CONNECTED_ROW).includes("memql.clusters.disconnect"));
  assert.ok(!menu(CONNECTED_ROW).includes("memql.clusters.signIn"));
  assert.ok(menu(CONNECTING_ROW).includes("memql.clusters.disconnect"));
  assert.ok(!menu(SIGN_IN_ROW).includes("memql.clusters.signOut"), "sign out with nothing stored");
  assert.ok(!menu(SIGN_IN_ROW).includes("memql.clusters.disconnect"), "disconnect with nothing connected");
  assert.ok(menu(SIGN_IN_ROW).includes("memql.clusters.signIn"));
  assert.ok(menu(SIGN_IN_ROW).includes("memql.clusters.signInWithCode"));
  assert.ok(!menu(UNREACHABLE_ROW).includes("memql.clusters.disconnect"));
  assert.ok(!menu(NOT_CONFIGURED_ROW).includes("memql.clusters.openConsole"), "no MemQL OS without an address");
  for (const r of EVERY_ROW) {
    for (const always of ["memql.clusters.connection", "memql.clusters.edit", "memql.clusters.remove"]) {
      assert.ok(menu(r).includes(always), `${always} is missing on ${String(r["viewItem"])}`);
    }
  }
});

test("remove from list is in the context menu's last group, never inline; uninstall is not inline", () => {
  const remove = entriesFor("memql.clusters.remove");
  assert.ok(remove.length > 0, "no remove entry in view/item/context");
  for (const entry of remove) {
    assert.ok(!(entry.group ?? "").startsWith("inline"), "remove is inline again");
  }
  const clusterGroups = itemMenu
    .filter((entry) => (entry.when ?? "").includes("memqlClusters") && !(entry.group ?? "").startsWith("inline"))
    .map((entry) => entry.group ?? "");
  const last = [...clusterGroups].sort().at(-1);
  assert.ok(remove.every((entry) => entry.group === last), `remove is not in the last group (${String(last)})`);

  const uninstall = entriesFor("memql.clusters.uninstall");
  assert.ok(
    uninstall.every((entry) => !(entry.group ?? "").startsWith("inline")),
    "uninstall is contributed inline -- an irreversible action must not sit under the cursor"
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
