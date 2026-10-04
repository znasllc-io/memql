// Nothing the extension could do before the redesign is gone, or out of reach.
//
// The design record's brief (docs/internal/design/2026-09-28-vscode-extension-ux.md)
// is a cleaner extension "without losing a single capability". This holds it
// to that at the manifest: every command and view the pre-redesign extension
// contributed (package.json at a718700f7, embedded below so the list cannot
// drift with the thing it checks) is still contributed, and every command can
// still be reached by a person.
//
// REACHABLE means at least one of: listed in the Command Palette (no
// commandPalette entry, or one whose `when` is not "false"); on a menu; a
// welcome view's button; or named by the source as something other than its
// own registration, which is how a tree row, a CodeLens or another command
// runs it.
//
// An intentional removal goes in REMOVED with its reason. It is empty.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

// dist-test/test/<name>.js -> the package root.
const PKG = path.resolve(__dirname, "..", "..");

/** `contributes.commands[].command` at a718700f7. */
const COMMANDS_BEFORE: readonly string[] = [
  "memql.clusters.refresh",
  "memql.clusters.select",
  "memql.clusters.add",
  "memql.clusters.edit",
  "memql.clusters.remove",
  "memql.clusters.uninstall",
  "memql.constructs.refresh",
  "memql.constructs.open",
  "memql.constructs.showDetails",
  "memql.deployments.refresh",
  "memql.deployments.createDeployment",
  "memql.deployments.open",
  "memql.deployments.openRun",
  "memql.deployments.openCheckout",
  "memql.deployments.rebuildFromCheckout",
  "memql.clusters.repair",
  "memql.clusters.takeOwnership",
  "memql.clusters.signIn",
  "memql.clusters.signOut",
  "memql.clusters.disconnect",
  "memql.clusters.signInWithCode",
  "memql.clusters.connection",
  "memql.clusters.openConsole",
  "memql.data.refresh",
  "memql.data.open",
  "memql.runs.refresh",
  "memql.runs.open",
  "memql.runs.execute",
  "memql.runs.delete",
  "memql.run.construct",
  "memql.run.constructWith",
  "memql.run.automation",
  "memql.training.dryRun",
  "memql.training.tryInSession",
  "memql.training.stage",
  "memql.training.promote",
  "memql.training.demote",
  "memql.training.showList",
  "memql.clusters.refreshReleases",
  "memql.language.showReference",
];

/** `contributes.views.*[].id` at a718700f7. */
const VIEWS_BEFORE: readonly string[] = ["memqlClusters", "memqlDeployments", "memqlConstructs", "memqlData", "memqlRuns"];

/** Commands or views removed on purpose, each with why. */
const REMOVED: Readonly<Record<string, string>> = {};

interface Manifest {
  contributes: {
    commands: { command: string }[];
    views: Record<string, { id: string }[]>;
    viewsWelcome: { contents: string }[];
    menus: Record<string, { command?: string; when?: string }[]>;
  };
}

function manifest(): Manifest {
  return JSON.parse(fs.readFileSync(path.join(PKG, "package.json"), "utf8")) as Manifest;
}

function sourceFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...sourceFiles(full));
    else if (entry.name.endsWith(".ts")) out.push(full);
  }
  return out;
}

/** Every src file with its imports and comment lines taken out: what is left is code that names things. */
function sourceTexts(): string[] {
  return sourceFiles(path.join(PKG, "src")).map((file) =>
    fs
      .readFileSync(file, "utf8")
      .replace(/^import[\s\S]*?from\s+["'][^"']+["'];?/gm, "")
      .split("\n")
      .filter((line) => !/^\s*(\/\/|\*|\/\*)/.test(line))
      .join("\n"),
  );
}

