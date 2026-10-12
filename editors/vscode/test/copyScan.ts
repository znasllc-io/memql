// The reusable half of the extension copy lints. A copy lint holds the words a
// person reads to the voice the design record sets
// (docs/internal/design/2026-09-28-vscode-extension-ux.md, "Audience and
// voice"). This file is what the lints share: the vocabulary RULES, the shape
// of one piece of copy, how copy is read out of a package.json, a rendered page
// and the calls that raise a toast, an input box, an open or save dialog or a
// progress notification, and the check that holds it all to the rules.
//
// Each extension keeps its own lint beside its tests and decides there which
// pages and sources to read, what to exempt and what else to hold them to:
//   editors/vscode/test/copyLint.test.ts        the MemQL extension
//   editors/productivity/test/copyLint.test.ts  MemQL Productivity Tools
//
// Nothing here knows about either extension, and it takes only Node built-ins,
// so either test bundle can import it.

import * as fs from "node:fs";
import * as path from "node:path";

/** The longest a line of copy may be, in words. */
export const MAX_LINE_WORDS = 25;

/** A vocabulary rule. The pattern must not be global: it is tested against many strings. */
export interface Rule {
  name: string;
  re: RegExp;
}

export const RULES: readonly Rule[] = [
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
export interface Copy {
  where: string;
  text: string;
  /** Held to the line length as well as the vocabulary. */
  line?: boolean;
  /** Held to this many words rather than the line length: a status line. A {placeholder} is one word. */
  maxWords?: number;
  /** A command title, which must not repeat its category. */
  title?: boolean;
}

/**
 * A genuine exception: the copy at `where` may contain `contains`, which trips
 * `rule`. Each says why, and each must still match something.
 */
export interface Allowed {
  where: string;
  rule: string;
  contains: string;
  reason: string;
}

/**
 * Every problem in `copy`, one line each. A piece of copy that `allowed` excuses
 * is not a problem, and the entry that excused it is added to `used`, so the
 * caller can tell an exemption that no longer matches anything. `rules` is the
 * vocabulary the copy is held to: the shared RULES unless an extension adds its own.
 */
export function problems(copy: readonly Copy[], allowed: readonly Allowed[], used: Set<Allowed>, rules: readonly Rule[] = RULES): string[] {
  const exempt = (c: Copy, rule: string): boolean => {
    const entry = allowed.find((a) => a.rule === rule && c.where.startsWith(a.where) && c.text.includes(a.contains));
    if (entry !== undefined) used.add(entry);
    return entry !== undefined;
  };
  const out = new Set<string>();
  for (const c of copy) {
    for (const rule of rules) {
      if (rule.re.test(c.text) && !exempt(c, rule.name)) out.add(`${c.where}: ${rule.name}: "${c.text}"`);
    }
    if (c.title === true && /\bMemQL:\s/.test(c.text) && !exempt(c, "MemQL: in a title")) {
      out.add(`${c.where}: MemQL: in a title: "${c.text}"`);
    }
    const words = c.text.split(/\s+/).filter((w) => w !== "").length;
    const limit = c.maxWords ?? (c.line === true ? MAX_LINE_WORDS : undefined);
    if (limit !== undefined && words > limit && !exempt(c, "line length")) {
      const count = c.maxWords === undefined ? `${words} words` : `${words} words, at most ${c.maxWords}`;
      out.add(`${c.where}: line length (${count}): "${c.text}"`);
    }
  }
  return [...out];
}

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

/** Every string the package.json under `pkgRoot` contributes that a person can read. */
export function manifestCopy(pkgRoot: string): Copy[] {
  const manifest = JSON.parse(fs.readFileSync(path.join(pkgRoot, "package.json"), "utf8")) as Manifest;
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
// 2. rendered pages
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

/** `text` with the HTML entities a page writes decoded: &amp; &#39; &#x2019; and the named few. */
export function decode(text: string): string {
  return text.replace(/&(#x[0-9a-f]+|#\d+|[a-z]+);/gi, (whole, ref: string) => {
    if (ref.startsWith("#x") || ref.startsWith("#X")) return String.fromCodePoint(parseInt(ref.slice(2), 16));
    if (ref.startsWith("#")) return String.fromCodePoint(parseInt(ref.slice(1), 10));
    return ENTITIES[ref.toLowerCase()] ?? whole;
  });
}

/** The page's visible text runs: no head, script, style or comment. */
export function visibleText(html: string): string[] {
  const body = html
    .replace(/<head[\s\S]*?<\/head>/gi, " ")
    .replace(/<script[\s\S]*?<\/script[^>]*>/gi, " ")
    .replace(/<style[\s\S]*?<\/style>/gi, " ")
    .replace(/<!--[\s\S]*?-->/g, " ");
  return body
    .split(/<[^>]+>/)
    .map((run) => decode(run).replace(/\s+/g, " ").trim())
    .filter((run) => run !== "");
}

/**
 * The text of each paragraph whose class is a line on the page: a notice, a
 * lede, an empty state, a confirmation. `lineClass` says which classes those
 * are, and must not be global. A paragraph is read when `class` is its first attribute.
 */
export function pageLines(html: string, lineClass: RegExp): string[] {
  const out: string[] = [];
  for (const m of html.matchAll(/<p class="([^"]*)"[^>]*>([\s\S]*?)<\/p>/g)) {
    if (!lineClass.test(m[1] ?? "")) continue;
    out.push(decode((m[2] ?? "").replace(/<[^>]+>/g, " ")).replace(/\s+/g, " ").trim());
  }
  return out;
}

// ---------------------------------------------------------------------------
// 3. toasts, input boxes, dialogs and progress titles, read off the source
// ---------------------------------------------------------------------------

/** Every .ts file under `dir`. */
export function sourceFiles(dir: string): string[] {
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
export function callArguments(src: string, open: number): string[] {
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
export function literalText(arg: string): string | undefined {
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
export function literalFragments(arg: string): string[] {
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

/** The source of the value of `key:` at the top level of an object-literal argument, as written. */
export function propertyValue(arg: string, key: string): string | undefined {
  if (!arg.startsWith("{")) return undefined;
  for (const inner of callArguments(arg, 0)) {
    const m = new RegExp(`^${key}\\s*:\\s*([\\s\\S]*)$`).exec(inner);
    if (m) return m[1] ?? "";
  }
  return undefined;
}

/** The literal value of `key:` at the top level of an object-literal argument. */
export function propertyText(arg: string, key: string): string | undefined {
  const value = propertyValue(arg, key);
  return value === undefined ? undefined : literalText(value);
}

/**
 * Every string literal in an expression, at any depth, each read as literalText
 * reads one: the sentences an input box's validateInput returns from inside its
 * arrow function. A literal it only compares with is read too, harmlessly.
 */
export function literalsIn(expression: string): string[] {
  const out: string[] = [];
  for (let i = 0; i < expression.length; i++) {
    const ch = expression[i];
    if (ch !== '"' && ch !== "'" && ch !== "`") continue;
    const end = skipString(expression, i);
    const text = literalText(expression.slice(i, end + 1));
    if (text !== undefined && text.trim() !== "") out.push(text);
    i = end;
  }
  return out;
}

/** The names of an object literal's keys written as words (`Images:` or `"PDF documents":`); a computed key is not one. */
export function objectKeys(arg: string): string[] {
  if (!arg.startsWith("{")) return [];
  const out: string[] = [];
  for (const inner of callArguments(arg, 0)) {
    const m = /^(?:"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)'|([A-Za-z_$][\w$]*))\s*:/.exec(inner);
    const name = m?.[1] ?? m?.[2] ?? m?.[3];
    if (name !== undefined) out.push(name.replace(/\\(.)/g, "$1"));
  }
  return out;
}

/**
 * The copy the .ts files under `srcDir` hand straight to a toast, an input box,
 * an open or save dialog or a progress notification. A finding names the file as
 * it sits under `pkgRoot`, and the line.
 */
export function sourceCopy(srcDir: string, pkgRoot: string): Copy[] {
  return sourceFiles(srcDir).flatMap((file) => fileCopy(fs.readFileSync(file, "utf8"), path.relative(pkgRoot, file)));
}

/**
 * The copy one source file hands straight to a person (see sourceCopy): `rel`
 * is the file as a finding names it. An input box gives its prompt, title,
 * placeholder and the sentences its validateInput returns; a dialog gives its
 * title, its button's label and the names of its file filters.
 */
export function fileCopy(src: string, rel: string): Copy[] {
  const out: Copy[] = [];
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
    for (const key of ["prompt", "title", "placeHolder"]) {
      const text = options === undefined ? undefined : propertyText(options, key);
      if (text !== undefined) out.push({ where: at(m.index ?? 0), text, line: true });
    }
    const validate = options === undefined ? undefined : propertyValue(options, "validateInput");
    for (const text of validate === undefined ? [] : literalsIn(validate)) out.push({ where: at(m.index ?? 0), text, line: true });
  }
  for (const m of src.matchAll(/\bshow(?:Open|Save)Dialog\s*\(/g)) {
    const [options] = callArguments(src, (m.index ?? 0) + m[0].length - 1);
    if (options === undefined) continue;
    for (const key of ["title", "openLabel", "saveLabel"]) {
      const text = propertyText(options, key);
      if (text !== undefined) out.push({ where: at(m.index ?? 0), text, line: true });
    }
    for (const name of objectKeys(propertyValue(options, "filters") ?? "")) out.push({ where: at(m.index ?? 0), text: name });
  }
  for (const m of src.matchAll(/\bwithProgress\s*\(/g)) {
    const [options] = callArguments(src, (m.index ?? 0) + m[0].length - 1);
    const text = options === undefined ? undefined : propertyText(options, "title");
    if (text !== undefined) out.push({ where: at(m.index ?? 0), text, line: true });
  }
  return out;
}
