// The extension's user-facing copy, held to the voice the design record sets
// (docs/internal/design/2026-09-28-vscode-extension-ux.md, "Audience and
// voice").
//
// THREE SOURCES, because copy reaches a person three ways:
//   1. package.json: every string it contributes (command titles, welcome
//      views, view names, setting descriptions and the rest), the Restricted
//      Mode line and the extension's own name and description.
//   2. Every gallery scenario's visible text, in both themes: the pages as the
//      editor draws them, with tags, scripts and styles stripped.
//   3. The string literals a source file hands straight to a toast (directly
//      or through offerDetails), an input box's prompt or title, or a progress
//      notification's title.
//
// WHAT FAILS: an issue reference, " -- " as punctuation, "portal" or
// "console" (MemQL OS is MemQL OS), internal vocabulary a reader cannot act on
// (refresh_token, client_id, SecretStorage, receipt, envelope, stderr, exit
// code, install graph, capability), a command title carrying its own "MemQL: "
// prefix (the category says it), and a lede or notice line over 25 words.
//
// A LINE is a welcome view's line, the Restricted Mode line, a toast, and on a
// page a notice, a lede, an empty state or the action bar's confirmation.
// Setting descriptions and the extension's description are held to the
// vocabulary only: they are reference text, not a line on a screen.
//
// An exemption goes in ALLOWED with its reason, and one that matches nothing
// fails the last case here, so the list cannot outlive the copy it excused.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import { GALLERY_THEMES } from "../gallery/harness.js";
import { SCENARIOS } from "../gallery/index.js";

// dist-test/test/<name>.js -> the package root.
const PKG = path.resolve(__dirname, "..", "..");

const MAX_LINE_WORDS = 25;

interface Rule {
  name: string;
  re: RegExp;
}

const RULES: readonly Rule[] = [
  // memql#12, memql-cockpit#12, a bare #12 in prose, or a link to one.
  { name: "issue reference", re: /\b[a-z][\w.-]*#\d+\b|(?<![\w&/])#\d+\b|\/(issues|pull)\/\d+/i },
  { name: "-- as punctuation", re: /(^|\s)--(\s|$)/ },
  { name: "portal", re: /\bportals?\b/i },
  { name: "console", re: /\bconsoles?\b/i },
  { name: "refresh_token", re: /refresh_token/i },
  { name: "client_id", re: /client_id/i },
  { name: "SecretStorage", re: /secret\s?storage/i },
  { name: "receipt", re: /\breceipts?\b/i },
  { name: "envelope", re: /\benvelopes?\b/i },
  { name: "stderr", re: /\bstderr\b/i },
  { name: "exit code", re: /\bexit codes?\b/i },
  { name: "install graph", re: /\binstall graphs?\b/i },
  { name: "capability", re: /\bcapabilit(y|ies)\b/i },
];

/** One piece of copy and where it came from. */
interface Copy {
  where: string;
  text: string;
  /** Held to the line length as well as the vocabulary. */
  line?: boolean;
  /** A command title, which must not repeat its category. */
  title?: boolean;
}

/**
 * A genuine exception: the copy at `where` may contain `contains`, which trips
 * `rule`. Each says why, and each must still match something.
 */
interface Allowed {
  where: string;
  rule: string;
  contains: string;
  reason: string;
}

const ALLOWED: readonly Allowed[] = [
  {
    where: "gallery author-language-",
    rule: "-- as punctuation",
    contains: "-- do not edit",
    reason: "The grammar's own header comment, shown verbatim as the cluster serves it. It is data, not the extension's words.",
  },
];

// ---------------------------------------------------------------------------
// 1. package.json
// ---------------------------------------------------------------------------

interface Manifest {
  displayName: string;
  description: string;
  capabilities: { untrustedWorkspaces: { description: string } };
  contributes: unknown;
}

/**
 * Keys whose strings are identifiers, paths, conditions or enum values, never
 * words a person reads. Every other string under `contributes` is copy, so a
 * key added later (a setting's markdownDescription, a submenu's label) is read
 * without anyone having to list it here.
 */
const NOT_COPY: ReadonlySet<string> = new Set([
  "command",
  "configuration",
  "dark",
  "default",
  "enum",
  "extensions",
  "group",
  "icon",
  "id",
  "language",
  "light",
  "path",
  "scope",
  "scopeName",
  "submenu",
  "type",
  "uiTheme",
  "view",
  "when",
]);

/** Whole subtrees with no copy in them: TextMate scope names. */
const NOT_COPY_TREES: ReadonlySet<string> = new Set(["semanticTokenScopes"]);

function manifestCopy(): Copy[] {
  const manifest = JSON.parse(fs.readFileSync(path.join(PKG, "package.json"), "utf8")) as Manifest;
  const out: Copy[] = [
    { where: "package.json displayName", text: manifest.displayName },
    { where: "package.json description", text: manifest.description },
    {
      where: "package.json untrustedWorkspaces",
      text: manifest.capabilities.untrustedWorkspaces.description,
      line: true,
    },
  ];
  collectManifest(manifest.contributes, "contributes", "", out);
  return out;
}

/**
 * Every string under `value`, as copy. An array entry is named by its command,
 * id or view where it has one, so a finding says which entry it is.
 */
function collectManifest(value: unknown, where: string, key: string, out: Copy[]): void {
  if (typeof value === "string") {
    if (NOT_COPY.has(key)) return;
    if (key === "contents") {
      // A welcome view: each line is a line, and a button line is its label;
      // the command it names is not copy.
      for (const raw of value.split("\n")) {
        const text = raw.replace(/\[([^\]]*)\]\([^)]*\)/g, "$1").trim();
        if (text !== "") out.push({ where, text, line: true });
      }
      return;
    }
    const title = key === "title" && where.startsWith("contributes.commands[");
    out.push({ where, text: value, title });
    return;
  }
  if (Array.isArray(value)) {
    value.forEach((entry: unknown, i) => {
      const named = entry !== null && typeof entry === "object" ? (entry as Record<string, unknown>) : {};
      const name = [named["command"], named["id"], named["view"]].find((v) => typeof v === "string");
      collectManifest(entry, `${where}[${i}${name === undefined ? "" : ` ${String(name)}`}]`, key, out);
    });
    return;
  }
  if (value !== null && typeof value === "object") {
    for (const [k, v] of Object.entries(value)) {
      if (!NOT_COPY_TREES.has(k)) collectManifest(v, `${where}.${k}`, k, out);
    }
  }
}

