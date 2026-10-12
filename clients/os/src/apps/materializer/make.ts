import {
  fileNameProblem,
  fileNameStem,
  isDescribedMarkdown,
  sameEntityId,
  type LibraryFileLookup,
  type MaterializeRequest,
  type MaterializeSource,
} from "@znasllc-io/memql-sdk-core/client";

import { COMPOSE_FORMATS, type ComposeFormat, type ComposeSetup, type ComposeSource } from "../../items/materialize";
import type { CompositionRow, SourceRow, TemplateRow } from "./rows";
import { deployableWord, formatWord } from "./words";

// make.ts -- the Make a file wizard as one pure model, with no DOM
// (design: docs/superpowers/specs/2026-10-12-materializer-app-design.md, section 2).
//
// A MakeDraft holds what the person (or the app that sent them) has answered; MakeFacts holds
// what this window has read about the cluster. From those two alone this module decides the
// steps, which one opens, what each says on the rail, what the floor says and offers, and the
// request a draft sends. The wizard draws it and the press sends it; neither decides anything.
//
// ===========================================================================
// THE FLOOR AND THE CLUSTER ARE TOLD THE SAME FACTS
// ===========================================================================
// What the floor says a file will be (blank, described, composed) is predicted from the very
// request the press sends, by the SDK's own predicate over the picked files' rows
// (isDescribedMarkdown). THIS FILE HOLDS NO LIST OF WHICH FILES THE LIBRARY CAN ATTACH:
// that rule is the SDK's, held to the Library's by a drift guard, and a second copy here
// would be one nothing holds. A file the window has not read counts as one the Library
// cannot attach, so the floor never promises "You review its draft in the editor." where the
// press, which reads the file, would send the request to the composer.
//
// EVERY PICKED LIBRARY FILE IS SENT WITH content: true, whichever path the request takes. The
// cluster decides the path from the files' rows and reads contents only when the composer
// writes the file (spec D13), so the flag costs a described request nothing and is what lets
// the composer read a file when it does.
//
// AN UNANSWERED OPTIONAL IS LEFT OUT of the request, never sent empty: "the caller said
// nothing" and "the caller said empty" are different statements to the cluster.
//
// ===========================================================================
// WHAT THIS FILE COPIES FROM THE ENGINE, AND WHAT HOLDS THE COPIES
// ===========================================================================
// A person is told what will happen before the engine is asked, so some of the engine's rules
// are copied here. A copy nothing holds drifts, and says something false to a person when it
// does (it did: an email template is filed as .email.json, and this once said .json).
// integrations/compose/os_copies_test.go reads this file and holds the copies to the engine:
//
//   READABLE_EXTENSIONS   the files the composer reads (source_content.go), in both directions
//   BLANK_FORMATS         the formats RenderBlank files empty
//   BLANK_DETAIL          the sentence ErrBlankFormat is
//   PACKAGE_KINDS         the packages the engine writes (DeployableKinds)
//   filedName             the name a file is filed under (filedFileName), through the table
//                         integrations/compose/testdata/filed_names.json, which make.test.ts
//                         runs through filedName and a Go test runs through the engine
//
// Those tests read the declarations above by their names and shapes. A declaration that moves
// or changes shape fails them with a sentence naming this file; move the test with it.

// ---------------------------------------------------------------------------
// The draft
// ---------------------------------------------------------------------------

const PACKAGE_KINDS = ["spa", "static", "shopify_storefront"] as const;
export type PackageKind = (typeof PACKAGE_KINDS)[number];

export type FileKind =
  | { kind: "document"; format: ComposeFormat }
  | { kind: "package"; deployableKind: PackageKind }
  | { kind: "emailTemplate" };

/** One thing to make the file from. A Library file's `ref` is the file row's own id, as the cluster gave it. */
export interface Ingredient {
  kind: ComposeSource["kind"];
  ref: string;
  label: string;
}

export interface MakeDraft {
  /** Change it with withKind and never by assigning: a kind change has consequences for the template and the organization. */
  fileKind: FileKind;
  /** "" for no template. */
  templateId: string;
  ingredients: Ingredient[];
  /** What the person described, as typed. */
  statement: string;
  /** The name as typed; the cluster adds the extension. */
  name: string;
  /** "" for the top level of the Library. A sender's folder is kept as claimed, even one that proves unavailable. */
  folderId: string;
  /** "" for no organization. */
  accountId: string;
  /** Optional steps the person (or a sender) has passed; passed with nothing in it reads as skipped. */
  passed: { from: boolean; describe: boolean };
}