function escapeRe(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/**
 * Whether the source runs `id` from somewhere other than its own registration:
 * a tree row's or a lens's `command`, an `executeCommand`. A constant holding
 * the id counts through its uses, by the same rule.
 */
function namedBySource(id: string, texts: readonly string[]): boolean {
  const quoted = new RegExp(`(registerCommand\\(\\s*)?(?:export\\s+)?(?:const\\s+(\\w+)\\s*=\\s*)?["'\`]${escapeRe(id)}["'\`]`, "g");
  for (const text of texts) {
    for (const m of text.matchAll(quoted)) {
      if (m[1] !== undefined) continue;
      const constant = m[2];
      if (constant === undefined) return true;
      const use = new RegExp(`(registerCommand\\(\\s*)?(const\\s+)?\\b${constant}\\b`, "g");
      for (const t of texts) {
        for (const u of t.matchAll(use)) if (u[1] === undefined && u[2] === undefined) return true;
      }
    }
  }
  return false;
}

function reachVia(id: string, m: Manifest, texts: readonly string[]): string[] {
  const how: string[] = [];
  const palette = (m.contributes.menus["commandPalette"] ?? []).filter((e) => e.command === id);
  if (palette.length === 0 || palette.some((e) => e.when !== "false")) how.push("palette");
  for (const [menu, entries] of Object.entries(m.contributes.menus)) {
    if (menu !== "commandPalette" && entries.some((e) => e.command === id)) how.push(menu);
  }
  if (m.contributes.viewsWelcome.some((w) => w.contents.includes(`(command:${id})`))) how.push("welcome");
  if (namedBySource(id, texts)) how.push("source");
  return how;
}

test("every command contributed before the redesign is still contributed", () => {
  const now = new Set(manifest().contributes.commands.map((c) => c.command));
  const missing = COMMANDS_BEFORE.filter((id) => !now.has(id) && REMOVED[id] === undefined);
  assert.deepEqual(missing, [], "a command was dropped; restore it, or add it to REMOVED with the reason");
});

test("every view contributed before the redesign is still contributed", () => {
  const now = new Set(Object.values(manifest().contributes.views).flat().map((v) => v.id));
  const missing = VIEWS_BEFORE.filter((id) => !now.has(id) && REMOVED[id] === undefined);
  assert.deepEqual(missing, [], "a view was dropped; restore it, or add it to REMOVED with the reason");
});

test("every contributed command can be reached by a person", () => {
  // Every command, not only the old ones: a new one nobody can reach is the
  // same loss arriving from the other side.
  const m = manifest();
  const texts = sourceTexts();
  const unreachable = m.contributes.commands.map((c) => c.command).filter((id) => reachVia(id, m, texts).length === 0);
  assert.deepEqual(unreachable, [], "hidden from the palette and on no menu, welcome view, tree row or lens");
});

test("a command run only from a tree row or a lens is seen as reachable", () => {
  // The positive control for the source rule, on commands whose only way in is
  // the source: a constructs row, a training lens through its constant.
  const m = manifest();
  const texts = sourceTexts();
  assert.deepEqual(reachVia("memql.constructs.open", m, texts), ["source"]);
  assert.deepEqual(reachVia("memql.training.dryRun", m, texts), ["source"]);
  // ...and its negative: registration alone is not a way in.
  assert.equal(namedBySource("memql.example", ['commands.registerCommand("memql.example", () => {});']), false);
  assert.equal(
    namedBySource("memql.example", ['export const RUN = "memql.example";', "commands.registerCommand(RUN, run);"]),
    false,
  );
  assert.equal(namedBySource("memql.example", ['const RUN = "memql.example";', "lens({ command: RUN });"]), true);
});

test("REMOVED names only what was there before and is gone now, each with a reason", () => {
  const m = manifest();
  const now = new Set([...m.contributes.commands.map((c) => c.command), ...Object.values(m.contributes.views).flat().map((v) => v.id)]);
  for (const [id, reason] of Object.entries(REMOVED)) {
    assert.ok(COMMANDS_BEFORE.includes(id) || VIEWS_BEFORE.includes(id), `${id} was never contributed`);
    assert.ok(!now.has(id), `${id} is still contributed; drop it from REMOVED`);
    assert.notEqual(reason.trim(), "", `${id} is removed without a reason`);
  }
});