// ---------------------------------------------------------------------------
// 2. the gallery
// ---------------------------------------------------------------------------

const ENTITIES: Readonly<Record<string, string>> = {
  amp: "&",
  lt: "<",
  gt: ">",
  quot: '"',
  apos: "'",
  nbsp: " ",
  middot: "·",
};

function decode(text: string): string {
  return text.replace(/&(#x[0-9a-f]+|#\d+|[a-z]+);/gi, (whole, ref: string) => {
    if (ref.startsWith("#x") || ref.startsWith("#X")) return String.fromCodePoint(parseInt(ref.slice(2), 16));
    if (ref.startsWith("#")) return String.fromCodePoint(parseInt(ref.slice(1), 10));
    return ENTITIES[ref.toLowerCase()] ?? whole;
  });
}

/** The page's visible text runs: no head, script, style or comment. */
function visibleText(html: string): string[] {
  const body = html
    .replace(/<head[\s\S]*?<\/head>/gi, " ")
    .replace(/<script[\s\S]*?<\/script>/gi, " ")
    .replace(/<style[\s\S]*?<\/style>/gi, " ")
    .replace(/<!--[\s\S]*?-->/g, " ");
  return body
    .split(/<[^>]+>/)
    .map((run) => decode(run).replace(/\s+/g, " ").trim())
    .filter((run) => run !== "");
}

/** The classes whose paragraph is a line: a notice, a lede, an empty state, a confirmation. */
const LINE_CLASS = /\b(mq-notice-line|mq-notice-next|mq-empty-line|mq-actbar-confirm|ac-line|ac-empty|[\w-]+-lede)\b/;

function pageLines(html: string): string[] {
  const out: string[] = [];
  for (const m of html.matchAll(/<p class="([^"]*)"[^>]*>([\s\S]*?)<\/p>/g)) {
    if (!LINE_CLASS.test(m[1] ?? "")) continue;
    out.push(decode((m[2] ?? "").replace(/<[^>]+>/g, " ")).replace(/\s+/g, " ").trim());
  }
  return out;
}

function galleryCopy(): Copy[] {
  const out: Copy[] = [];
  for (const scenario of SCENARIOS) {
    for (const theme of GALLERY_THEMES) {
      const html = scenario.render(theme);
      const where = `gallery ${scenario.id} (${theme})`;
      for (const text of visibleText(html)) out.push({ where, text });
      for (const text of pageLines(html)) out.push({ where, text, line: true });
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// 3. toasts, input boxes and progress titles, read off the source
// ---------------------------------------------------------------------------

function sourceFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...sourceFiles(full));
    else if (entry.name.endsWith(".ts")) out.push(full);
  }
  return out;
}

/**
 * The arguments of the call whose `(` is at `open`, split at top-level commas.
 * Strings, template literals and nesting are honoured; comments are not
 * expected inside a call's argument list.
 */
