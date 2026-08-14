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
