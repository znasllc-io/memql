// The email document: the structured `*.email.json` file an email template is
// written in, as the editor reads, edits and writes it.
//
// Pure: no vscode, no SDK, so the view bundles and the host share it.
//
// The format and its rules belong to the engine (component/compose/emaildoc:
// doc.go and parse.go). This module mirrors the parts of them the editor needs
// and a test (test/email-document.test.ts) compares the tables against that
// source, so a change there fails here. It is not a second parser. readEmail
// checks the shape the editor relies on (the mode, the top-level keys, the
// subject, the bodies, that every block has an id and a known type) and stops
// there: unknown block keys and out-of-range values are kept untouched, and the
// server's Parse and the Source schema (emailSchema) are the validators of
// everything else. Not mirrored on purpose: duplicate keys (JSON.parse keeps the
// last), block ids and where blocks may sit, value ranges, Markdown and html
// markup, and the opaque layout id.

import { UserInputError } from "./userError.js";

// -- Vocabulary ---------------------------------------------------------------

export const THEME_TOKENS = ["page", "canvas", "primary", "secondary", "accent", "tint", "text", "muted", "white"] as const;
export type ThemeToken = typeof THEME_TOKENS[number];
export const LINK_TAGS = ["{{unsubscribeUrl}}", "{{preferencesUrl}}", "{{viewUrl}}", "{{confirmUrl}}"] as const;
export const BLOCK_TYPES = ["image", "heading", "text", "highlight", "button", "divider", "spacer", "section", "columns", "social", "footer", "html"] as const;
export type BlockType = typeof BLOCK_TYPES[number];

export interface EmailTheme { page: string; canvas: string; primary: string; secondary: string; accent: string; tint: string; text: string; muted: string; headingFont: string; bodyFont: string; webFonts?: boolean }
export interface EmailColumn { width: number; cellPadding?: number; blocks: EmailBlock[] }
export interface EmailBlock { id: string; type: BlockType; role?: "brand" | "content"; blocks?: EmailBlock[]; columns?: EmailColumn[]; [field: string]: unknown }
export interface LayoutEmail { subject: string; preheader?: string; layout?: string; theme: EmailTheme; blocks: EmailBlock[] }
export interface CustomEmail { subject: string; preheader?: string; textBody: string; htmlBody: string }
export type EmailDocument = LayoutEmail | CustomEmail;

export type EditableField = "text" | "label" | "href" | "alt" | "src" | "color" | "background" | "align" | "height" | "title" | "links" | "lines" | "html";
export type AddableType = "heading" | "text" | "highlight" | "button" | "image" | "divider" | "spacer";
export type EmailEdit =
  | { kind: "subject"; value: string }
  | { kind: "preheader"; value: string }
  | { kind: "field"; block: string; field: EditableField; value: string | number | string[] | Array<Record<string, string>> }
  | { kind: "move"; block: string; by: -1 | 1 }
  | { kind: "add"; after: string; type: AddableType }
  | { kind: "remove"; block: string }
  | { kind: "role"; block: string; brand: boolean }
  | { kind: "theme"; key: Exclude<keyof EmailTheme, "webFonts">; value: string };

/** How a block is named wherever the editor lists blocks (the outline and a selected block's title). */
export const BLOCK_NAMES: Readonly<Record<BlockType, string>> = {
  image: "Picture", heading: "Heading", text: "Text", highlight: "Highlight", button: "Button", divider: "Divider", spacer: "Space",
  section: "Section", columns: "Columns", social: "Social links", footer: "Footer", html: "HTML",
};

// -- Field tables -------------------------------------------------------------

/** The order the engine writes a block's keys in (the Block struct in doc.go). */
const BLOCK_KEY_ORDER = [
  "id", "type", "role", "padding", "text", "size", "lineHeight", "listLineHeight", "gap", "color", "textColor", "linkColor", "background", "align",
  "italic", "weight", "font", "letterSpacing", "src", "alt", "width", "height", "href", "bleed", "label", "paddingY", "inset", "maxWidth",
  "cellPadding", "stackBelow", "blocks", "columns", "title", "links", "lines", "separator", "after", "html",
] as const;
const THEME_KEY_ORDER = ["page", "canvas", "primary", "secondary", "accent", "tint", "text", "muted", "headingFont", "bodyFont", "webFonts"] as const;
/** Both modes in one list: each mode's keys keep the engine's order inside it. */
const TOP_KEY_ORDER = ["subject", "preheader", "layout", "theme", "blocks", "textBody", "htmlBody"] as const;
const LAYOUT_KEYS: readonly string[] = ["subject", "preheader", "layout", "theme", "blocks"];
const CUSTOM_KEYS: readonly string[] = ["subject", "preheader", "textBody", "htmlBody"];
/** The theme's own colours, in the order the theme lists them. White is a token blocks may use, not one the theme sets. */
export const THEME_COLOURS = ["page", "canvas", "primary", "secondary", "accent", "tint", "text", "muted"] as const;
const THEME_FONTS: readonly string[] = ["headingFont", "bodyFont"];