export type MakeStepId = "kind" | "template" | "from" | "describe" | "name" | "folder" | "organization" | "file";

/**
 * What this window has read. `files` is the facts of the person's Library files it has read
 * (the SDK's LibraryFileFacts of a file row), by the id a picked file names: null for a file
 * whose row cannot be read, undefined for one the window has not read.
 */
export interface MakeFacts {
  templates: readonly TemplateRow[];
  /**
   * Whether `templates` is the person's whole list, settled: false while the feed has not
   * answered, and between two reads of it. Until then nothing can be said of a template from
   * its absence, so a template the draft holds is kept (keepUsableTemplate).
   */
  templatesLoaded: boolean;
  /** Whether readiness reports Inference set up: null while that is not known, which does not block. */
  inference: boolean | null;
  files: LibraryFileLookup;
}

/** The step's state on the rail: a step waits for the person, or has been answered, or was passed with nothing. */
export type MakeStepState = "done" | "skipped" | "waiting";

/** What the cluster will do with the request: file an empty page, file one a description will fill in the editor, or compose it. */
export type MakePath = "blank" | "described" | "composed";

/** The floor's state word, and the sentence under it. */
export interface FloorWords {
  word: string;
  detail?: string;
}

export interface Blocker {
  reason: "name" | "blank" | "organization" | "inference";
  words: FloorWords;
}

/** The names the rail shows. folder(id): the folder's name; null once the folder list has loaded without it; undefined while it loads. */
export interface AnswerNames {
  folder(id: string): string | null | undefined;
  template(id: string): string;
  account(id: string): string;
}

export type MakeAct = "cancel" | "next" | "make" | "setUpInference" | "stop" | "leave";

// ---------------------------------------------------------------------------
// The steps and the kinds of file
// ---------------------------------------------------------------------------

/** Each step's name and what it asks (spec 2.1). */
export const MAKE_STEPS: Record<MakeStepId, { name: string; sentence: string }> = {
  kind: { name: "Kind of file", sentence: "What comes out." },
  template: { name: "Template", sentence: "How it looks." },
  from: { name: "From", sentence: "What to make it from." },
  describe: { name: "Describe", sentence: "What it should say." },
  name: { name: "Name", sentence: "What to call it." },
  folder: { name: "Folder", sentence: "Where it is filed." },
  organization: { name: "Organization", sentence: "Who it is for." },
  file: { name: "File", sentence: "What you get." },
};

export interface FileKindEntry {
  /** Unique among the kinds, for a choice to key on. */
  value: string;
  kind: FileKind;
  label: string;
  description?: string;
}

const PACKAGE_DESCRIPTION = "A package for Deployables";
const EMAIL_TEMPLATE_DESCRIPTION = "Opens in the email editor";

/** A kind of file as the choice offers it, and as the rail names it. */
export function fileKindEntry(kind: FileKind): FileKindEntry {
  switch (kind.kind) {
    case "document":
      return { value: kind.format, kind, label: formatWord(kind.format) };
    case "package":
      return { value: kind.deployableKind, kind, label: `${deployableWord(kind.deployableKind)} package`, description: PACKAGE_DESCRIPTION };
    case "emailTemplate":
      return { value: "emailTemplate", kind, label: "Email template", description: EMAIL_TEMPLATE_DESCRIPTION };
  }
}

const KINDS_IN_ORDER: readonly FileKind[] = [
  ...COMPOSE_FORMATS.map((format): FileKind => ({ kind: "document", format })),
  ...PACKAGE_KINDS.map((deployableKind): FileKind => ({ kind: "package", deployableKind })),
  { kind: "emailTemplate" },
];

/**
 * Every kind, in the order the choice offers them (spec 2.4). A package is one choice, not a
 * format crossed with a deploy kind: the cluster renders a package's page as HTML whatever
 * format is sent. The email template is the last, and the wizard offers it only to a draft
 * that already is one, which only a sender's setup makes.
 */
