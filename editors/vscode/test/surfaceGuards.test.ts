// The two renames this epic makes, guarded so they cannot creep back.
//
// Both are the same kind of claim: a thing was replaced EVERYWHERE, and the
// value of having done it is exactly the "everywhere". One surface left on the
// old renderer keeps the readability complaint alive in one place, and one
// command left under the old id is a keybinding that silently stops working.
//
// SCANNED FROM SOURCE, not from behaviour. A rendering test only covers the
// branches a fixture happens to reach; a surface nobody wrote a fixture for is
// exactly where an old renderer survives. This is the same reasoning view-kit's
// own stylesheet guard states, and it catches the same class of thing.
//
// Refs: #3754 #3752 #3751 #3747

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

// dist-test/test/<name>.js, so the package root is two levels up.
const ROOT = path.resolve(__dirname, "..", "..");
const MANIFEST = path.join(ROOT, "package.json");

interface Manifest {
  contributes: {
    commands: { command: string; title: string }[];
    views: Record<string, { id: string; name: string }[]>;
    menus: Record<string, { command: string; when?: string }[]>;
  };
}

const manifest = JSON.parse(fs.readFileSync(MANIFEST, "utf8")) as Manifest;

/** Every .ts under the directories that ship or drive the extension. */
function sources(): { file: string; text: string }[] {
  const out: { file: string; text: string }[] = [];
  for (const dir of ["src", "test-host"]) {
    walk(path.join(ROOT, dir), out);
  }
  return out;
}

function walk(dir: string, out: { file: string; text: string }[]): void {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) walk(full, out);
    else if (entry.name.endsWith(".ts")) {
      out.push({ file: path.relative(ROOT, full), text: fs.readFileSync(full, "utf8") });
    }
  }
}

// -----------------------------------------------------------------------------
// One renderer for every value surface
// -----------------------------------------------------------------------------

test("nothing imports or calls renderDetail", () => {
  // `detail.ts` is deleted (memql#3751), so this guards against it coming BACK
  // -- which is what would happen if someone reached for the old name and
  // recreated it rather than finding renderValueView.
  const offenders = sources()
    .filter(({ text }) => text.includes("renderDetail"))
    .map(({ file }) => file);
  assert.deepEqual(
    offenders,
    [],
    `these files still name renderDetail: ${offenders.join(", ")}. The value viewer replaced it; the old renderer had no collapsing, no type badges, no filter and no bound on a large payload.`,
  );
});

test("no surface renders a value as stringified JSON in a pre", () => {
  // THE ACTUAL COMPLAINT this epic answers. `<pre>{JSON.stringify(...)}</pre>`
  // is the shape every unreadable value surface had, and three of them were
  // converted: the row detail, the run result's raw pane, and an automation
  // trace's raw pane and per-step output. A fourth appearing is the complaint
  // coming back somewhere new.
  //
  // Scoped to the webview layer, which is what renders. `JSON.stringify` in a
  // state module is serialisation for the wire or for a file, which is a
  // different thing entirely.
  const offenders = sources()
    .filter(({ file }) => file.startsWith(path.join("src", "webview")))
    .filter(({ text }) => stripComments(text).includes("JSON.stringify"))
    .map(({ file }) => file);
  assert.deepEqual(
    offenders,
    [],
    `these webview files stringify a value for display: ${offenders.join(", ")}. Render it through renderValueView instead -- it collapses, badges types, filters, and bounds a large payload.`,
  );
});

/**
 * Comments are stripped before the JSON.stringify scan.
 *
 * A comment EXPLAINING why a surface no longer stringifies is not a surface
 * that stringifies, and the honest comment on runPanel.ts says the phrase
 * verbatim. A guard that counted it would force the explanation out of the
 * code, which is the opposite of what it is for.
 */
function stripComments(source: string): string {
  return source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^[ \t]*\/\/.*$/gm, "");
}

// -----------------------------------------------------------------------------
// Nothing runs automatically
// -----------------------------------------------------------------------------