/** Each type's own keys (parse.go, blockKeys); every type also takes id, type and role, and every type but spacer takes padding. */
const OWN_FIELDS: Readonly<Record<BlockType, string>> = {
  image: "src alt width height align href bleed",
  heading: "text size lineHeight color align italic weight font letterSpacing",
  text: "text size lineHeight listLineHeight gap color linkColor align italic weight font letterSpacing",
  highlight: "text size lineHeight gap color background align italic weight font letterSpacing",
  button: "label href color textColor width align size paddingY font",
  divider: "color height",
  spacer: "height",
  section: "background inset maxWidth cellPadding blocks",
  columns: "background inset maxWidth gap stackBelow columns",
  social: "title links maxWidth gap",
  footer: "links lines background inset maxWidth cellPadding size lineHeight letterSpacing color linkColor separator gap after font",
  html: "html",
};

/** Every key a block may carry, by type, in the order the engine writes them. */
export const BLOCK_FIELDS: Readonly<Record<BlockType, readonly string[]>> = (() => {
  const fields = {} as Record<BlockType, readonly string[]>;
  for (const type of BLOCK_TYPES) {
    const allowed = new Set(["id", "type", "role", ...OWN_FIELDS[type].split(" "), ...(type === "spacer" ? [] : ["padding"])]);
    fields[type] = BLOCK_KEY_ORDER.filter(key => allowed.has(key));
  }
  return fields;
})();

/** The fields the inspector edits, by type, in display order. Sizes, spacing and padding stay in Source. */
export const EDITABLE_FIELDS: Readonly<Record<BlockType, readonly EditableField[]>> = {
  image: ["src", "alt", "href", "align"],
  heading: ["text", "color", "align"],
  text: ["text", "color", "align"],
  highlight: ["text", "color", "background", "align"],
  button: ["label", "href", "color", "align"],
  divider: ["color"],
  spacer: ["height"],
  section: ["background"],
  columns: ["background"],
  social: ["title", "links"],
  footer: ["links", "lines", "color", "background"],
  html: ["html"],
};

// -- Rules and sentences ------------------------------------------------------

const MAX_BYTES = 2 * 1024 * 1024;
const MAX_SUBJECT_BYTES = 998;
const MAX_PREHEADER = 200;
const MAX_HEIGHT = 2000;
/** The engine allows three levels (a section, columns in it, a column's blocks); more than this is not an email. */
const MAX_DEPTH = 16;

const ID_PATTERN = "^[a-z0-9][a-z0-9-]{0,63}$";
const HEX_PATTERN = "^#[0-9a-fA-F]{6}$";
const FONT_PATTERN = "^[A-Za-z][A-Za-z0-9 -]{0,39}$";
const SPACING_PATTERN = "^-?[0-9]{1,2}(\\.[0-9]{1,3})?em$";
const PICTURE_PATTERN = "^(?:|artifact:[A-Za-z0-9_-]{1,160}|data:image/(?:png|jpeg|gif);base64,[A-Za-z0-9+/]+={0,2})$";
const HEX = new RegExp(HEX_PATTERN);
const FONT = new RegExp(FONT_PATTERN);
const PICTURE = new RegExp(PICTURE_PATTERN);

// What the engine calls a space (Go's unicode.IsSpace), a control character (unicode.IsControl), and a character that never belongs in a link.
const SPACES = "\\u0009-\\u000d\\u0020\\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000";
const CONTROLS = "\\u0000-\\u001f\\u007f-\\u009f";
const UNSAFE_IN_LINK = "\\u0000-\\u0020\\u007f-\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000\"<>`\\\\";
const NOT_A_SPACE = new RegExp(`[^${SPACES}]`);
const HAS_CONTROL = new RegExp(`[${CONTROLS}]`);
/** For the schema: a subject has something in it and sits on one line; a preheader sits on one line; a plain-text body has something in it. */
const SUBJECT_PATTERN = `^(?=.*[^${SPACES}])[^${CONTROLS}]+$`;
const ONE_LINE_PATTERN = `^[^${CONTROLS}]*$`;
const SOMETHING_PATTERN = `[^${SPACES}]`;
const UNSAFE_LINK_CHAR = new RegExp(`[${UNSAFE_IN_LINK}]`);

const SENTENCES = {
  tooLarge: "Emails must be 2 MiB or smaller.",
  invalidJson: "This email has invalid JSON. Open Source and correct its formatting.",
  notObject: "An email must be a JSON object.",
  bothModes: "An email is either a layout (blocks) or custom HTML (textBody and htmlBody), not both.",
  neitherMode: "An email needs blocks (a layout) or textBody and htmlBody (custom HTML).",
  subject: "Enter a subject on one line.",
  subjectLength: "Enter a shorter subject.",
  preheader: "The preheader must be one line of 200 characters or fewer.",
  bodies: "The email needs textBody and htmlBody strings.",
  plainText: "Include a plain-text version.",
  layoutShape: "A layout email needs a theme and a list of blocks.",
  blockShape: "Every block needs an id and a type.",
  gone: "That block is no longer in the email.",
  colour: "Choose one of the theme colours.",
  link: "Enter a web address, an email address (mailto:), a phone number (tel:) or a link tag such as {{unsubscribeUrl}}.",
  themeColour: "Enter a colour as #rrggbb.",
  font: "Enter a font name using letters, digits, spaces or hyphens.",
  move: "This block can’t move further.",
  writtenInHtml: "This email is written in HTML. Change it in Source.",
  noField: "This block doesn’t have that field.",
  align: "Choose left, center or right.",
  height: `Enter a height between 0 and ${MAX_HEIGHT} pixels.`,
  picture: "A picture is artifact:<id> from Files, an embedded PNG, JPEG or GIF, or empty.",
} as const;