export const FILE_KINDS: readonly FileKindEntry[] = KINDS_IN_ORDER.map(fileKindEntry);

/** The format the cluster is asked for: a document's own, HTML for a package, JSON for an email template. */
export function formatOf(kind: FileKind): ComposeFormat {
  switch (kind.kind) {
    case "document":
      return kind.format;
    case "package":
      return "html";
    case "emailTemplate":
      return "json";
  }
}

// ---------------------------------------------------------------------------
// Where a draft starts
// ---------------------------------------------------------------------------

const holds = (text: string): boolean => text.trim() !== "";

/**
 * The picked things once each: the same file listed twice is one ingredient, as it is when a
 * person picks it twice. Two are the same when they have the same kind and the same ref, the
 * ref trimmed and otherwise as given. That is the right comparison under the bare-ids contract:
 * a ref is the id the cluster gave (or the query it was asked), spelled one way wherever it
 * comes from, so there is no canonical form to reconcile; sameEntityId is for comparing an id
 * from one source with an id from another (a template row against a draft's templateId).
 */
function distinct(items: readonly { kind: string; ref: string; label: string }[], accept: (kind: string) => kind is Ingredient["kind"]): Ingredient[] {
  const seen = new Set<string>();
  const out: Ingredient[] = [];
  for (const { kind, ref, label } of items) {
    const key = `${kind} ${ref.trim()}`;
    if (!accept(kind) || seen.has(key)) continue;
    seen.add(key);
    out.push({ kind, ref, label });
  }
  return out;
}

// Exhaustive by construction: a source kind added to the setup contract is a compile error here.
const INGREDIENT_KINDS: Record<Ingredient["kind"], true> = { concept_row: true, library_file: true, query: true };
const isIngredientKind = (kind: string): kind is Ingredient["kind"] => Object.hasOwn(INGREDIENT_KINDS, kind);

function emptyDraft(format: ComposeFormat): MakeDraft {
  return {
    fileKind: { kind: "document", format },
    templateId: "", ingredients: [], statement: "", name: "", folderId: "", accountId: "",
    passed: { from: false, describe: false },
  };
}

/**
 * A fresh draft. From the plus (`null`) every step starts answered but the name, From and
 * Describe still waiting. From another app's setup the sender has answered the kind of file
 * and the folder, and From and Describe count as passed (From is answered when the setup
 * carried sources), so the one thing left is what to call it. An email template is what a
 * setup's output kind asks for, with JSON or no format; the sender's other format wins, as
 * readComposeIntent has it. The setup's way back is the app's to keep; it is no part of what
 * a file is made from.
 */
export function draftFromSetup(setup: ComposeSetup | null, defaultFormat: ComposeFormat): MakeDraft {
  const plus = emptyDraft(defaultFormat);
  if (setup === null) return plus;
  const emailTemplate = setup.outputKind === "email_template" && (setup.format === undefined || setup.format === "json");
  return {
    ...plus,
    fileKind: emailTemplate ? { kind: "emailTemplate" } : { kind: "document", format: setup.format ?? defaultFormat },
    ingredients: distinct(setup.sources ?? [], isIngredientKind),
    folderId: setup.folderId ?? "",
    passed: { from: true, describe: true },
  };
}

function kindOfComposition(c: CompositionRow): FileKind {
  if (c.outputKind === "email_template") return { kind: "emailTemplate" };
  const deployableKind = PACKAGE_KINDS.find((candidate) => candidate === c.deployableKind);
  if (deployableKind !== undefined) return { kind: "package", deployableKind };
  // A record names a format this app writes; one it does not know falls back to the first kind.
  return { kind: "document", format: COMPOSE_FORMATS.find((candidate) => candidate === c.format) ?? "markdown" };
}

/**
 * Start over: every step answered from the record, with From and Describe passed, so nothing
 * waits and the wizard opens on Describe. The sources are the ones the record kept, once each.
 * The record's organization is its first.
 */
export function draftFromComposition(c: CompositionRow, sources: readonly SourceRow[]): MakeDraft {
  return {
    fileKind: kindOfComposition(c),
    templateId: c.templateId,
    ingredients: distinct(sources, isIngredientKind),
    statement: c.statement,
    name: c.name,
    folderId: c.folderId,
    accountId: c.accountIds[0] ?? "",
    passed: { from: true, describe: true },
  };
}

