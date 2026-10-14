import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative, sep } from "node:path";
import { describe, expect, it } from "vitest";

// THE OS NEVER CREATES A TEMPLATE OR SAVES A LAYOUT (memql#6033, spec 1.6,
// Rulings 9 and 24).
//
// A template is written in the editor and published from there; a layout is
// saved from a finished email in the editor. The OS reads both, previews them,
// and puts them away or brings them back -- and that is all. So the two save
// calls appear in exactly one place each, the lifecycle hooks in
// apps/email/actions.ts, and every action either passes is archive or restore.
// A placeholder draft ("Your subject", "Your message") written from a list page
// is what this pins against.
//
// THE REACHABLE POSITIVE: the scan finds the hooks' own calls, so an empty list
// of offenders is evidence about the tree, not about the pattern; and a planted
// save is caught wherever it is put.

const SRC = join(__dirname, "..", "..", "..", "src");

interface Source {
  /** The path under src/, with forward slashes. */
  path: string;
  text: string;
}

function walk(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) out.push(...walk(path));
    else if (/\.(ts|tsx)$/.test(name)) out.push(path);
  }
  return out;
}

function sources(): Source[] {
  return walk(SRC).map((path) => ({ path: relative(SRC, path).split(sep).join("/"), text: readFileSync(path, "utf8") }));
}

/** Where each save may be called, and nowhere else: the file and the hook. */
const ALLOWED: Record<string, { path: string; hook: string }> = {
  campaignSaveTemplate: { path: "apps/email/actions.ts", hook: "useTemplateLifecycle" },
  campaignSaveLayout: { path: "apps/email/actions.ts", hook: "useLayoutLifecycle" },
};

/**
 * A call to either save, or its name as a string -- what a call that takes a
 * name (`executeNamed`) is handed. Inside its hook the name is how a refusal
 * loses the call's name in front of it; anywhere else it is a way round the
 * typed call.
 */
const SAVE = /\b(campaignSaveTemplate|campaignSaveLayout)\s*\(|["'](campaignSaveTemplate|campaignSaveLayout)["']/g;

/** The text from an opening parenthesis to the one that closes it. */
function argumentsFrom(text: string, open: number): string {
  let depth = 0;
  for (let at = open; at < text.length; at++) {
    const char = text[at];
    if (char === "(") depth++;
    else if (char === ")" && --depth === 0) return text.slice(open + 1, at);
  }
  return text.slice(open + 1);
}

/** Where a top-level function's body starts and ends: to the next top-level declaration. */
function hookSpan(text: string, hook: string): [number, number] | null {
  const start = text.search(new RegExp(`\\n(?:export )?function ${hook}\\b`));
  if (start < 0) return null;
  const rest = text.slice(start + 1).search(/\n(?:export )?(?:async )?(?:function|const|interface|type|class) /);
  return [start, rest < 0 ? text.length : start + 1 + rest];
}

/** Every save that is called somewhere it may not be, or with an action other than archive or restore. */
function offences(files: readonly Source[]): string[] {
  const out: string[] = [];
  for (const file of files) {
    for (const match of file.text.matchAll(SAVE)) {
      const name = match[1] ?? match[2] ?? "";
      const at = match.index ?? 0;
      const where = `${file.path}:${file.text.slice(0, at).split("\n").length}`;
      const allowed = ALLOWED[name]!;
      const span = file.path === allowed.path ? hookSpan(file.text, allowed.hook) : null;
      if (span === null || at < span[0] || at >= span[1]) {
        out.push(`${where}: ${name} outside ${allowed.hook}`);
        continue;
      }
      // The name inside its own hook, which calls nothing.
      if (match[1] === undefined) continue;
      const args = argumentsFrom(file.text, file.text.indexOf("(", at));
      const actions = [...args.matchAll(/\baction\s*:\s*([^,}\n]+)/g)].map((found) => found[1]!.trim());
      if (actions.length !== 1 || !/^["'](archive|restore)["']$/.test(actions[0]!)) {
        out.push(`${where}: ${name} with action ${actions.join(", ") || "(none)"}`);
      }
    }
  }
  return out;
}

describe("templates and layouts are made in the editor", () => {
  it("the OS never creates a template or saves a layout", () => {
    const tree = sources();
    expect(offences(tree)).toEqual([]);

    // The scan reaches the hooks' own calls: one archive and one restore each.
    const actions = tree.find((file) => file.path === "apps/email/actions.ts")!.text;
    for (const [name, { hook }] of Object.entries(ALLOWED)) {
      const span = hookSpan(actions, hook);
      expect(span, hook).not.toBeNull();
      const body = actions.slice(span![0], span![1]);
      const calls = [...body.matchAll(new RegExp(`\\b${name}\\s*\\(`, "g"))].map((found) => argumentsFrom(body, body.indexOf("(", found.index ?? 0)));
      expect(calls.map((args) => /action\s*:\s*["'](\w+)["']/.exec(args)?.[1])).toEqual(["archive", "restore"]);
    }
  });

  it("catches a save planted anywhere else, or with any other action", () => {
    const actions = sources().find((file) => file.path === "apps/email/actions.ts")!;
    const plantedElsewhere: Source = { path: "apps/email/templates/TemplatePage.tsx", text: 'void query.campaignSaveTemplate({ action: "save" });\n' };
    expect(offences([plantedElsewhere])).toEqual(["apps/email/templates/TemplatePage.tsx:1: campaignSaveTemplate outside useTemplateLifecycle"]);

    // Planted in the hook itself, as a save.
    const inHook = actions.text.replace(/(\nexport function useTemplateLifecycle\b[^\n]*\n)/, '$1  void query.campaignSaveTemplate({ action: "save" });\n');
    expect(inHook).not.toBe(actions.text);
    expect(offences([{ ...actions, text: inHook }])).toEqual([expect.stringMatching(/campaignSaveTemplate with action "save"$/)]);

    // A layout saved, or one whose action is decided at run time.
    const asLayout = actions.text.replace(/(\nexport function useLayoutLifecycle\b[^\n]*\n)/, "$1  void query.campaignSaveLayout({ layoutId, action: chosen });\n");
    expect(offences([{ ...actions, text: asLayout }])).toEqual([expect.stringMatching(/campaignSaveLayout with action chosen$/)]);

    // Named to a call that takes a name, or called from another file.
    expect(offences([{ path: "apps/files/x.ts", text: 'query.executeNamed("campaignSaveLayout", call);' }])).toHaveLength(1);
    expect(offences([{ path: "apps/email/useEmail.ts", text: "await query.campaignSaveLayout({ action: \"archive\" });" }])).toHaveLength(1);
  });
});