const refusal = (message: string) => new UserInputError(message);
/** A value the page should never send: a mistake in the editor, not something to tell the person. */
const misuse = (what: string) => new Error(`The email editor sent an edit it cannot apply: ${what}`);

// -- Small helpers ------------------------------------------------------------

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);
const has = (object: object, key: string) => Object.prototype.hasOwnProperty.call(object, key);
const utf8Length = (text: string) => new TextEncoder().encode(text).length;
const isBlank = (text: string) => !NOT_A_SPACE.test(text);
const isToken = (value: unknown): value is ThemeToken => (THEME_TOKENS as readonly unknown[]).includes(value);
const isBlockType = (value: unknown): value is BlockType => (BLOCK_TYPES as readonly unknown[]).includes(value);
/** The keys a block of this type may carry; none for a type that is not one (a document that did not come through readEmail). */
const fieldsOf = (type: unknown): readonly string[] => isBlockType(type) ? BLOCK_FIELDS[type] : [];
const clone = <T>(value: T): T => JSON.parse(JSON.stringify(value)) as T;

/** At most `limit` characters of a name, for a sentence that quotes it: a hostile 2 MiB key must not become the message. */
function quote(text: string, limit = 60): string {
  let head = "", count = 0;
  for (const character of text) {
    if (count++ === limit) return JSON.stringify(head + "...");
    head += character;
  }
  return JSON.stringify(text);
}

/** Whether `text` has more than `limit` characters (code points), counting no further than it needs to. */
function longerThan(text: string, limit: number): boolean {
  if (text.length <= limit) return false;
  let count = 0;
  for (const _ of text) if (++count > limit) return true;
  return false;
}

/** Sets a key, in the engine's position when it is new, and never reorders the keys that are there. */
function setKey(object: object, key: string, value: unknown, order: readonly string[]): void {
  const target = object as Record<string, unknown>;
  if (has(target, key)) { target[key] = value; return; }
  const rank = (name: string) => { const at = order.indexOf(name); return at < 0 ? Infinity : at; };
  const entries = Object.entries(target);
  const before = entries.findIndex(([name]) => rank(name) > rank(key));
  entries.splice(before < 0 ? entries.length : before, 0, [key, value]);
  for (const [name] of entries) delete target[name];
  for (const [name, item] of entries) Object.defineProperty(target, name, { value: item, writable: true, enumerable: true, configurable: true });
}

// -- Reading and writing ------------------------------------------------------

export function isLayoutEmail(doc: EmailDocument): doc is LayoutEmail {
  return Array.isArray((doc as LayoutEmail).blocks);
}

function checkSubject(value: unknown): asserts value is string {
  if (typeof value !== "string" || isBlank(value) || HAS_CONTROL.test(value)) throw refusal(SENTENCES.subject);
  if (utf8Length(value) > MAX_SUBJECT_BYTES) throw refusal(SENTENCES.subjectLength);
}

function checkPreheader(value: unknown): asserts value is string {
  if (typeof value !== "string" || HAS_CONTROL.test(value) || longerThan(value, MAX_PREHEADER)) throw refusal(SENTENCES.preheader);
}

function checkBlocks(blocks: unknown[], depth: number): void {
  if (depth > MAX_DEPTH) throw refusal(SENTENCES.layoutShape);
  for (const block of blocks) {
    if (!isRecord(block) || typeof block.id !== "string" || block.id === "" || typeof block.type !== "string" || block.type === "") throw refusal(SENTENCES.blockShape);
    if (!isBlockType(block.type)) throw refusal(`Block ${quote(block.id)} has a type MemQL doesn’t know: ${quote(block.type)}. Fix it in Source.`);
    // A nested list that is absent or null is no blocks, as in the engine.
    if (block.type === "section" && block.blocks != null) {
      if (!Array.isArray(block.blocks)) throw refusal(SENTENCES.layoutShape);
      checkBlocks(block.blocks, depth + 1);
    }
    if (block.type === "columns" && block.columns != null) {
      if (!Array.isArray(block.columns)) throw refusal(SENTENCES.layoutShape);
      for (const column of block.columns) {
        if (!isRecord(column) || (column.blocks != null && !Array.isArray(column.blocks))) throw refusal(SENTENCES.layoutShape);
        if (Array.isArray(column.blocks)) checkBlocks(column.blocks, depth + 1);
      }
    }
  }
}

/**
 * Reads the text of an `*.email.json` file. The document is the parsed object
 * itself, so writing it back changes nothing the person did not change. Refusals
 * are UserInputErrors: sentences the person reads.
 */