// ---------------------------------------------------------------------------
// The steps of a draft
// ---------------------------------------------------------------------------

/** The live templates that make this draft's kind: those of its format, or naming none (an archived one is not offered). */
export function templatesFor(draft: MakeDraft, facts: MakeFacts): TemplateRow[] {
  const format = formatOf(draft.fileKind);
  return facts.templates.filter((template) => !template.archived && (template.format === "" || template.format === format));
}

/**
 * The draft without a template that no longer makes its kind. The Template step shows only
 * the templates that do, so a stale choice would be sent from a step that cannot show it.
 * Apply it when the kind changes (withKind does), and to a draft made from a record, whose
 * template may since have been archived. While the templates are not loaded
 * (`facts.templatesLoaded`) the choice is kept whatever the list says: an absent list proves
 * nothing, and a valid choice must never be lost to a feed that has not settled or is between
 * reads. The component applies this again once the templates have loaded. The draft itself is
 * returned when there is nothing to let go.
 */
export function keepUsableTemplate(draft: MakeDraft, facts: MakeFacts): MakeDraft {
  if (draft.templateId === "" || !facts.templatesLoaded) return draft;
  const usable = templatesFor(draft, facts).some((template) => sameEntityId(template.id, draft.templateId));
  return usable ? draft : { ...draft, templateId: "" };
}

/**
 * The draft with its kind changed: THE ONLY WAY A KIND CHANGES. Choosing a kind has two
 * consequences the model owns, because requestOf holds no facts and sends whatever the draft
 * holds:
 *
 *  - a template that does not make the new kind is let go (keepUsableTemplate's rule). The
 *    Template step goes with it, so a template left behind would be sent from a step that is
 *    no longer there, and the floor would speak of a template the rail does not show;
 *  - the organization an email template carried is dropped when it becomes an ordinary file
 *    (D9: ordinary files ask for no organization). A draft that is not an email template and
 *    already carries one, as an older record's does, keeps it.
 */
export function withKind(draft: MakeDraft, kind: FileKind, facts: MakeFacts): MakeDraft {
  const leavingEmailTemplate = draft.fileKind.kind === "emailTemplate" && kind.kind !== "emailTemplate";
  return keepUsableTemplate({ ...draft, fileKind: kind, accountId: leavingEmailTemplate ? "" : draft.accountId }, facts);
}

/**
 * The steps this draft has, in order: Template only when a live template makes the kind,
 * Organization only for an email template or a draft that already carries one.
 */
export function stepsOf(draft: MakeDraft, facts: MakeFacts): MakeStepId[] {
  const steps: MakeStepId[] = ["kind"];
  if (templatesFor(draft, facts).length > 0) steps.push("template");
  steps.push("from", "describe", "name", "folder");
  if (draft.fileKind.kind === "emailTemplate" || draft.accountId !== "") steps.push("organization");
  steps.push("file");
  return steps;
}

const optionalState = (held: boolean, passed: boolean): MakeStepState => (held ? "done" : passed ? "skipped" : "waiting");
const fromState = (draft: MakeDraft): MakeStepState => optionalState(draft.ingredients.length > 0, draft.passed.from);
const describeState = (draft: MakeDraft): MakeStepState => optionalState(holds(draft.statement), draft.passed.describe);
const nameProblem = (draft: MakeDraft): string | undefined => fileNameProblem(draft.name, formatOf(draft.fileKind));

/**
 * Where a step stands on the rail. Kind of file, Template and Folder start answered. From
 * and Describe are answered when they hold something, skipped when passed with nothing, and
 * otherwise waiting. Name is answered when the SDK finds nothing wrong with it. Organization
 * is answered when set, waits for an email template without one, and is otherwise skipped.
 *
 * FILE IS NEVER WAITING: it is the cluster's step, not the person's, and the component draws
 * its own mark (ahead until the press, current while the cluster works). Reporting it as
 * waiting would make "the first step still waiting" and "a later step is waiting" mean File.
 * No step's state reads the facts; the signature takes them so that every function of the
 * model is called with the same pair.
 */