test("no training surface is triggered by a save", () => {
  // memql#3761's hardest rule to test, because it is an ABSENCE: no
  // promote-on-save, no train-on-save. The extension already holds this line
  // for runs -- `capabilities.untrustedWorkspaces` records it in the manifest --
  // and promotion is a strictly larger commitment than a run.
  //
  // Scoped to the training modules rather than the whole extension, because a
  // save subscription elsewhere is legitimate (a formatter, a watcher). Here it
  // is not: state comes from the BUFFER, so a save is by definition a moment at
  // which nothing about it changed.
  const offenders = sources()
    .filter(
      ({ file }) =>
        [
          path.join("src", "state", "training.ts"),
          path.join("src", "constructs", "decorations.ts"),
          path.join("src", "constructs", "trainingLens.ts"),
          path.join("src", "constructs", "gutterIcons.ts"),
        ].includes(file) ||
        // The ACTIONS, added by memql#3763. The rule matters more here than in
        // any of the modules above: those only render, and these ones promote.
        file.startsWith(path.join("src", "training") + path.sep),
    )
    .filter(({ text }) => /onDidSaveTextDocument|onWillSaveTextDocument/.test(stripComments(text)))
    .map(({ file }) => file);
  assert.deepEqual(
    offenders,
    [],
    `these training modules subscribe to a save event: ${offenders.join(", ")}. Saving is not promoting -- that is the one idea this surface exists to convey.`,
  );
});

test("the training lens offers the four actions, and each one is contributed AND registered", () => {
  // THE FLIP AND THE REGISTRATION ARE ONE CHANGE BY CONSTRUCTION (memql#3763).
  // A lens posting to an unregistered command fails with "command not found",
  // and a click that does nothing teaches a developer the extension is broken --
  // which is worse than the button not being there yet. So this asserts the
  // three halves together: the lens emits the action lenses, the manifest
  // declares the commands, and activation registers them.
  const lens = sources().find(
    ({ file }) => file === path.join("src", "constructs", "trainingLens.ts"),
  );
  assert.ok(
    lens?.text.includes("offerActions: true") === true,
    "the training CodeLens no longer offers the action lenses",
  );

  const declared = new Set(manifest.contributes.commands.map((c) => c.command));
  const palette = new Map(
    (manifest.contributes.menus.commandPalette ?? []).map((e) => [e.command, e.when]),
  );
  const activation = sources().find(({ file }) => file.endsWith("extension.ts"));
  assert.notEqual(activation, undefined);

  // The ids are spelled out rather than imported, on purpose: they cross into
  // package.json, which no TypeScript import reaches. A rename that updated the
  // constant and left the manifest behind is exactly what this catches.
  for (const [id, constant] of [
    ["memql.training.dryRun", "COMMAND_DRY_RUN"],
    ["memql.training.tryInSession", "COMMAND_TRY_IN_SESSION"],
    ["memql.training.promote", "COMMAND_PROMOTE"],
    ["memql.training.demote", "COMMAND_DEMOTE"],
  ] as const) {
    assert.ok(declared.has(id), `${id} is not contributed`);
    assert.ok(
      activation?.text.includes(`registerCommand(${constant}`) === true,
      `${id} is contributed but never registered in extension.ts`,
    );
    // Palette-hidden: each takes the lens's {uri, name} payload, which the
    // palette cannot supply, so a visible entry would offer a command whose only
    // possible outcome is a silent no-op.
    assert.equal(palette.get(id), "false", `${id} must be hidden from the command palette`);
  }

  // The fifth is the status bar's click-through (design §4). It IS in the
  // palette, and the difference is exactly why it can be: it navigates and
  // submits nothing, and it takes no argument that could be missing.
  assert.ok(declared.has("memql.training.showList"), "the click-through is not contributed");
  assert.ok(
    activation?.text.includes("registerCommand(COMMAND_SHOW_LIST") === true,
    "the click-through is contributed but never registered in extension.ts",
  );
  assert.equal(palette.get("memql.training.showList"), "isWorkspaceTrusted");
});