export function readEmail(text: string): EmailDocument {
  if (utf8Length(text) > MAX_BYTES) throw refusal(SENTENCES.tooLarge);
  let parsed: unknown;
  try { parsed = JSON.parse(text); } catch { throw refusal(SENTENCES.invalidJson); }
  if (!isRecord(parsed)) throw refusal(SENTENCES.notObject);

  const layout = has(parsed, "blocks");
  const custom = has(parsed, "textBody") || has(parsed, "htmlBody");
  if (layout && custom) throw refusal(SENTENCES.bothModes);
  if (!layout && !custom) throw refusal(SENTENCES.neitherMode);
  const allowed = layout ? LAYOUT_KEYS : CUSTOM_KEYS;
  const unknown = Object.keys(parsed).filter(key => !allowed.includes(key)).sort()[0];
  if (unknown !== undefined) throw refusal(`Remove ${quote(unknown)} in Source. Emails don’t have that field.`);

  checkSubject(parsed.subject);
  if (has(parsed, "preheader")) checkPreheader(parsed.preheader);
  if (custom) {
    if (typeof parsed.textBody !== "string" || typeof parsed.htmlBody !== "string") throw refusal(SENTENCES.bodies);
    if (isBlank(parsed.textBody)) throw refusal(SENTENCES.plainText);
  } else {
    if (!isRecord(parsed.theme) || !Array.isArray(parsed.blocks)) throw refusal(SENTENCES.layoutShape);
    checkBlocks(parsed.blocks, 0);
  }
  return parsed as unknown as EmailDocument;
}

/** U+2028 and U+2029, which JSON.stringify writes as they are and Go's encoder always writes as escapes. */
const SEPARATORS = /[\u2028\u2029]/g;

/**
 * The text of an email file as the engine writes one (Document.JSON in doc.go): two-space indentation, one trailing
 * newline, markup and characters outside ASCII as they are, and U+2028 and U+2029 as \u2028 and \u2029. Every
 * `*.email.json` the editor writes goes through here, the text a template opens as included (documents.ts), so a file the
 * engine wrote is written again byte for byte (testdata/writer.email.json and the built-in layouts hold the two together).
 */
export function writeEmail(doc: EmailDocument | Readonly<Record<string, unknown>>): string {
  return JSON.stringify(doc, null, 2).replace(SEPARATORS, character => character === "\u2028" ? "\\u2028" : "\\u2029") + "\n";
}

// -- Walking ------------------------------------------------------------------

type Visit = (block: EmailBlock, depth: number, siblings: EmailBlock[], index: number, brand: boolean, inherited: boolean) => void;

/** The block lists a block holds: a section's blocks, or each column's. */
function childLists(block: EmailBlock): EmailBlock[][] {
  if (block.type === "section" && Array.isArray(block.blocks)) return [block.blocks];
  if (block.type === "columns" && Array.isArray(block.columns)) return block.columns.filter(column => isRecord(column) && Array.isArray(column.blocks)).map(column => column.blocks);
  return [];
}

function walk(siblings: EmailBlock[], depth: number, inherited: boolean, visit: Visit): void {
  for (let index = 0; index < siblings.length; index++) {
    const block = siblings[index] as EmailBlock;
    // A block's role is its own, else its container's, else content.
    const brand = block.role === "brand" ? true : block.role === "content" ? false : inherited;
    visit(block, depth, siblings, index, brand, inherited);
    for (const children of childLists(block)) walk(children, depth + 1, brand, visit);
  }
}

/** Visits every block in document order: a block, then what it holds. `brand` is the block's effective role. */
export function walkBlocks(blocks: readonly EmailBlock[], visit: (block: EmailBlock, depth: number, siblings: EmailBlock[], index: number, brand: boolean) => void): void {
  walk(blocks as EmailBlock[], 0, false, (block, depth, siblings, index, brand) => visit(block, depth, siblings, index, brand));
}

interface Found { block: EmailBlock; siblings: EmailBlock[]; index: number; depth: number; brand: boolean; inherited: boolean }

function locate(doc: LayoutEmail, id: string): Found | undefined {
  let found: Found | undefined;
  walk(doc.blocks, 0, false, (block, depth, siblings, index, brand, inherited) => {
    if (!found && block.id === id) found = { block, siblings, index, depth, brand, inherited };
  });
  return found;
}

export function findBlock(doc: LayoutEmail, id: string): { block: EmailBlock; siblings: EmailBlock[]; index: number; depth: number; brand: boolean } | undefined {
  const found = locate(doc, id);
  return found && { block: found.block, siblings: found.siblings, index: found.index, depth: found.depth, brand: found.brand };
}

export function canMove(doc: LayoutEmail, id: string, by: -1 | 1): boolean {
  const found = locate(doc, id);
  return !!found && found.index + by >= 0 && found.index + by < found.siblings.length;
}

// -- Naming blocks ------------------------------------------------------------

/** How much of a block's text the label reads: the label shows 40 characters, and a huge block must not make the outline slow. */
const LABEL_SOURCE = 2000;

/** Markdown as the words it shows: links by their label, markers and list bullets gone, escapes undone. */
function plainMarkdown(markdown: string): string {
  const escaped: string[] = [];
  return markdown
    .replace(/\\([\\*[\]()])/g, (_, character: string) => `\u0000${escaped.push(character) - 1}\u0000`)
    .replace(/\[([^\]]*)\]\((?:[^()]|\([^()]*\))*\)/g, "$1")
    .replace(/^[ \t]*(?:- |\d+\. )/gm, "")
    .replace(/(\*{1,3})(?=\S)([^*]*?\S)\1/g, "$2")
    .replace(/\u0000(\d+)\u0000/g, (_, at: string) => escaped[Number(at)] ?? "");
}