export function stepState(step: MakeStepId, draft: MakeDraft, _facts: MakeFacts): MakeStepState {
  switch (step) {
    case "kind":
    case "template":
    case "folder":
    case "file":
      return "done";
    case "from":
      return fromState(draft);
    case "describe":
      return describeState(draft);
    case "name":
      return nameProblem(draft) === undefined ? "done" : "waiting";
    case "organization":
      if (draft.accountId !== "") return "done";
      return draft.fileKind.kind === "emailTemplate" ? "waiting" : "skipped";
  }
}

/**
 * The step the wizard opens on: the first still waiting for the person, in the order of the
 * steps. A wizard with nothing waiting (Start over) opens on Describe, the answer most often
 * changed before trying again.
 */
export function openingStep(draft: MakeDraft, facts: MakeFacts): MakeStepId {
  return stepsOf(draft, facts).find((step) => stepState(step, draft, facts) === "waiting") ?? "describe";
}

// ---------------------------------------------------------------------------
// What the rail says
// ---------------------------------------------------------------------------

const DESCRIBE_LIMIT = 60;

/** Nothing picked, one name, "A and B", or "A, B and 3 more". */
export function ingredientsPhrase(ingredients: readonly Ingredient[]): string {
  const [first, second, ...rest] = ingredients.map((ingredient) => ingredient.label);
  if (first === undefined) return "Nothing picked";
  if (second === undefined) return first;
  if (rest.length === 0) return `${first} and ${second}`;
  return `${first}, ${second} and ${rest.length} more`;
}

/**
 * The first line of what was described (a line ends at a line feed, a carriage return, or a Unicode
 * line or paragraph separator), cut at a word within 60 characters and marked with an
 * ellipsis when cut; "Not described" for nothing. A line with no space in it is cut where the
 * limit falls. Characters are counted as the person sees them, so a cut never splits one.
 */
export function describeAnswer(statement: string): string {
  const line = (statement.trim().split(/\r\n|[\r\n\u2028\u2029]/, 1)[0] ?? "").trimEnd();
  if (line === "") return "Not described";
  const letters = Array.from(line);
  if (letters.length <= DESCRIBE_LIMIT) return line;
  const head = letters.slice(0, DESCRIBE_LIMIT).join("");
  // A space right after the limit means the head ends on a whole word; otherwise the last
  // word in it is partial and goes.
  const at = /\s/.test(letters[DESCRIBE_LIMIT] ?? "") ? -1 : head.search(/\s\S*$/);
  return `${(at > 0 ? head.slice(0, at) : head).trimEnd()}…`;
}

/**
 * What the rail shows for a step, or undefined where it shows nothing: a step still waiting
 * has no answer yet, and File is drawn by the component (it shows the filed name once the
 * file is made).
 */
export function answerOf(step: MakeStepId, draft: MakeDraft, names: AnswerNames): string | undefined {
  switch (step) {
    case "kind":
      return fileKindEntry(draft.fileKind).label;
    case "template":
      return draft.templateId === "" ? "No template" : names.template(draft.templateId);
    case "from":
      return fromState(draft) === "waiting" ? undefined : ingredientsPhrase(draft.ingredients);
    case "describe":
      return describeState(draft) === "waiting" ? undefined : describeAnswer(draft.statement);
    case "name":
      return nameProblem(draft) === undefined ? filedName(draft) : undefined;
    case "folder": {
      if (draft.folderId === "") return "Library";
      const folder = names.folder(draft.folderId);
      return folder === undefined ? undefined : (folder ?? "Unavailable folder");
    }
    case "organization":
      if (draft.accountId !== "") return names.account(draft.accountId);
      return draft.fileKind.kind === "emailTemplate" ? undefined : "No organization";
    case "file":
      return undefined;
  }
}

// ---------------------------------------------------------------------------
// The file as it will be named
// ---------------------------------------------------------------------------

// What the cluster keeps of the stem: at most this many bytes (sanitiseFileStem).
const MAX_STEM_BYTES = 120;

/**
 * The stem as the cluster files it, from the SDK's stem (the name without ONE trailing
 * extension of the kind). sanitiseFileStem, integrations/compose/materialize.go, also drops
 * a DEL character, trims dots from both ends, keeps at most 120 bytes and falls back to
 * "composition" for nothing. A slash, a backslash and a control character never get here:
 * fileNameProblem refuses them as the name is typed.
 */