function callArguments(src: string, open: number): string[] {
  const args: string[] = [];
  let depth = 0;
  let start = open + 1;
  for (let i = open; i < src.length; i++) {
    const ch = src[i];
    if (ch === '"' || ch === "'" || ch === "`") {
      i = skipString(src, i);
      continue;
    }
    if (ch === "(" || ch === "[" || ch === "{") depth++;
    else if (ch === ")" || ch === "]" || ch === "}") {
      depth--;
      if (depth === 0) {
        args.push(src.slice(start, i));
        return args.map((a) => a.trim()).filter((a) => a !== "");
      }
    } else if (ch === "," && depth === 1) {
      args.push(src.slice(start, i));
      start = i + 1;
    }
  }
  return args;
}

/** The index of the quote that closes the string opening at `i`. */
function skipString(src: string, i: number): number {
  const quote = src[i];
  for (let j = i + 1; j < src.length; j++) {
    if (src[j] === "\\") {
      j++;
      continue;
    }
    if (quote === "`" && src[j] === "$" && src[j + 1] === "{") {
      let depth = 0;
      for (let k = j + 1; k < src.length; k++) {
        if (src[k] === "{") depth++;
        else if (src[k] === "}" && --depth === 0) {
          j = k;
          break;
        } else if (src[k] === '"' || src[k] === "'" || src[k] === "`") k = skipString(src, k);
      }
      continue;
    }
    if (src[j] === quote) return j;
  }
  return src.length;
}

/**
 * The text of an argument made only of string literals (joined by `+`), with
 * each `${...}` of a template standing in as "X". Undefined for anything else:
 * a variable is not a literal passed directly.
 */
function literalText(arg: string): string | undefined {
  const parts: string[] = [];
  let i = 0;
  const s = arg.trim();
  while (i < s.length) {
    const ch = s[i];
    if (ch === '"' || ch === "'" || ch === "`") {
      const end = skipString(s, i);
      const body = s.slice(i + 1, end);
      parts.push(ch === "`" ? body.replace(/\$\{[\s\S]*?\}/g, "X") : body.replace(/\\(.)/g, "$1"));
      i = end + 1;
    } else if (/\s|\+/.test(ch ?? "")) {
      i++;
    } else {
      return undefined;
    }
  }
  return parts.length === 0 ? undefined : parts.join("");
}

/**
 * The string literals standing at the top level of an argument that is not
 * one: the fallback in `err.message ?? "Couldn't connect."`, both arms of a
 * conditional. Each is copy a person can be shown, read for its words alone.
 */
function literalFragments(arg: string): string[] {
  const out: string[] = [];
  let depth = 0;
  for (let i = 0; i < arg.length; i++) {
    const ch = arg[i];
    if (ch === '"' || ch === "'" || ch === "`") {
      const end = skipString(arg, i);
      if (depth === 0) out.push(literalText(arg.slice(i, end + 1)) ?? "");
      i = end;
    } else if (ch === "(" || ch === "[" || ch === "{") depth++;
    else if (ch === ")" || ch === "]" || ch === "}") depth--;
  }
  return out.filter((text) => text.trim() !== "");
}

/** The literal value of `key:` at the top level of an object-literal argument. */
function propertyText(arg: string, key: string): string | undefined {
  if (!arg.startsWith("{")) return undefined;
  for (const inner of callArguments(arg, 0)) {
    const m = new RegExp(`^${key}\\s*:\\s*([\\s\\S]*)$`).exec(inner);
    if (m) return literalText(m[1] ?? "");
  }
  return undefined;
}