function wordsOf(block: EmailBlock): string {
  const text = (value: unknown) => typeof value === "string" ? value : "";
  switch (block.type) {
    case "image": return text(block.alt);
    case "heading": case "highlight": return text(block.text);
    case "text": return plainMarkdown(text(block.text).slice(0, LABEL_SOURCE));
    case "button": return text(block.label);
    case "social": return text(block.title);
    default: return "";
  }
}

/** "Heading · Thanks for joining us": the block's name and the first 40 characters of its words. */
export function blockLabel(block: EmailBlock): string {
  const name = isBlockType(block.type) ? BLOCK_NAMES[block.type] : String(block.type);
  const characters = Array.from(wordsOf(block).slice(0, LABEL_SOURCE).replace(/\s+/g, " ").trim());
  if (characters.length === 0) return name;
  return `${name} · ${characters.length > 40 ? characters.slice(0, 40).join("") + "…" : characters.join("")}`;
}

// -- Links --------------------------------------------------------------------

const WEB_SCHEME = /^https?:\/\//i;
const MAIL_SCHEME = /^(?:mailto|tel):/i;

/** The part of a web address that names the site: after the slashes, up to the first "/", "?" or "#". */
function authorityOf(afterScheme: string): string {
  const rest = afterScheme.replace(/^\/+/, "");
  const end = rest.search(/[/?#]/);
  return end < 0 ? rest : rest.slice(0, end);
}

/**
 * Whether `href` may stand as a link (engine hrefProblem, allowing empty): a web
 * address, an email address, a phone number or exactly one of the four link tags,
 * at most 2000 bytes, with no space, control character, quote, angle bracket,
 * backtick or backslash. A web address takes merge tags in its path, query and
 * fragment but not in its host, user name or port, and an email or phone link
 * takes none: a value a recipient supplied must never choose where a link goes.
 */
export function validLink(href: string): boolean {
  if (typeof href !== "string") return false;
  if (href === "" || (LINK_TAGS as readonly string[]).includes(href)) return true;
  if (LINK_TAGS.some(tag => href.includes(tag))) return false;
  if (utf8Length(href) > 2000 || UNSAFE_LINK_CHAR.test(href)) return false;
  const web = WEB_SCHEME.exec(href);
  if (web && href.length > web[0].length) return !authorityOf(href.slice(web[0].length)).includes("{{");
  const mail = MAIL_SCHEME.exec(href);
  if (mail && href.length > mail[0].length) return !href.includes("{{");
  return false;
}

/** validLink as a regular expression, for the schema. Schemes are matched letter by letter: a pattern has no flags. */
const LINK_PATTERN = "^(?:" + [
  "",
  LINK_TAGS.map(tag => tag.replace(/[{}]/g, "\\$&")).join("|"),
  `(?!.*(?:${LINK_TAGS.map(tag => tag.replace(/[{}]/g, "\\$&")).join("|")}))[Hh][Tt][Tt][Pp][Ss]?:\\/\\/(?!\\/*[^\\/?#]*\\{\\{)[^${UNSAFE_IN_LINK}]+`,
  `(?:[Mm][Aa][Ii][Ll][Tt][Oo]|[Tt][Ee][Ll]):(?!.*\\{\\{)[^${UNSAFE_IN_LINK}]+`,
].join("|") + ")$";

// -- Edits --------------------------------------------------------------------

/** Fields where an empty value means "not set": the key goes, as the engine writes nothing for it. */
const CLEARED_WHEN_EMPTY: ReadonlySet<EditableField> = new Set(["href", "alt", "title", "color", "background", "align"]);

/** The id `editEmail` gives the block an `add` edit makes: `<type>-<n>` with the smallest n not used by any block. */
export function addedBlockId(doc: LayoutEmail, edit: Extract<EmailEdit, { kind: "add" }>): string {
  const taken = new Set<string>();
  walkBlocks(doc.blocks, block => taken.add(block.id));
  for (let n = 1; ; n++) if (!taken.has(`${edit.type}-${n}`)) return `${edit.type}-${n}`;
}

function newBlock(type: AddableType, id: string): EmailBlock {
  switch (type) {
    case "heading": case "text": case "highlight": return { id, type, text: "" };
    case "button": return { id, type, label: "Button", href: "" };
    case "image": return { id, type, src: "", alt: "", width: 552 };
    case "divider": return { id, type };
    case "spacer": return { id, type, height: 24 };
    default: throw misuse(`a ${String(type)} block cannot be added`);
  }
}

function heightOf(value: unknown): number {
  const number = typeof value === "number" ? value : typeof value === "string" && /^\s*\d+(\.\d+)?\s*$/.test(value) ? Number(value) : Number.NaN;
  if (!Number.isFinite(number) || number < 0 || number > MAX_HEIGHT) throw refusal(SENTENCES.height);
  return number;
}

function checkLinks(value: unknown): void {
  if (!Array.isArray(value)) throw misuse("links must be a list");
  for (const link of value) {
    if (!isRecord(link)) throw misuse("a link must be an object");
    if (link.href !== undefined && (typeof link.href !== "string" || !validLink(link.href))) throw refusal(SENTENCES.link);
  }
}

function setField(block: EmailBlock, field: EditableField, given: unknown): void {
  let value = given;
  switch (field) {
    case "color": case "background":
      if (value !== "" && !isToken(value)) throw refusal(SENTENCES.colour);
      break;
    case "href":
      if (typeof value !== "string" || !validLink(value)) throw refusal(SENTENCES.link);
      break;
    case "src":
      // The engine's pictureProblem refuses every other source, a web address included: an email carries its pictures.
      if (typeof value !== "string" || !PICTURE.test(value)) throw refusal(SENTENCES.picture);
      break;
    case "align":
      if (value !== "" && value !== "left" && value !== "center" && value !== "right") throw refusal(SENTENCES.align);
      break;
    case "height":
      value = heightOf(value);
      break;
    case "links":
      checkLinks(value);
      break;
    case "lines":
      if (!Array.isArray(value) || value.some(line => typeof line !== "string")) throw misuse("lines must be a list of text");
      break;
    default:
      if (typeof value !== "string") throw misuse(`${field} must be text`);
  }
  if (value === "" && CLEARED_WHEN_EMPTY.has(field)) delete block[field];
  else setKey(block, field, value, BLOCK_KEY_ORDER);
}

type LayoutEdit = Exclude<EmailEdit, { kind: "subject" | "preheader" }>;

function editLayout(next: LayoutEmail, edit: LayoutEdit): LayoutEmail {
  if (edit.kind === "theme") {
    if ((THEME_COLOURS as readonly string[]).includes(edit.key)) { if (!HEX.test(edit.value)) throw refusal(SENTENCES.themeColour); }
    else if (THEME_FONTS.includes(edit.key)) { if (!FONT.test(edit.value)) throw refusal(SENTENCES.font); }
    else throw misuse(`${String(edit.key)} is not a theme field`);
    setKey(next.theme, edit.key, edit.value, THEME_KEY_ORDER);
    return next;
  }

  const found = locate(next, edit.kind === "add" ? edit.after : edit.block);
  if (!found) throw refusal(SENTENCES.gone);
  switch (edit.kind) {
    case "field":
      if (!fieldsOf(found.block.type).includes(edit.field)) throw refusal(SENTENCES.noField);
      setField(found.block, edit.field, edit.value);
      return next;
    case "move": {
      if (!canMove(next, edit.block, edit.by)) throw refusal(SENTENCES.move);
      const neighbour = found.siblings[found.index + edit.by] as EmailBlock;
      found.siblings[found.index + edit.by] = found.block;
      found.siblings[found.index] = neighbour;
      return next;
    }
    case "add":
      found.siblings.splice(found.index + 1, 0, newBlock(edit.type, addedBlockId(next, edit)));
      return next;
    case "remove":
      found.siblings.splice(found.index, 1);
      return next;
    case "role":
      // Content only needs saying inside a brand container; anywhere else it is what a block without a role is.
      if (edit.brand) setKey(found.block, "role", "brand", BLOCK_KEY_ORDER);
      else if (found.inherited) setKey(found.block, "role", "content", BLOCK_KEY_ORDER);
      else delete found.block.role;
      return next;
  }
}

/**
 * Applies one inspector edit and returns a new document; the one passed in is
 * left alone, and every key the edit does not name stays as it was. Refusals the
 * person can cause are UserInputErrors with the sentence to show.
 */
export function editEmail(doc: EmailDocument, edit: EmailEdit): EmailDocument {
  const next = clone(doc);
  switch (edit.kind) {
    case "subject":
      checkSubject(edit.value);
      next.subject = edit.value;
      return next;
    case "preheader":
      if (edit.value === "") delete next.preheader;
      else { checkPreheader(edit.value); setKey(next, "preheader", edit.value, TOP_KEY_ORDER); }
      return next;
    case "theme": case "field": case "move": case "add": case "remove": case "role":
      if (!isLayoutEmail(next)) throw refusal(SENTENCES.writtenInHtml);
      return editLayout(next, edit);
    default:
      throw misuse(`${String((edit as { kind?: unknown }).kind)} is not an edit`);
  }
}

// -- Comparing and inspecting -------------------------------------------------

/** Keys the engine writes without a pointer and leaves out at zero (Go omitempty on a plain number). */
const ZERO_IS_ABSENT: ReadonlySet<string> = new Set(["size", "lineHeight", "listLineHeight", "weight", "width", "height", "paddingY", "maxWidth", "stackBelow", "after"]);

/**
 * A value in the form two copies of the same email share: keys sorted, and
 * the values the engine leaves out ("", false, null, an empty list, and a zero in
 * the keys above) left out. A footer's separator is a pointer there, so an empty
 * one is a choice and stays.
 */
function canonical(value: unknown, depth = 0): unknown {
  if (depth > 64) return value;
  if (Array.isArray(value)) return value.map(item => canonical(item, depth + 1));
  if (!isRecord(value)) return value;
  const out: Record<string, unknown> = {};
  for (const key of Object.keys(value).sort()) {
    const item = value[key];
    if (item === null || item === false || (item === "" && key !== "separator") || (Array.isArray(item) && item.length === 0) || (item === 0 && ZERO_IS_ABSENT.has(key))) continue;
    const next = canonical(item, depth + 1);
    Object.defineProperty(out, key, { value: next, writable: true, enumerable: true, configurable: true });
  }
  return out;
}

/** Whether `b` (the copy the server keeps, say) says the same as `a`, however its keys are ordered and whichever empty values it leaves out. */
export function sameEmail(a: EmailDocument, b: unknown): boolean {
  return isRecord(b) && JSON.stringify(canonical(a)) === JSON.stringify(canonical(b));
}

/** The merge tags a recipient fills (displayName, email, accountName, campaignName and fields.<key>); the link tags are not among them. */
const MERGE_TAG = /\{\{(displayName|email|accountName|campaignName|fields\.[^{}\r\n]{1,120})\}\}/g;
const MAX_SAMPLE_KEYS = 64;

/** The merge tags the email uses, sorted, at most 64: what "Preview with" asks sample values for. */
export function sampleKeys(doc: EmailDocument): string[] {
  const keys = new Set<string>();
  const pending: unknown[] = [doc];
  while (pending.length > 0) {
    const value = pending.pop();
    if (typeof value === "string") for (const match of value.matchAll(MERGE_TAG)) keys.add(match[1] as string);
    else if (Array.isArray(value)) for (const item of value) pending.push(item);
    else if (isRecord(value)) for (const item of Object.values(value)) pending.push(item);
  }
  return [...keys].sort().slice(0, MAX_SAMPLE_KEYS);
}

/** Keys a block carries that its type does not have, as `block "<id>": <key>`. The editor keeps them; Source shows them. */
export function unknownFields(doc: LayoutEmail): string[] {
  const unknown: string[] = [];
  walkBlocks(doc.blocks, block => {
    const known = fieldsOf(block.type);
    for (const key of Object.keys(block)) if (!known.includes(key)) unknown.push(`block "${block.id}": ${key}`);
  });
  return unknown;
}

// -- The Source schema --------------------------------------------------------

type Schema = Record<string, unknown>;
const ref = (name: string): Schema => ({ $ref: `#/definitions/${name}` });
const between = (minimum: number, maximum: number): Schema => ({ type: "number", minimum, maximum });
/** What a block of the type must carry for the engine to accept it. */
const REQUIRED_FIELDS: Readonly<Partial<Record<BlockType, readonly string[]>>> = { image: ["width"], button: ["label"], columns: ["columns"], social: ["links"] };

// A failed pattern is shown to the person as written, so each carries its sentence (patternErrorMessage, which VS Code's JSON support reads).
const idSchema = (): Schema => ({ type: "string", pattern: ID_PATTERN, patternErrorMessage: "Use lowercase letters, digits and hyphens.", description: "Lowercase letters, digits and hyphens. Every block in the email needs its own." });
const colourSchema = (): Schema => ({ enum: [...THEME_TOKENS], description: "One of the theme colours." });
const linkSchema = (): Schema => ({
  type: "string",
  maxLength: 2000,
  pattern: LINK_PATTERN,
  patternErrorMessage: SENTENCES.link,
  description: "A web address, an email address (mailto:), a phone number (tel:) or a link tag such as {{unsubscribeUrl}}.",
});
const pictureSchema = (): Schema => ({
  type: "string",
  pattern: PICTURE_PATTERN,
  patternErrorMessage: SENTENCES.picture,
  description: "A picture from Files (artifact:<id>) or an embedded PNG, JPEG or GIF. Leave it empty for a placeholder.",
});

/** The schema of one key of one block type. A key without an entry here fails the build, so the table cannot drift from it. */
function fieldSchema(type: BlockType, field: string): Schema {
  switch (field) {
    case "id": return idSchema();
    case "type": return { const: type };
    case "role": return { enum: ["brand", "content"], description: "Brand blocks are the same in every email made from the layout. Content blocks are the ones you write." };
    case "padding": case "inset": return ref("lengths");
    case "text": return type === "text" ? { type: "string", description: "Markdown: **bold**, *italic*, [label](address), - bullets and 1. numbers." } : { type: "string" };
    case "size": return between(8, 96);
    case "lineHeight": case "listLineHeight": case "gap": case "cellPadding": case "after": case "paddingY": return between(0, 200);
    case "color": case "textColor": case "linkColor": case "background": return colourSchema();
    case "align": return { enum: ["left", "center", "right"] };
    case "italic": case "bleed": return { type: "boolean" };
    case "weight": return { enum: [0, 400, 700] };
    case "font": return { enum: ["heading", "body", "base"] };
    case "letterSpacing": return { type: "string", pattern: SPACING_PATTERN, patternErrorMessage: "Enter a length in em, such as 0.05em or -0.02em.", description: "A length in em, such as 0.05em or -0.02em." };
    case "src": return pictureSchema();
    case "alt": case "title": return { type: "string" };
    case "width": return { type: "integer", minimum: type === "image" ? 1 : 0, maximum: 600 };
    case "height": case "maxWidth": case "stackBelow": return between(0, MAX_HEIGHT);
    case "href": return linkSchema();
    case "label": return { type: "string", minLength: 1, maxLength: 200 };
    case "blocks": return { type: "array", maxItems: 300, items: ref("block") };
    case "columns": return { type: "array", minItems: 1, maxItems: 4, items: ref("column") };
    case "links": return { type: "array", minItems: type === "social" ? 1 : 0, maxItems: 8, items: ref(type === "social" ? "socialLink" : "footerLink") };
    case "lines": return { type: "array", maxItems: 12, items: { type: "string", maxLength: 300 } };
    case "separator": return { type: "string", maxLength: 40 };
    case "html": return { type: "string", maxLength: 65536 };
    default: throw new Error(`The email schema has no definition for the block field "${field}"`);
  }
}

/** The branch of the block definition that applies when `type` is this block type. */
function blockBranch(type: BlockType): Schema {
  const required = REQUIRED_FIELDS[type];
  return {
    if: { properties: { type: { const: type } }, required: ["type"] },
    then: {
      ...(required ? { required: [...required] } : {}),
      properties: Object.fromEntries(BLOCK_FIELDS[type].map(field => [field, fieldSchema(type, field)])),
      additionalProperties: false,
    },
  };
}

/** The JSON schema (draft-07) that validates `*.email.json` in Source, generated from the field tables. */
export function emailSchema(): Record<string, unknown> {
  const subject: Schema = { type: "string", minLength: 1, maxLength: MAX_SUBJECT_BYTES, pattern: SUBJECT_PATTERN, patternErrorMessage: SENTENCES.subject, description: "One line." };
  const preheader: Schema = { type: "string", maxLength: MAX_PREHEADER, pattern: ONE_LINE_PATTERN, patternErrorMessage: SENTENCES.preheader, description: "The preview line after the subject: one line, up to 200 characters." };
  const themeColours = Object.fromEntries(THEME_COLOURS.map(name => [name, { type: "string", pattern: HEX_PATTERN, patternErrorMessage: SENTENCES.themeColour, description: "A colour as #rrggbb." }]));
  const font: Schema = { type: "string", pattern: FONT_PATTERN, patternErrorMessage: SENTENCES.font, description: "A font name using letters, digits, spaces or hyphens." };
  return {
    $schema: "http://json-schema.org/draft-07/schema#",
    title: "MemQL email",
    description: "An email is either a layout (blocks) or custom HTML (textBody and htmlBody).",
    oneOf: [ref("layoutEmail"), ref("customEmail")],
    definitions: {
      layoutEmail: {
        type: "object",
        additionalProperties: false,
        required: ["subject", "theme", "blocks"],
        properties: {
          subject,
          preheader,
          layout: { type: "string", description: "The layout this email was made from. Leave it as it is." },
          theme: ref("theme"),
          blocks: { type: "array", maxItems: 300, items: ref("block") },
        },
      },
      customEmail: {
        type: "object",
        additionalProperties: false,
        required: ["subject", "textBody", "htmlBody"],
        properties: { subject, preheader, textBody: { type: "string", minLength: 1, pattern: SOMETHING_PATTERN, patternErrorMessage: SENTENCES.plainText, description: "The plain-text version." }, htmlBody: { type: "string" } },
      },
      theme: {
        type: "object",
        additionalProperties: false,
        required: [...THEME_COLOURS, ...THEME_FONTS],
        properties: {
          ...themeColours,
          headingFont: font,
          bodyFont: font,
          webFonts: { type: "boolean" },
        },
      },
      block: {
        type: "object",
        required: ["id", "type"],
        properties: { id: idSchema(), type: { enum: [...BLOCK_TYPES] } },
        allOf: BLOCK_TYPES.map(blockBranch),
      },
      column: {
        type: "object",
        additionalProperties: false,
        required: ["width"],
        properties: {
          // The engine allows the columns of a row to add up to 100.01 percent, for rounding.
          width: { type: "number", exclusiveMinimum: 0, maximum: 100.01, description: "A percentage of the row." },
          cellPadding: between(0, 200),
          blocks: { type: "array", items: ref("block") },
        },
      },
      socialLink: {
        type: "object",
        additionalProperties: false,
        required: ["network"],
        properties: { network: { type: "string", minLength: 1, maxLength: 40 }, href: linkSchema(), icon: pictureSchema(), width: { type: "integer", minimum: 0, maximum: 600 } },
      },
      footerLink: {
        type: "object",
        additionalProperties: false,
        required: ["label"],
        properties: { label: { type: "string", minLength: 1, maxLength: 80 }, href: linkSchema() },
      },
      lengths: { type: "array", minItems: 4, maxItems: 4, items: between(0, 200), description: "Top, right, bottom and left, in pixels." },
    },
  };
}

// -- Writing an edit ----------------------------------------------------------

const isHighSurrogate = (unit: number) => unit >= 0xd800 && unit <= 0xdbff;
const isLowSurrogate = (unit: number) => unit >= 0xdc00 && unit <= 0xdfff;

/**
 * The one span of `before` to replace with `text` to get `after`: everything
 * between the longest shared start and the longest shared end. Applying it as a
 * single edit keeps undo, and a Source editor's cursor, where they were. The
 * span never cuts a character (a surrogate pair) in half.
 */
export function minimalEdit(before: string, after: string): { start: number; end: number; text: string } {
  const limit = Math.min(before.length, after.length);
  let start = 0;
  while (start < limit && before.charCodeAt(start) === after.charCodeAt(start)) start++;
  if (start > 0 && isHighSurrogate(before.charCodeAt(start - 1))) start--;
  let tail = 0;
  while (tail < limit - start && before.charCodeAt(before.length - 1 - tail) === after.charCodeAt(after.length - 1 - tail)) tail++;
  if (tail > 0 && isLowSurrogate(before.charCodeAt(before.length - tail))) tail--;
  return { start, end: before.length - tail, text: after.slice(start, after.length - tail) };
}
