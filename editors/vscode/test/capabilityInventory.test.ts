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
// commandPalette entry, or one whose `when` is not "false"); on a menu, under a
// `when` other than "false"; a welcome view's button; or run by the source, as
// a tree row's, a CodeLens's or a status bar item's `command` or through
// `executeCommand`. A view is still there when it is contributed and its
// `when` is not "false". That every contributed command is also REGISTERED,
// so none of these ends in "command not found", is held by
// activation.test.ts, which is where the extension is activated.
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
    views: Record<string, { id: string; when?: string }[]>;
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
 * Whether the source runs `id`: a tree row's, a lens's or a status bar item's
 * `command`, or an `executeCommand`. A constant holding the id counts through
 * the places that run it, by the same rule. Its registration, or a mention in
 * a list, runs nothing and does not count.
 */
function runBySource(id: string, texts: readonly string[]): boolean {
  const runs = (expr: string): RegExp =>
    new RegExp(`(?:\\bcommand\\s*(?::|=(?!=))|\\bexecuteCommand\\s*\\()[^,;)}\\n]*?${expr}`);
  const quoted = `["'\`]${escapeRe(id)}["'\`]`;
  if (texts.some((t) => runs(quoted).test(t))) return true;
  const constants = texts.flatMap((t) =>
    [...t.matchAll(new RegExp(`\\bconst\\s+(\\w+)\\s*=\\s*${quoted}`, "g"))].map((m) => m[1] ?? ""),
  );
  return constants.some((c) => texts.some((t) => runs(`\\b${c}\\b`).test(t)));
}

function reachVia(id: string, m: Manifest, texts: readonly string[]): string[] {
  const how: string[] = [];
  const palette = (m.contributes.menus["commandPalette"] ?? []).filter((e) => e.command === id);
  if (palette.length === 0 || palette.some((e) => e.when !== "false")) how.push("palette");
  for (const [menu, entries] of Object.entries(m.contributes.menus)) {
    if (menu !== "commandPalette" && entries.some((e) => e.command === id && e.when !== "false")) how.push(menu);
  }
  if (m.contributes.viewsWelcome.some((w) => w.contents.includes(`(command:${id})`))) how.push("welcome");
  if (runBySource(id, texts)) how.push("source");
  return how;
}

test("every command contributed before the redesign is still contributed", () => {
  const now = new Set(manifest().contributes.commands.map((c) => c.command));
  const missing = COMMANDS_BEFORE.filter((id) => !now.has(id) && REMOVED[id] === undefined);
  assert.deepEqual(missing, [], "a command was dropped; restore it, or add it to REMOVED with the reason");
});

test("every view contributed before the redesign is still contributed, and can be shown", () => {
  const now = new Set(
    Object.values(manifest().contributes.views)
      .flat()
      .filter((v) => v.when !== "false")
      .map((v) => v.id),
  );
  const missing = VIEWS_BEFORE.filter((id) => !now.has(id) && REMOVED[id] === undefined);
  assert.deepEqual(missing, [], "a view was dropped or hidden; restore it, or add it to REMOVED with the reason");
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
  // ...and its negative: registration alone is not a way in, nor is a mention.
  assert.equal(runBySource("memql.example", ['commands.registerCommand("memql.example", () => {});']), false);
  assert.equal(
    runBySource("memql.example", ['export const RUN = "memql.example";', "commands.registerCommand(RUN, run);"]),
    false,
  );
  assert.equal(runBySource("memql.example", ['const HIDDEN = ["memql.example"];']), false);
  assert.equal(runBySource("memql.example", ['const RUN = "memql.example";', "lens({ command: RUN });"]), true);
  assert.equal(runBySource("memql.example", ['void commands.executeCommand(ok ? "memql.other" : "memql.example");']), true);
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