function filedStem(stem: string): string {
  const clean = stem.replace(/\u007f/g, "").trim().replace(/^\.+|\.+$/g, "");
  const bytes = new TextEncoder().encode(clean);
  const kept = bytes.length > MAX_STEM_BYTES ? new TextDecoder().decode(bytes.slice(0, MAX_STEM_BYTES)).trim() : clean;
  return kept === "" ? "composition" : kept;
}

// How an email template is filed (the filing site in integrations/compose/materialize.go).
const JSON_EXTENSION = ".json";
const EMAIL_TEMPLATE_SUFFIX = ".email.json";

/**
 * The name the file is filed under, as the filing site in integrations/compose/materialize.go
 * makes it (outputFileName, and for an email template the suffix after it): the extension is
 * md for Markdown, zip for a package and otherwise the format; the stem is the name without
 * one trailing extension of the kind (fileNameStem); and a stem that already ends with the
 * extension does not get it twice. AN EMAIL TEMPLATE IS FILED AS <stem>.email.json: the name
 * made that way for any JSON file has its ".json" taken off, case-sensitively, and
 * ".email.json" put on. Meaningful once the name can be used (fileNameProblem finds nothing
 * wrong with it).
 *
 * The engine's table of these (integrations/compose/testdata/filed_names.json) is held to this
 * function by make.test.ts, and to the engine by a Go test.
 */
export function filedName(draft: MakeDraft): string {
  const format = formatOf(draft.fileKind);
  const extension = draft.fileKind.kind === "package" ? "zip" : format === "markdown" ? "md" : format;
  const stem = filedStem(fileNameStem(draft.name, format));
  const filed = stem.toLowerCase().endsWith(`.${extension}`) ? stem : `${stem}.${extension}`;
  if (draft.fileKind.kind !== "emailTemplate") return filed;
  return (filed.endsWith(JSON_EXTENSION) ? filed.slice(0, -JSON_EXTENSION.length) : filed) + EMAIL_TEMPLATE_SUFFIX;
}

// ---------------------------------------------------------------------------
// The request, and the path it takes
// ---------------------------------------------------------------------------

function sourceOf(ingredient: Ingredient): MaterializeSource {
  const { kind, ref, label } = ingredient;
  return kind === "library_file" ? { kind, ref, label, content: true } : { kind, ref, label };
}

/**
 * What a press sends (spec 2.10), every unanswered optional left out: the name as typed,
 * trimmed; the kind's format; a package's deployable kind or an email template's output
 * kind; the template, the folder (not the top level), the organization, the description and
 * the picked things. The request id is the ask's, not the draft's.
 */
export function requestOf(draft: MakeDraft): Omit<MaterializeRequest, "requestId"> {
  const kind = draft.fileKind;
  const statement = draft.statement.trim();
  const request: Omit<MaterializeRequest, "requestId"> = { name: draft.name.trim(), format: formatOf(kind) };
  if (statement !== "") request.statement = statement;
  if (draft.ingredients.length > 0) request.sources = draft.ingredients.map(sourceOf);
  if (draft.templateId !== "") request.templateId = draft.templateId;
  if (draft.folderId !== "") request.folderId = draft.folderId;
  if (kind.kind === "emailTemplate") request.outputKind = "email_template";
  if (draft.accountId !== "") request.accountIds = [draft.accountId];
  if (kind.kind === "package") request.deployableKind = kind.deployableKind;
  return request;
}

/**
 * What the cluster will do with this draft. Blank: no description, nothing picked, no
 * template and a document kind, so an empty file with no model call. Described: the SDK's
 * isDescribedMarkdown over the request and the picked files' rows, so the file is filed
 * empty and its description is handed to the Library's revision workflow. Composed:
 * everything else, written by a model that reads everything picked.
 */
export function pathOf(draft: MakeDraft, facts: MakeFacts): MakePath {
  if (!holds(draft.statement) && draft.ingredients.length === 0 && draft.templateId === "" && draft.fileKind.kind === "document") return "blank";
  return isDescribedMarkdown(requestOf(draft), facts.files) ? "described" : "composed";
}

// ---------------------------------------------------------------------------
// The floor
// ---------------------------------------------------------------------------

