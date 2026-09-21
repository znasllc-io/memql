// Settings -> Language, the view model (memql#5390).
//
// The one row `languageStatus` answers, read defensively, and the sentences the
// section prints about it. Pure: no React, no connection, so every string the
// section shows is testable on its own.
//
// THE ROW IS READ STRICTLY. A row this window does not recognise answers null
// rather than a best guess, and the section says it could not read the answer.
// A page that quietly dropped a form it did not understand would report fewer
// deprecations than the cluster has, and a status it could not read would be a
// guess about the one promise this page exists to state.

/** Where one use of a deprecated form starts in the tree the node loaded. */
export interface LanguageUse {
  file: string;
  line: number;
  column: number;
  /** The spelling as written -- `array(string)` where the form is `array(T)`. */
  text: string;
}

export interface LanguageForm {
  rule: string;
  spelling: string;
  replacement: string;
  migrator: string;
  deprecatedIn: string;
  /** The first release ALLOWED to refuse the form. A floor, not a date; "" when the engine could not read one. */
  refusedFrom: string;
  state: "deprecated" | "refused";
  uses: LanguageUse[];
}

export interface LanguageFacts {
  language: string;
  edition: string;
  status: "frozen" | "draft";
  grammarVersion: string;
  editorRelease: string;
  deprecationWindowMinors: number;
  forms: LanguageForm[];
}

type Fields = Record<string, unknown>;

function objectOf(value: unknown): Fields | null {
  return value !== null && typeof value === "object" && !Array.isArray(value) ? (value as Fields) : null;
}

function text(from: Fields, key: string): string | null {
  const v = from[key];
  return typeof v === "string" ? v : null;
}

function whole(from: Fields, key: string): number | null {
  const v = from[key];
  return typeof v === "number" && Number.isInteger(v) ? v : null;
}

function useFrom(value: unknown): LanguageUse | null {
  const o = objectOf(value);
  if (o === null) return null;
  const file = text(o, "file");
  const line = whole(o, "line");
  const column = whole(o, "column");
  const written = text(o, "text");
  return file === null || line === null || column === null || written === null
    ? null
    : { file, line, column, text: written };
}

function formFrom(value: unknown): LanguageForm | null {
  const o = objectOf(value);
  if (o === null || !Array.isArray(o.uses)) return null;
  const rule = text(o, "rule");
  const spelling = text(o, "spelling");
  const replacement = text(o, "replacement");
  const migrator = text(o, "migrator");
  const deprecatedIn = text(o, "deprecatedIn");
  const refusedFrom = text(o, "refusedFrom");
  const state = text(o, "state");
  if (
    rule === null ||
    spelling === null ||
    replacement === null ||
    migrator === null ||
    deprecatedIn === null ||
    refusedFrom === null ||
    (state !== "deprecated" && state !== "refused")
  ) {
    return null;
  }
  const uses: LanguageUse[] = [];
  for (const raw of o.uses) {
    const use = useFrom(raw);
    if (use === null) return null;
    uses.push(use);
  }
  return { rule, spelling, replacement, migrator, deprecatedIn, refusedFrom, state, uses };
}

/**
 * The `languageStatus` row as facts, or null when it is not one this window
 * reads. Takes the SDK's flattened row or a row still carrying its `payload`
 * envelope; never throws.
 */
export function languageFactsFromRow(payload: unknown): LanguageFacts | null {
  const outer = objectOf(payload);
  if (outer === null) return null;
  const row = objectOf(outer.payload) ?? outer;

  const language = text(row, "language");
  const edition = text(row, "edition");
  const status = text(row, "status");
  const grammarVersion = text(row, "grammarVersion");
  const editorRelease = text(row, "editorRelease");
  const deprecationWindowMinors = whole(row, "deprecationWindowMinors");
  if (
    language === null ||
    edition === null ||
    (status !== "frozen" && status !== "draft") ||
    grammarVersion === null ||
    editorRelease === null ||
    deprecationWindowMinors === null ||
    !Array.isArray(row.forms)
  ) {
    return null;
  }
  const forms: LanguageForm[] = [];
  for (const raw of row.forms) {
    const form = formFrom(raw);
    if (form === null) return null;
    forms.push(form);
  }
  return { language, edition, status, grammarVersion, editorRelease, deprecationWindowMinors, forms };
}