test("the status-bar item posts the click-through, and the list is not a second read", () => {
  // Design §4: "a status-bar item ... clicking through to a list. This is what
  // makes 'I saved but did not promote' impossible to miss." An item with no
  // command is a notice; the command is what makes it a way back.
  //
  // The second half is the one worth guarding. The list must be the set the
  // COUNT was computed from -- a fresh fetch on click could answer differently
  // from the number the developer just clicked, which is a race inside the very
  // surface built to stop the gutter and the status bar from disagreeing. So the
  // handler reads the retained paint and never the server.
  const decorations = sources().find(
    ({ file }) => file === path.join("src", "constructs", "decorations.ts"),
  );
  assert.ok(decorations !== undefined);
  assert.ok(
    decorations.text.includes("this.status.command = COMMAND_SHOW_LIST"),
    "the status-bar item posts no command, so the count clicks through to nothing",
  );
  const body = stripComments(decorations.text);
  const from = body.indexOf("async showList(");
  assert.ok(from > 0, "showList is gone; the status bar posts a command nothing implements");
  const rest = body.slice(from);
  const nextMethod = rest.indexOf("\n  private ");
  const scoped = nextMethod < 0 ? rest : rest.slice(0, nextMethod);
  assert.equal(
    /this\.fetch\(|sendRequest/.test(scoped),
    false,
    "showList reads the server again; it must list the constructs the count was taken from",
  );
});

test("no file spells a training command id as a literal except the one that declares it", () => {
  // The ids live in src/state/training.ts, which is where the LENS reads them
  // from. A second spelling anywhere else is a pair of strings that must agree
  // and can silently disagree -- and the failure it produces is a lens button
  // that does nothing.
  const offenders = sources()
    .filter(({ file }) => file !== path.join("src", "state", "training.ts"))
    .filter(({ text }) => /["']memql\.training\.[a-zA-Z]+["']/.test(stripComments(text)))
    .map(({ file }) => file);
  assert.deepEqual(
    offenders,
    [],
    `these files spell a training command id as a literal: ${offenders.join(", ")}. Import the constant from src/state/training.ts instead.`,
  );
});

// -----------------------------------------------------------------------------
// Concepts became Data
// -----------------------------------------------------------------------------

test("the manifest contributes Data, and not Concepts", () => {
  const views = manifest.contributes.views["memql"] ?? [];
  const ids = views.map((v) => v.id);
  assert.ok(ids.includes("memqlData"), `no memqlData view: ${ids.join(", ")}`);
  assert.equal(ids.includes("memqlConcepts"), false, "memqlConcepts came back");

  const data = views.find((v) => v.id === "memqlData");
  // The rename is the point: the old view never showed a concept's DEFINITION,
  // it showed rows. Constructs shows definitions now, so a "Concepts" view
  // beside it overlapped by name.
  assert.equal(data?.name, "Data");
});

test("no memql.concepts.* command survives, anywhere", () => {
  const stale = manifest.contributes.commands
    .map((c) => c.command)
    .filter((c) => c.startsWith("memql.concepts."));
  assert.deepEqual(stale, [], `stale command ids: ${stale.join(", ")}`);

  for (const [menu, entries] of Object.entries(manifest.contributes.menus)) {
    const inMenu = entries
      .map((e) => e.command)
      .filter((c) => c.startsWith("memql.concepts."));
    assert.deepEqual(inMenu, [], `stale command ids in ${menu}: ${inMenu.join(", ")}`);
  }

  const inSource = sources()
    .filter(({ text }) => /memql\.concepts\./.test(text))
    .map(({ file }) => file);
  assert.deepEqual(
    inSource,
    [],
    `these files still name a memql.concepts.* command: ${inSource.join(", ")}`,
  );
});

test("every command the Data view contributes is registered under the new id", () => {
  // The failure this catches is half a rename: the manifest renamed, the
  // registration not -- which produces a command that is contributed, visible,
  // and fails with "command not found" when pressed.
  const declared = new Set(manifest.contributes.commands.map((c) => c.command));
  for (const id of ["memql.data.refresh", "memql.data.open"]) {
    assert.ok(declared.has(id), `${id} is not contributed`);
  }
  const activation = sources().find(({ file }) => file.endsWith("extension.ts"));
  assert.notEqual(activation, undefined);
  for (const id of ["memql.data.refresh", "memql.data.open"]) {
    assert.ok(
      activation?.text.includes(`'${id}'`) === true,
      `${id} is contributed but never registered in extension.ts`,
    );
  }
});