// The formats the cluster can file as an empty document (RenderBlank, component/compose/write.go).
const BLANK_FORMATS: readonly ComposeFormat[] = ["markdown", "txt", "html", "csv", "json"];
const canStartBlank = (kind: FileKind): boolean => kind.kind === "document" && BLANK_FORMATS.includes(kind.format);

const NEEDS_SOMETHING = "Describe it or pick what to make it from";
// What it says of a kind that cannot start blank: the engine's own refusal of one (ErrBlankFormat).
const BLANK_DETAIL = "A blank file can be Markdown, text, HTML, CSV or JSON.";

/**
 * What is missing before the file can be made, or null: the first that matches, in the
 * spec's order (2.3). The inference words carry no detail; the component adds it by who can
 * set Inference up. A cluster that does not know yet whether Inference is set up (null) does
 * not block, and neither does a blank file, which needs no model.
 */
export function blocker(draft: MakeDraft, facts: MakeFacts): Blocker | null {
  if (nameProblem(draft) !== undefined) return { reason: "name", words: { word: "Name the file" } };
  const templated = draft.templateId !== "";
  if (!holds(draft.statement) && draft.ingredients.length === 0 && (templated || !canStartBlank(draft.fileKind))) {
    return {
      reason: "blank",
      words: { word: NEEDS_SOMETHING, detail: templated ? "A template needs something to fill it." : BLANK_DETAIL },
    };
  }
  if (draft.fileKind.kind === "emailTemplate" && draft.accountId === "") return { reason: "organization", words: { word: "Choose an organization" } };
  if (facts.inference === false && pathOf(draft, facts) !== "blank") return { reason: "inference", words: { word: "Making this needs Inference" } };
  return null;
}

const READY_DETAIL: Record<MakePath, string> = {
  blank: "It starts blank.",
  described: "You review its draft in the editor.",
  composed: "A model writes it.",
};

/** The floor when nothing is missing: the name the file will be filed under, and what happens next. */
export function readyWords(draft: MakeDraft, facts: MakeFacts): FloorWords {
  return { word: `Ready to make ${filedName(draft)}`, detail: READY_DETAIL[pathOf(draft, facts)] };
}

/**
 * The acts the floor offers, Cancel and at most one forward act (rule 12: an act that is not
 * legal is absent, never disabled). While the file is being made it is Stop and Leave.
 * Otherwise the forward act is Make the file when nothing is missing, Set up Inference when
 * that is what is missing and this person can reach it, and Next while a step after the open
 * one is still waiting; with none of those there is Cancel alone.
 */
export function floorActs(args: {
  draft: MakeDraft;
  facts: MakeFacts;
  open: MakeStepId;
  making: boolean;
  canSetUpInference: boolean;
}): MakeAct[] {
  const { draft, facts, open, making, canSetUpInference } = args;
  if (making) return ["stop", "leave"];
  const missing = blocker(draft, facts);
  if (missing === null) return ["cancel", "make"];
  if (missing.reason === "inference" && canSetUpInference) return ["cancel", "setUpInference"];
  const steps = stepsOf(draft, facts);
  const later = steps.slice(steps.indexOf(open) + 1);
  return later.some((step) => stepState(step, draft, facts) === "waiting") ? ["cancel", "next"] : ["cancel"];
}

// ---------------------------------------------------------------------------
// What the tray offers
// ---------------------------------------------------------------------------

// The extensions the composer reads (integrations/compose/source_content.go refuses the whole
// request for any other). Which of them a description can take is not decided here: that is
// the SDK's isDescribedMarkdown.
const READABLE_EXTENSIONS: ReadonlySet<string> = new Set([
  ".md", ".markdown", ".txt", ".html", ".htm", ".css", ".json", ".csv", ".svg", ".png", ".jpg", ".jpeg", ".gif", ".zip",
]);

/**
 * Whether the composer can read a file of this name, so the tray offers it. The extension is
 * Go's path.Ext lower-cased as Go's strings.ToLower does: a capital dotted I (U+0130) is an
 * ordinary i to Go, where JavaScript's toLowerCase adds a combining dot to it.
 */
export function readableReference(name: string): boolean {
  const element = name.slice(name.lastIndexOf("/") + 1);
  const dot = element.lastIndexOf(".");
  return dot >= 0 && READABLE_EXTENSIONS.has(element.slice(dot).replace(/\u0130/g, "i").toLowerCase());
}