/** Every use across the forms, counted with the files they are in. */
export function usesSummary(forms: readonly LanguageForm[]): string {
  let uses = 0;
  const files = new Set<string>();
  for (const form of forms) {
    uses += form.uses.length;
    for (const use of form.uses) files.add(use.file);
  }
  if (uses === 0) return "No loaded file uses a deprecated form.";
  return `${uses} ${uses === 1 ? "use" : "uses"} in ${files.size} ${files.size === 1 ? "file" : "files"}`;
}

/**
 * Where one form stands in its window, as the answering node's loader treats
 * it.
 *
 * `refusedFrom` IS A FLOOR, NOT A DATE. It is the first release ALLOWED to
 * refuse the form, and a form may warn for longer than its window, so a
 * deprecated form "may be refused from" it -- saying it "is refused" then would
 * promise a removal nobody has decided. A refused form is a fact, and says
 * since when.
 *
 * An EMPTY floor is the engine saying it could not read one (a form whose
 * deprecation point does not parse). The sentence then states only what is
 * true: it still loads. An invented floor would be the one number on this page
 * somebody plans a migration around.
 */
export function windowSentence(form: LanguageForm): string {
  if (form.state === "refused") {
    return form.refusedFrom === "" ? "Refused: this cluster no longer loads it." : `Refused since ${form.refusedFrom}.`;
  }
  return form.refusedFrom === ""
    ? "Loads with a warning."
    : `Loads with a warning. It may be refused from ${form.refusedFrom}.`;
}

/**
 * What the edition promises, which depends on whether it has frozen.
 *
 * A DRAFT PROMISES NOTHING YET. Its authored surface may still change, and a
 * form retired inside a draft is refused the moment it is retired, so the
 * frozen edition's guarantee printed under "2026, draft" would be a promise
 * this cluster does not make. The draft sentence says what is true now and what
 * freezing will add.
 */
export function editionCaption(facts: Pick<LanguageFacts, "edition" | "status" | "deprecationWindowMinors">): string {
  if (facts.status === "draft") {
    return (
      `Edition ${facts.edition} is still a draft: the forms it accepts can change before it freezes. ` +
      "Once frozen, it keeps every form it accepts, with the same meaning, until a new edition."
    );
  }
  const minors = facts.deprecationWindowMinors;
  return (
    "A frozen edition keeps every form it accepts, with the same meaning, until a new edition. " +
    `Deprecated forms keep loading for at least ${minors} minor ${minors === 1 ? "release" : "releases"}.`
  );
}

/** The two documents the section copies for a model. */
export type LanguageDocsTopic = "grammar" | "vocabulary";

/** The SDK's own envelope keys on a flattened builtin row: not the document. */
const ENVELOPE_KEYS = new Set(["id", "concept", "createdAt", "createdBy", "type", "schema"]);

/**
 * The text a copy puts on the clipboard, from the row the topic's builtin
 * answered.
 *
 * TWO BUILTINS, NOT ONE READ WITH A MODE, because that is how this cluster
 * serves them: `memqlGrammar()` answers the engine's EBNF and `memqlVocabulary()`
 * its terms. The grammar is copied VERBATIM. The vocabulary is copied as the
 * engine's own structure as JSON -- its entries and whatever the row says about
 * them -- rather than a rendering this window invents: a model reads JSON as
 * well as prose, and a second format written here is one that can drift from
 * the terms it describes.
 *
 * THROWS when the row is not the document asked for, so a copy never puts the
 * wrong document on somebody's clipboard under the right label.
 */
export function languageDocsText(topic: LanguageDocsTopic, payload: unknown): string {
  const outer = objectOf(payload);
  const row = outer === null ? null : (objectOf(outer.payload) ?? outer);
  if (row !== null && topic === "grammar" && text(row, "format") === "ebnf") {
    const content = text(row, "content");
    if (content !== null) return content;
  }
  if (row !== null && topic === "vocabulary" && Array.isArray(row.entries)) {
    const document: Fields = {};
    for (const [key, value] of Object.entries(row)) {
      if (!ENVELOPE_KEYS.has(key)) document[key] = value;
    }
    return JSON.stringify(document, null, 2);
  }
  throw new Error(
    topic === "grammar"
      ? "The answer carries no grammar document."
      : "The answer carries no vocabulary document.",
  );
}
