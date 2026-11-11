import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative, sep } from "node:path";
import { describe, expect, it } from "vitest";

// THE OS NEVER CREATES A TEMPLATE OR SAVES A LAYOUT (memql#6033, spec 1.6,
// Rulings 9 and 24).
//
// A template is written in the editor and published from there; a layout is
// saved from a finished email in the editor. The OS reads templates, previews
// them, and puts them away or brings them back -- and that is all. So the
// template save appears in exactly one place, the lifecycle hook in
// apps/email/actions.ts, and every action it passes is archive or restore --
// or a save that names the template's purpose (X10), which sends back the
// document read whole, by id, at the revision on screen, and writes no content
// of the OS's own. A save that names no purpose is still an offence. The
// layout save appears nowhere: the OS has no layout lifecycle at all (the
// Layouts view is gone, and a layout is only ever read, to name the one a
// template was made from). A placeholder draft ("Your subject", "Your message")
// written from a list page is what this pins against.
//
// THE REACHABLE POSITIVE: the scan finds the hook's own call, so an empty list
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

/** Where each save may be called, and nowhere else: the file and the hook. A save not listed may be called nowhere. */
const ALLOWED: Record<string, { path: string; hook: string }> = {
  campaignSaveTemplate: { path: "apps/email/actions.ts", hook: "useTemplateLifecycle" },
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
      const allowed = ALLOWED[name];
      if (allowed === undefined) {
        out.push(`${where}: ${name} is called nowhere`);
        continue;
      }
      const span = file.path === allowed.path ? hookSpan(file.text, allowed.hook) : null;
      if (span === null || at < span[0] || at >= span[1]) {
        out.push(`${where}: ${name} outside ${allowed.hook}`);
        continue;
      }
      // The name inside its own hook, which calls nothing.
      if (match[1] === undefined) continue;
      const args = argumentsFrom(file.text, file.text.indexOf("(", at));
      const actions = [...args.matchAll(/\baction\s*:\s*([^,}\n]+)/g)].map((found) => found[1]!.trim());
      const purposeSave = actions.length === 1 && actions[0] === '"save"' && /\bpurpose\s*:/.test(args);
      if (!purposeSave && (actions.length !== 1 || !/^["'](archive|restore)["']$/.test(actions[0]!))) {
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

    // The scan reaches the hook's own calls: one archive, one restore and the purpose's save.
    const actions = tree.find((file) => file.path === "apps/email/actions.ts")!.text;
    for (const [name, { hook }] of Object.entries(ALLOWED)) {
      const span = hookSpan(actions, hook);
      expect(span, hook).not.toBeNull();
      const body = actions.slice(span![0], span![1]);
      const calls = [...body.matchAll(new RegExp(`\\b${name}\\s*\\(`, "g"))].map((found) => argumentsFrom(body, body.indexOf("(", found.index ?? 0)));
      expect(calls.map((args) => /action\s*:\s*["'](\w+)["']/.exec(args)?.[1])).toEqual(["archive", "restore", "save"]);
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

    // A template whose action is decided at run time.
    const decided = actions.text.replace(/(\nexport function useTemplateLifecycle\b[^\n]*\n)/, "$1  void query.campaignSaveTemplate({ templateId, action: chosen });\n");
    expect(offences([{ ...actions, text: decided }])).toEqual([expect.stringMatching(/campaignSaveTemplate with action chosen$/)]);

    // A layout saved anywhere, by call or by name, is an offence: the OS has no layout lifecycle.
    expect(offences([{ path: "apps/files/x.ts", text: 'query.executeNamed("campaignSaveLayout", call);' }])).toEqual(["apps/files/x.ts:1: campaignSaveLayout is called nowhere"]);
    expect(offences([{ ...actions, text: actions.text + '\nvoid query.campaignSaveLayout({ action: "archive" });\n' }])).toHaveLength(1);
    expect(offences([{ path: "apps/email/useEmail.ts", text: "await query.campaignSaveLayout({ action: \"archive\" });" }])).toHaveLength(1);
  });

  it("stopping and restarting an email being written are called from their own hooks and nowhere else", () => {
    const allowed: Record<string, string> = { campaignCancelComposition: "useCancelComposition", campaignRetryComposition: "useRetryComposition" };
    const tree = sources();
    for (const file of tree) {
      for (const match of file.text.matchAll(/\b(campaignCancelComposition|campaignRetryComposition)\b/g)) {
        expect(file.path, `${match[1]} in ${file.path}`).toBe("apps/email/actions.ts");
      }
    }
    // The hooks exist, and each names its own builtin: the scan reaches them.
    const actions = tree.find((file) => file.path === "apps/email/actions.ts")!.text;
    for (const [name, hook] of Object.entries(allowed)) {
      const span = hookSpan(actions, hook);
      expect(span, hook).not.toBeNull();
      expect(actions.slice(0, span![0]) + actions.slice(span![1])).toContain(name);
      expect(actions.slice(span![0], span![1])).toContain("useComposition(");
    }
  });
});