function sourceCopy(): Copy[] {
  const out: Copy[] = [];
  for (const file of sourceFiles(path.join(PKG, "src"))) {
    const src = fs.readFileSync(file, "utf8");
    const rel = path.relative(PKG, file);
    const at = (index: number): string => `${rel}:${src.slice(0, index).split("\n").length}`;
    // offerDetails (src/extension.ts) is the error-toast policy: its third
    // argument is the toast, and the rest are its buttons.
    for (const m of src.matchAll(/(?<!function\s+)\b(show(Information|Warning|Error)Message|offerDetails)\s*\(/g)) {
      const args = callArguments(src, (m.index ?? 0) + m[0].length - 1).slice(m[1] === "offerDetails" ? 2 : 0);
      args.forEach((arg, n) => {
        const text = literalText(arg);
        if (text !== undefined) out.push({ where: at(m.index ?? 0), text, line: n === 0 });
        else for (const fragment of literalFragments(arg)) out.push({ where: at(m.index ?? 0), text: fragment });
        const detail = propertyText(arg, "detail");
        if (detail !== undefined) out.push({ where: at(m.index ?? 0), text: detail, line: true });
      });
    }
    for (const m of src.matchAll(/\bshowInputBox\s*\(/g)) {
      const [options] = callArguments(src, (m.index ?? 0) + m[0].length - 1);
      for (const key of ["prompt", "title"]) {
        const text = options === undefined ? undefined : propertyText(options, key);
        if (text !== undefined) out.push({ where: at(m.index ?? 0), text, line: true });
      }
    }
    for (const m of src.matchAll(/\bwithProgress\s*\(/g)) {
      const [options] = callArguments(src, (m.index ?? 0) + m[0].length - 1);
      const text = options === undefined ? undefined : propertyText(options, "title");
      if (text !== undefined) out.push({ where: at(m.index ?? 0), text, line: true });
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// the check
// ---------------------------------------------------------------------------

const used = new Set<Allowed>();

function exempt(copy: Copy, rule: string): boolean {
  const entry = ALLOWED.find((a) => a.rule === rule && copy.where.startsWith(a.where) && copy.text.includes(a.contains));
  if (entry !== undefined) used.add(entry);
  return entry !== undefined;
}

function problems(copy: readonly Copy[]): string[] {
  const out = new Set<string>();
  for (const c of copy) {
    for (const rule of RULES) {
      if (rule.re.test(c.text) && !exempt(c, rule.name)) out.add(`${c.where}: ${rule.name}: "${c.text}"`);
    }
    if (c.title === true && /\bMemQL:\s/.test(c.text) && !exempt(c, "MemQL: in a title")) {
      out.add(`${c.where}: MemQL: in a title: "${c.text}"`);
    }
    const words = c.text.split(/\s+/).filter((w) => w !== "").length;
    if (c.line === true && words > MAX_LINE_WORDS && !exempt(c, "line length")) {
      out.add(`${c.where}: line length (${words} words): "${c.text}"`);
    }
  }
  return [...out];
}

function assertClean(copy: readonly Copy[], source: string): void {
  assert.ok(copy.length > 0, `no copy was read from ${source}, so this case checks nothing`);
  const found = problems(copy);
  assert.deepEqual(found, [], `${source} carries copy the design record rules out:\n  ${found.join("\n  ")}`);
}

test("package.json copy is plain", () => {
  assertClean(manifestCopy(), "package.json");
});

test("every gallery page's visible text is plain, in both themes", () => {
  assertClean(galleryCopy(), "the gallery");
});

test("toasts, input boxes and progress titles are plain", () => {
  assertClean(sourceCopy(), "src");
});

test("the source scan reads the calls it exists for", () => {
  // The positive control: a scanner that matched nothing would pass forever.
  const sample = [
    'void window.showWarningMessage("Couldn\'t reach " + name + ".", "Retry");',
    "window.showInputBox({ title: `Sign in to ${cluster}`, prompt: 'Paste the code', value: x });",
    "window.withProgress({ location: 1, title: 'Signing in' }, async () => {});",
  ].join("\n");
  const warn = callArguments(sample, sample.indexOf("("));
  assert.deepEqual(warn.map(literalText), [undefined, "Retry"], "a concatenation with a variable is not a literal");
  assert.equal(literalText(`"Couldn't reach " + "it."`), "Couldn't reach it.");
  assert.deepEqual(literalFragments(`err?.message ?? "Couldn't connect." + f("not this")`), ["Couldn't connect."]);
  const box = callArguments(sample, sample.indexOf("showInputBox(") + "showInputBox".length)[0] ?? "";
  assert.equal(propertyText(box, "title"), "Sign in to X");
  assert.equal(propertyText(box, "prompt"), "Paste the code");
  const progress = callArguments(sample, sample.indexOf("withProgress(") + "withProgress".length)[0] ?? "";
  assert.equal(propertyText(progress, "title"), "Signing in");
  assert.ok(sourceCopy().length > 20, "the scan found the extension's toasts");
  assert.ok(
    sourceCopy().some((c) => c.text === "MemQL: Couldn't save the cluster." && c.line === true),
    "the scan reads a toast offerDetails shows",
  );
});

test("the manifest scan reads every kind of copy, and no identifier", () => {
  // The positive control for section 1: a walker that skipped a key would pass forever.
  const copy = manifestCopy();
  const texts = new Set(copy.map((c) => c.text));
  for (const expected of ["Clusters", "MemQL Dark", "Refresh Clusters", "No clusters yet."]) {
    assert.ok(texts.has(expected), `the scan did not read "${expected}"`);
  }
  assert.ok(
    copy.some((c) => c.title === true && c.text === "Refresh Clusters"),
    "a command title is read as a title",
  );
  for (const notCopy of ["isWorkspaceTrusted", "memqlClusters", "verbose", "keyword.control.memql", "navigation@0"]) {
    assert.ok(!texts.has(notCopy), `"${notCopy}" is not copy`);
  }
});

test("every exemption still matches the copy it excuses", () => {
  problems([...manifestCopy(), ...galleryCopy(), ...sourceCopy()]);
  const stale = ALLOWED.filter((a) => !used.has(a));
  assert.deepEqual(stale, [], "an exemption matches nothing; delete it");
});
