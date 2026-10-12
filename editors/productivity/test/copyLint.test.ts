// MemQL Productivity Tools' user-facing copy, held to the voice the design
// record sets (docs/internal/design/2026-09-28-vscode-extension-ux.md,
// "Audience and voice" and "Tree views and menus"). The twin of the MemQL
// extension's lint (editors/vscode/test/copyLint.test.ts): the rules and the
// scanning are theirs, shared through editors/vscode/test/copyScan.ts, and this
// file chooses what Productivity Tools is held to.
//
// FOUR SOURCES, because copy reaches a person four ways:
//   1. package.json: every string it contributes (command titles, setting
//      descriptions and the rest) and the extension's own name and description.
//   2. The email pages: every string in EMAIL_COPY (src/emailCopy.ts, where all
//      of the New email page's and the email editor's words live) and the
//      static text the two pages are built with.
//   3. The string literals a source file hands straight to a toast, an input
//      box's prompt, title, placeholder or validation sentence, an open or save
//      dialog's title, button and file filter names, or a progress
//      notification's title; the title, placeholder and written-out items of a
//      quick pick, typed or not; the messages of the
//      errors an input raises (a UserInputError is shown as the toast exactly
//      as it is written); the refusal sentences of the email model
//      (src/emailDocument.ts), which the Design page shows under a field and
//      which reach it through a function of its own; the sentences userMessage
//      returns (src/problems.ts), which the toast shows for every other error;
//      and the operation phrases that finish them ("open the PDF" in "Couldn’t
//      open the PDF.").
//   4. The dialogs: a modal one stops the person's work, so each must say why.
//
// WHAT FAILS: everything the MemQL extension's lint rules out (an issue
// reference, " -- " as punctuation, "portal", "console", internal vocabulary
// such as receipt, envelope or capability, a title carrying "MemQL: ", a line
// over 25 words); the names of the products behind the editor (Materializer,
// Nexus, and the Campaigns app) anywhere a person reads; a command outside the
// category MemQL; a command title that starts "<Word>: "; a status line over six
// words; and a modal dialog nobody excused.
//
// A STATUS LINE is what an action bar says about the email or the page. EMAIL_COPY
// names them (STATES). A {placeholder} counts as one word.
//
// An exemption goes in ALLOWED (or ALLOWED_MODALS) with its reason, and one that
// matches nothing fails the last case here, so the lists cannot outlive the
// copy they excused.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import { EMAIL_COPY } from "../src/emailCopy.js";
import { emailPage } from "../src/emailPage.js";
import { newEmailPage } from "../src/newEmailPage.js";
import {
  callArguments,
  decode,
  fileCopy,
  literalFragments,
  literalText,
  manifestCopy,
  problems,
  propertyText,
  RULES,
  sourceCopy,
  sourceFiles,
  visibleText,
  type Allowed,
  type Copy,
  type Rule,
} from "../../vscode/test/copyScan.js";

// dist-test/<name>.js -> the package root.
const PKG = path.resolve(__dirname, "..");
const SRC = path.join(PKG, "src");

/** The longest a status line may be, in words. */
const STATE_WORDS = 6;

/** The category every command is in (the design record: "Commands carry the category MemQL and titles without the prefix"). */
const CATEGORY = "MemQL";

/** The names of the products behind the editor, which a person using it has no reason to know. */
const NAMES: readonly Rule[] = [
  { name: "Materializer", re: /\bMaterializers?\b/i },
  { name: "Nexus", re: /\bNexus\b/i },
  // The Campaigns app. A campaign, in lower case, is just what an email belongs to.
  { name: "Campaigns", re: /\bCampaigns\b/ },
];

/** Genuine exceptions to the rules above. None yet: copy that trips one is reworded. */
const ALLOWED: readonly Allowed[] = [];

/** A modal dialog the extension is allowed to show, and why. */
interface AllowedModal {
  /** The source file, as it sits under the package root. */
  file: string;
  /** Text of the toast call that sets `modal: true`, which says which dialog this excuses. */
  contains: string;
  reason: string;
}

const ALLOWED_MODALS: readonly AllowedModal[] = [
  {
    file: "src/extension.ts",
    contains: "Use compared revision",
    reason:
      "Use Compared Revision is the one destructive confirmation: the next save will overwrite the newer cluster copy, so it waits for an answer instead of a toast.",
  },
];

/**
 * The EMAIL_COPY keys that are a status line: what an action bar says about the
 * email or the page. Each is held to STATE_WORDS words, wherever it sits.
 */
const STATES: ReadonlySet<string> = new Set([
  "notConnected",
  "notConnectedTo",
  "chooseOrganization",
  "chooseLayout",
  "nameIt",
  "ready",
  "writing",
  "failed",
  "layoutsFailed",
  "noOrganization",
  "unreadable",
  "local",
  "issuesOne",
  "issuesMany",
  "unsaved",
  "draft",
  "published",
  "changed",
  "publishing",
  "savingLayout",
  "savingCopy",
  "layoutSaved",
]);

// ---------------------------------------------------------------------------
// 1. package.json
// ---------------------------------------------------------------------------

interface Command {
  command: string;
  title: string;
  shortTitle?: string;
  category?: string;
}

function commands(): Command[] {
  const manifest = JSON.parse(fs.readFileSync(path.join(PKG, "package.json"), "utf8")) as { contributes: { commands: Command[] } };
  return manifest.contributes.commands;
}

/** What is wrong with the commands the manifest contributes: a category other than MemQL, a title that starts "<Word>: ". */
function commandProblems(): string[] {
  const out: string[] = [];
  for (const c of commands()) {
    if (c.category !== CATEGORY) out.push(`${c.command}: category ${c.category === undefined ? "missing" : `"${c.category}"`}, not "${CATEGORY}"`);
    for (const title of [c.title, c.shortTitle]) {
      if (title !== undefined && /^\w+:\s/.test(title)) out.push(`${c.command}: "${title}" starts with a prefix the category already says`);
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// 2. the email pages
// ---------------------------------------------------------------------------

/** Every EMAIL_COPY string, where it is; a status line carries its word limit. */
function emailCopy(): Copy[] {
  const out: Copy[] = [];
  const walk = (value: unknown, keys: string[]): void => {
    if (typeof value === "string") {
      const state = STATES.has(keys[keys.length - 1] ?? "");
      out.push({ where: `EMAIL_COPY.${keys.join(".")}`, text: value, line: true, ...(state ? { maxWords: STATE_WORDS } : {}) });
    } else if (value !== null && typeof value === "object") {
      for (const [key, inner] of Object.entries(value)) walk(inner, [...keys, key]);
    }
  };
  walk(EMAIL_COPY, []);
  return out;
}

/** The words a page carries in attributes, which a screen reader announces: labels, titles, placeholders and descriptions of pictures. */
const ATTRIBUTE_TEXT = /\s(?:aria-label|title|placeholder|alt)="([^"]*)"/g;

/** The static text of the two pages, as the editor draws it before a script fills it in: what is shown, and what is announced. */
function pageText(): Copy[] {
  const pages: Record<string, string> = {
    "emailPage()": emailPage({ title: "Welcome.email.json", script: "emailView.js", nonce: "abc123" }),
    "newEmailPage()": newEmailPage({ script: "newEmailView.js", nonce: "abc123" }),
  };
  return Object.entries(pages).flatMap(([where, html]) =>
    [...visibleText(html), ...[...html.matchAll(ATTRIBUTE_TEXT)].map((m) => decode(m[1] ?? ""))].filter((text) => text !== "").map((text) => ({ where, text })),
  );
}

function pageCopy(): Copy[] {
  return [...emailCopy(), ...pageText()];
}

// ---------------------------------------------------------------------------
// 3. what a notification, a prompt or a picker says: sourceCopy reads the calls; these read the rest
// ---------------------------------------------------------------------------

const lineAt = (src: string, index: number): number => src.slice(0, index).split("\n").length;

/** `read` over every .ts file under src, with the file as it sits under the package root. */
function eachSource(read: (src: string, file: string) => Copy[]): Copy[] {
  return sourceFiles(SRC).flatMap((file) => read(fs.readFileSync(file, "utf8"), path.relative(PKG, file)));
}

/**
 * The literals handed straight to `new UserInputError(...)`. The problem toast
 * (problemUI.ts) shows that message as it is written, where any other error
 * gets a safe message of its own.
 */
function userErrorsIn(src: string, file: string): Copy[] {
  const out: Copy[] = [];
  for (const m of src.matchAll(/\bnew UserInputError\s*\(/g)) {
    const [arg] = callArguments(src, (m.index ?? 0) + m[0].length - 1);
    const where = `${file}:${lineAt(src, m.index ?? 0)}`;
    const text = arg === undefined ? undefined : literalText(arg);
    if (text !== undefined) out.push({ where, text, line: true });
    else for (const fragment of arg === undefined ? [] : literalFragments(arg)) out.push({ where, text: fragment });
  }
  return out;
}

/**
 * The sentences problems.ts returns: userMessage's safe messages, which the
 * problem toast shows for an error that is not one of the person's own inputs.
 */
function userMessagesIn(src: string, file: string): Copy[] {
  if (!file.endsWith("problems.ts")) return [];
  const out: Copy[] = [];
  for (const m of src.matchAll(/\breturn\s+("(?:[^"\\\n]|\\.)*"|`(?:[^`\\]|\\.)*`)\s*;/g)) {
    const text = literalText(m[1] ?? "");
    if (text !== undefined) out.push({ where: `${file}:${lineAt(src, m.index ?? 0)}`, text, line: true });
  }
  return out;
}

/**
 * The words of the quick picks: each one's title and placeholder, and the items
 * written out in the call. A list built from data has none of our words in it.
 */
function quickPicksIn(src: string, file: string): Copy[] {
  const out: Copy[] = [];
  // A typed call (showQuickPick<Item>(...)) is the same call.
  for (const m of src.matchAll(/\bshowQuickPick\s*(?:<[^<>()]*>)?\s*\(/g)) {
    const [items, options] = callArguments(src, (m.index ?? 0) + m[0].length - 1);
    const where = `${file}:${lineAt(src, m.index ?? 0)}`;
    if (items?.startsWith("[")) {
      for (const item of callArguments(items, 0)) {
        const label = literalText(item);
        if (label !== undefined) out.push({ where, text: label });
        else
          for (const key of ["label", "description", "detail"]) {
            const text = propertyText(item, key);
            if (text !== undefined) out.push({ where, text });
          }
      }
    }
    for (const key of ["title", "placeHolder"]) {
      const text = options === undefined ? undefined : propertyText(options, key);
      if (text !== undefined) out.push({ where, text, line: true });
    }
  }
  return out;
}

/**
 * The refusals the Design page shows under a field. The email model raises them through `refusal(message)`, not
 * `new UserInputError(...)`, so userErrorsIn does not see them: the values of its SENTENCES table, and the literals
 * handed to refusal() itself.
 */
function refusalsIn(src: string, file: string): Copy[] {
  if (!file.endsWith("emailDocument.ts")) return [];
  const out: Copy[] = [];
  const table = src.indexOf("const SENTENCES = {");
  if (table >= 0) {
    for (const entry of callArguments(src, src.indexOf("{", table))) {
      const m = /^\w+\s*:\s*([\s\S]*)$/.exec(entry);
      const text = m === null ? undefined : literalText(m[1] ?? "");
      if (text !== undefined) out.push({ where: `${file}:${lineAt(src, src.indexOf(entry, table))}`, text, line: true });
    }
  }
  for (const m of src.matchAll(/\brefusal\s*\(/g)) {
    const [arg] = callArguments(src, (m.index ?? 0) + m[0].length - 1);
    const text = arg === undefined ? undefined : literalText(arg);
    if (text !== undefined) out.push({ where: `${file}:${lineAt(src, m.index ?? 0)}`, text, line: true });
  }
  return out;
}

/**
 * The operation phrases that finish "Couldn’t <operation>." in userMessage: the second argument of showProblem and
 * reportProblem, and the first of userOperation. Each is read as the sentence it ends. A phrase that is a conditional
 * gives its arms (and any literal it compares with, which is read as a phrase too and harmlessly).
 */
function operationsIn(src: string, file: string): Copy[] {
  const out: Copy[] = [];
  for (const m of src.matchAll(/(?<!function\s+)\b(showProblem|reportProblem|userOperation)\s*(?:<[^<>()]*>)?\s*\(/g)) {
    const arg = callArguments(src, (m.index ?? 0) + m[0].length - 1)[m[1] === "userOperation" ? 0 : 1];
    if (arg === undefined) continue;
    const where = `${file}:${lineAt(src, m.index ?? 0)}`;
    const text = literalText(arg);
    for (const phrase of text === undefined ? literalFragments(arg) : [text]) out.push({ where, text: `Couldn’t ${phrase}.`, line: true });
  }
  return out;
}

/** Everything the source shows a person in a notification, a prompt or a picker, and the phrases and sentences that finish them. */
function shownCopy(): Copy[] {
  return [
    ...sourceCopy(SRC, PKG),
    ...eachSource(userErrorsIn),
    ...eachSource(refusalsIn),
    ...eachSource(userMessagesIn),
    ...eachSource(operationsIn),
    ...eachSource(quickPicksIn),
  ];
}

// ---------------------------------------------------------------------------
// 4. dialogs, read off the source
// ---------------------------------------------------------------------------

const MODAL = /\bmodal\s*:\s*true\b/g;

interface Modal {
  file: string;
  /** The line of the toast that sets it; 0 when it sits outside a toast call. */
  line: number;
  /** The toast call's arguments: its message, options and buttons. Empty outside a toast call. */
  call: string;
}

/**
 * Every `modal: true` in `src`. Each belongs to the toast call whose arguments
 * hold it, and one that sits anywhere else (an options object kept in a
 * variable) is reported without a call, so nothing excuses it.
 */
function modalsIn(src: string, file: string): Modal[] {
  const uses: Modal[] = [];
  for (const m of src.matchAll(/(?<!function\s+)\bshow(?:Information|Warning|Error)Message\s*\(/g)) {
    const call = callArguments(src, (m.index ?? 0) + m[0].length - 1).join(", ");
    const line = src.slice(0, m.index ?? 0).split("\n").length;
    for (let n = call.match(MODAL)?.length ?? 0; n > 0; n--) uses.push({ file, line, call });
  }
  for (let n = (src.match(MODAL)?.length ?? 0) - uses.length; n > 0; n--) uses.push({ file, line: 0, call: "" });
  return uses;
}

function modals(): Modal[] {
  return sourceFiles(SRC).flatMap((file) => modalsIn(fs.readFileSync(file, "utf8"), path.relative(PKG, file)));
}

const usedModals = new Set<AllowedModal>();

function excusedBy(use: Modal): AllowedModal | undefined {
  const entry = ALLOWED_MODALS.find((a) => a.file === use.file && use.call.includes(a.contains));
  if (entry !== undefined) usedModals.add(entry);
  return entry;
}

// ---------------------------------------------------------------------------
// the check
// ---------------------------------------------------------------------------

const used = new Set<Allowed>();

/** `copy` held to `rules`, and to `more`: findings of the caller's own, reported with the rest. */
function assertClean(copy: readonly Copy[], source: string, rules: readonly Rule[] = RULES, more: readonly string[] = []): void {
  assert.ok(copy.length > 0, `no copy was read from ${source}, so this case checks nothing`);
  const found = [...problems(copy, ALLOWED, used, rules), ...more];
  assert.deepEqual(found, [], `${source} carries copy the design record rules out:\n  ${found.join("\n  ")}`);
}

/** The copy of all three sources a person reads, stripped to words: the names are held against the words alone. */
function allWords(): Copy[] {
  return [...manifestCopy(PKG), ...pageCopy(), ...shownCopy()].map(({ where, text }) => ({ where, text }));
}

test("package.json copy is plain and every command is in the MemQL category", () => {
  assert.ok(commands().length > 0, "the manifest contributes commands, so this case checks something");
  assertClean(manifestCopy(PKG), "package.json", RULES, commandProblems());
});

test("page copy is plain and status lines are at most six words", () => {
  assertClean(pageCopy(), "the email pages");
});

test("toasts, input boxes and progress titles are plain", () => {
  assertClean(shownCopy(), "src");
});

test("no user copy names Materializer, Nexus or Campaigns", () => {
  assertClean(allWords(), "package.json, the email pages and src", NAMES);
});

test("only allowed modals remain", () => {
  const stray = modals().filter((use) => excusedBy(use) === undefined);
  const where = (use: Modal): string => (use.line > 0 ? `${use.file}:${use.line}` : use.file);
  assert.deepEqual(
    stray.map((use) => `${where(use)}: ${use.call === "" ? "modal: true outside a toast call" : use.call.slice(0, 100)}`),
    [],
    "a modal dialog stops the person's work: use a toast with a button, or list this one in ALLOWED_MODALS with its reason",
  );
});

test("the scans read what they exist for", () => {
  // The positive controls: a scanner that read nothing would pass forever.
  const texts = new Set(manifestCopy(PKG).map((c) => c.text));
  for (const expected of [
    "MemQL Productivity Tools",
    "Documents, emails, PDFs and human-reviewed suggestions connected to MemQL.",
    "Markdown Review",
    "Insert Image into Markdown",
    "Match MemQL OS each time a file opens.",
  ]) {
    assert.ok(texts.has(expected), `the manifest scan did not read "${expected}"`);
  }
  assert.ok(
    manifestCopy(PKG).some((c) => c.title === true && c.text === "Markdown Review"),
    "a command title is read as a title",
  );
  for (const notCopy of ["memql.productivity.markdown.review", "navigation@3", "ask", "string"]) {
    assert.ok(!texts.has(notCopy), `"${notCopy}" is not copy`);
  }

  const copy = pageCopy();
  const at = (where: string, text?: string): boolean => copy.some((c) => c.where === where && (text === undefined || c.text === text));
  assert.ok(at("emailPage()", "Design") && at("emailPage()", "Source"), "the Design page's static text is read");
  assert.ok(at("newEmailPage()", "New email") && at("newEmailPage()", "Layout"), "the New email page's static text is read");
  assert.ok(at("newEmailPage()", "Autumn welcome") && at("emailPage()", "Email preview"), "the words a page announces in attributes are read");
  assert.ok(at("EMAIL_COPY.newEmail.title") && at("EMAIL_COPY.editor.design") && at("EMAIL_COPY.host.tooMany"), "every section of EMAIL_COPY is read");
  assert.ok(at("EMAIL_COPY.editor.themeColours.primary", "Primary"), "EMAIL_COPY is read to its depth");
  const states = emailCopy().filter((c) => c.maxWords === STATE_WORDS);
  for (const key of STATES) {
    assert.ok(states.some((c) => c.where.endsWith(`.${key}`)), `no EMAIL_COPY string is named ${key}, so the status line limit would not reach it: update STATES`);
  }
  assert.ok(!states.some((c) => c.where === "EMAIL_COPY.host.tooMany"), "a string that is not a status line is held to the line length");

  // The controls name strings, never counts: a count falls when the code it counted is deleted, and says nothing about what was read.
  const sources = sourceCopy(SRC, PKG);
  assert.ok(
    sources.some((c) => c.text === "Your edits are unchanged. Save when your review is complete." && c.line === true),
    "the scan reads a toast",
  );
  assert.ok(sources.some((c) => c.text === "New Markdown document" && c.line === true), "the scan reads an input box's title");
  assert.ok(sources.some((c) => c.text === "Creating X" && c.line === true), "the scan reads a progress title");
  const errors = eachSource(userErrorsIn);
  assert.ok(errors.some((c) => c.text === "Invalid MemQL file link." && c.line === true), "the scan reads a UserInputError");
  assert.ok(
    errors.some((c) => c.text === "Save the document, then attach a supported image or Markdown file." && c.where.startsWith("src/markdownEditor.ts:")),
    "the scan reads the files under src, not only the extension's",
  );
  const messages = eachSource(userMessagesIn);
  assert.ok(messages.some((c) => c.text === "The AI stopped responding. Try again." && c.line === true), "the scan reads a sentence userMessage returns");
  assert.ok(
    messages.some((c) => c.text === "Couldn’t X. Try again. If this continues, choose Show details." && c.line === true),
    "the scan reads a template sentence, its placeholder as X",
  );
  const refusals = eachSource(refusalsIn);
  assert.ok(refusals.some((c) => c.text === "Enter a subject on one line." && c.line === true), "the scan reads the SENTENCES the Design page shows");
  assert.ok(refusals.some((c) => c.text === "Enter a height between 0 and X pixels."), "the scan reads a template sentence of SENTENCES, its placeholder as X");
  assert.ok(refusals.some((c) => c.text === "Remove X in Source. Emails don’t have that field."), "the scan reads a literal handed to refusal()");
  assert.deepEqual(
    refusalsIn(
      ['const SENTENCES = {', '  a: "One.",', "  b: `Two ${n}.`,", "  c: other,", '} as const;', "throw refusal(SENTENCES.a);", "throw refusal(`Three ${x}.`);", 'const refusal = (message: string) => new UserInputError(message);'].join("\n"),
      "src/emailDocument.ts",
    ).map((c) => c.text),
    ["One.", "Two X.", "Three X."],
    "SENTENCES' values and refusal()'s literals are read; a value that is not a literal and a reference to SENTENCES are not",
  );
  assert.deepEqual(refusalsIn('const SENTENCES = { a: "One." };', "src/other.ts"), [], "only emailDocument.ts is read for refusals");
  const operations = eachSource(operationsIn);
  assert.ok(operations.some((c) => c.text === "Couldn’t open the Markdown view." && c.line === true), "the scan reads the phrase a command gives showProblem");
  assert.ok(operations.some((c) => c.text === "Couldn’t open the file."), "the scan reads the phrase given to userOperation");
  assert.ok(operations.some((c) => c.text === "Couldn’t load your notes."), "the scan reads the phrase given to reportProblem");
  assert.deepEqual(
    operationsIn(
      [
        'void showProblem(error, "open the PDF");',
        "reportProblem(e, 'load your notes', lease);",
        'reportProblem(err, ok ? "apply the approved changes" : "prepare changes");',
        'return userOperation("save the file", () => files.save(doc));',
        'await userOperation<void>("reload it", async () => {});',
        "await showProblem(error, `copy ${what}`);",
        "export async function showProblem(error: unknown, operation: string): Promise<void> {}",
        'showProblem({ message: "Not an operation." });',
        "reportProblem(error, operation);",
      ].join("\n"),
      "src/x.ts",
    ).map((c) => c.text),
    ["Couldn’t open the PDF.", "Couldn’t load your notes.", "Couldn’t apply the approved changes.", "Couldn’t prepare changes.", "Couldn’t save the file.", "Couldn’t reload it.", "Couldn’t copy X."],
    "the phrase is showProblem's and reportProblem's second argument and userOperation's first; a definition, a call without one and a variable are not read",
  );
  assert.deepEqual(userMessagesIn('return "Not a message.";', "src/other.ts"), [], "only problems.ts is read for returned sentences");
  assert.deepEqual(userMessagesIn("if (x) return `Couldn’t ${what}.`; return error.message;", "src/problems.ts").map((c) => c.text), ["Couldn’t X."]);
  assert.deepEqual(
    quickPicksIn(
      [
        'await window.showQuickPick(["Open it", "Open it elsewhere"], { title: `Open ${name}`, placeHolder: "Choose where", canPickMany: false });',
        "await window.showQuickPick(rows.map(row => ({ label: row.name })), { title: 'Pick one', placeHolder: 'Search' });",
        'await window.showQuickPick([{ label: "A", description: "The first" }, { label: "B", detail: cond ? "x" : "y" }], { placeHolder: "Pick" });',
      ].join("\n"),
      "src/x.ts",
    ).map((c) => [c.text, c.line === true]),
    [
      ["Open it", false],
      ["Open it elsewhere", false],
      ["Open X", true],
      ["Choose where", true],
      ["Pick one", true],
      ["Search", true],
      ["A", false],
      ["The first", false],
      ["B", false],
      ["Pick", true],
    ],
    "a quick pick's title, placeholder and written-out items are read; a list built from data is not",
  );
  assert.deepEqual(
    quickPicksIn("const chosen = await window.showQuickPick<Picture>(pictures, { title: 'Choose a picture', placeHolder: 'Recent pictures' });", "src/x.ts").map((c) => c.text),
    ["Choose a picture", "Recent pictures"],
    "a typed quick pick is read like any other",
  );
  // An input box's placeholder and the sentences its validation returns, and an open or save dialog's words, are read.
  assert.deepEqual(
    fileCopy(
      [
        "await window.showInputBox({ title: 'Name it', prompt: `For ${who}`, placeHolder: 'Autumn welcome', validateInput: v => v.trim() ? undefined : 'Enter a name.' });",
        "await window.showOpenDialog({ title: 'Choose a picture', openLabel: 'Use picture', canSelectMany: false, filters: { Pictures: ['png'], 'PDF documents': ['pdf'], [COPY.name]: ['gif'] } });",
        "await window.showSaveDialog({ saveLabel: 'Download ZIP', defaultUri: target });",
        "await window.showOpenDialog(options);",
      ].join("\n"),
      "src/x.ts",
    ).map((c) => [c.text, c.line === true]),
    [
      ["For X", true],
      ["Name it", true],
      ["Autumn welcome", true],
      ["Enter a name.", true],
      ["Choose a picture", true],
      ["Use picture", true],
      ["Pictures", false],
      ["PDF documents", false],
      ["Download ZIP", true],
    ],
    "the placeholder, the validation sentences, the dialog's title and button and its filters' names are read; a computed name and options kept in a variable are not",
  );
  assert.ok(sources.some((c) => c.text === "Insert image" && c.where.startsWith("src/insertImage.ts:")), "the scan reads an open dialog of the tree");
  assert.ok(sources.some((c) => c.text === "Download ZIP" && c.where.startsWith("src/extension.ts:")), "the scan reads a save dialog of the tree");
  const sample = [
    'void window.showWarningMessage("Couldn\'t reach " + name + ".", "Retry");',
    "window.showInputBox({ title: `Sign in to ${cluster}`, prompt: 'Paste the code', value: x });",
    "window.withProgress({ location: 1, title: 'Signing in' }, async () => {});",
  ].join("\n");
  assert.deepEqual(callArguments(sample, sample.indexOf("(")).map(literalText), [undefined, "Retry"]);
  assert.deepEqual(literalFragments(`err?.message ?? "Couldn't connect."`), ["Couldn't connect."]);
  assert.equal(propertyText(callArguments(sample, sample.indexOf("showInputBox(") + "showInputBox".length)[0] ?? "", "title"), "Sign in to X");

  // The modal scan finds a modal, and finds one hidden in a variable.
  const found = modalsIn('void window.showWarningMessage(`Overwrite ${name}?`, { modal: true }, "Overwrite");\nconst options = { modal: true };', "src/x.ts");
  assert.deepEqual(
    found.map((use) => [use.file, use.line, use.call.includes('"Overwrite"')]),
    [
      ["src/x.ts", 1, true],
      ["src/x.ts", 0, false],
    ],
  );
  assert.equal(modals().filter((use) => use.call.includes("Use compared revision")).length, 1, "the scan finds the extension's one modal");

  // The rules can fail: each of them catches what it is there for.
  const unused = new Set<Allowed>();
  const flagged = (text: string, extra: Partial<Copy> = {}, rules: readonly Rule[] = RULES): number =>
    problems([{ where: "sample", text, ...extra }], [], unused, rules).length;
  assert.equal(flagged("Use in the Campaigns app.", {}, NAMES), 1);
  assert.equal(flagged("Use in the Materializer.", {}, NAMES), 1);
  assert.equal(flagged("Ask Nexus.", {}, NAMES), 1);
  assert.equal(flagged("Written for your campaigns.", {}, NAMES), 0, "a campaign in lower case is only a word");
  assert.equal(flagged("One two three four five six seven", { maxWords: STATE_WORDS }), 1, "a status line has at most six words");
  assert.equal(flagged("One two three four five six", { maxWords: STATE_WORDS }), 0);
  assert.equal(flagged("Published to {organization}", { maxWords: 3 }), 0, "a placeholder is one word");
  assert.equal(flagged("Published to {organization}", { maxWords: 2 }), 1);
  assert.equal(flagged("See memql#12.", { line: true }), 1);

  // An exemption excuses what it names, is recorded as used, and is not used by what it does not name.
  const entry: Allowed = { where: "sample", rule: "Campaigns", contains: "the Campaigns app", reason: "A control." };
  const seen = new Set<Allowed>();
  assert.equal(problems([{ where: "sample", text: "Open the Campaigns app." }], [entry], seen, NAMES).length, 0);
  assert.ok(seen.has(entry));
  const missed = new Set<Allowed>();
  assert.equal(problems([{ where: "sample", text: "Open Campaigns." }], [entry], missed, NAMES).length, 1);
  assert.ok(!missed.has(entry));
});

test("every exemption still matches", () => {
  problems([...manifestCopy(PKG), ...pageCopy(), ...shownCopy()], ALLOWED, used);
  problems(allWords(), ALLOWED, used, NAMES);
  const stale = ALLOWED.filter((a) => !used.has(a));
  assert.deepEqual(stale, [], "an exemption matches nothing; delete it");
  for (const use of modals()) excusedBy(use);
  const staleModals = ALLOWED_MODALS.filter((a) => !usedModals.has(a));
  assert.deepEqual(staleModals, [], "a modal exemption matches nothing; delete it");
});
