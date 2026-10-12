import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, readFileSync, readdirSync, writeFileSync } from "node:fs";
import {
  BLOCK_FIELDS, BLOCK_TYPES, EDITABLE_FIELDS, LINK_TAGS, THEME_TOKENS,
  addedBlockId, blockLabel, canMove, editEmail, emailSchema, findBlock, isLayoutEmail, minimalEdit, readEmail, sameEmail, sampleKeys,
  unknownFields, validLink, walkBlocks, writeEmail,
  type EmailBlock, type EmailDocument, type EmailEdit, type LayoutEmail,
} from "../src/emailDocument.js";
import { UserInputError } from "../src/problems.js";

const theme = { page: "#eef0f3", canvas: "#ffffff", primary: "#1f5f8b", secondary: "#5b8fb9", accent: "#d9534f", tint: "#f6f4ef", text: "#2b2b2b", muted: "#8a8f98", headingFont: "Georgia", bodyFont: "Arial" };
const layout = (blocks: unknown[]) => JSON.stringify({ subject: "Hello", preheader: "Soon", layout: "builtin:article", theme, blocks }, null, 2) + "\n";

// The helpers below are not part of the brief's cases; they keep the extra cases short.
const read = (blocks: unknown[]) => readEmail(layout(blocks)) as LayoutEmail;
const refuses = (run: () => unknown, message: RegExp) => assert.throws(run, (error: unknown) => {
  assert.ok(error instanceof UserInputError, `expected a UserInputError, got ${String(error)}`);
  assert.match(error.message, message);
  return true;
});
const ids = (blocks: readonly EmailBlock[] | undefined) => (blocks ?? []).map(block => block.id);

test("an email is a layout or custom HTML, never both", () => {
  assert.ok(isLayoutEmail(readEmail(layout([{ id: "headline", type: "heading", text: "Hi" }]))));
  assert.ok(!isLayoutEmail(readEmail('{"subject":"Plain","textBody":"Hello","htmlBody":""}')));
  assert.throws(() => readEmail(`{"subject":"x","textBody":"y","htmlBody":"","blocks":[],"theme":${JSON.stringify(theme)}}`), /either a layout \(blocks\) or custom HTML/);
  assert.throws(() => readEmail('{"subject":"x"}'), /needs blocks \(a layout\) or textBody and htmlBody/);
  assert.throws(() => readEmail("{"), /invalid JSON\. Open Source/);
  assert.throws(() => readEmail('{"subject":"x","textBody":"y","htmlBody":"","accountId":"c"}'), /Remove "accountId" in Source/);
  assert.throws(() => readEmail(layout([{ id: "m", type: "marquee" }])), /doesn’t know: "marquee"/);
});

test("an inspector edit changes one span and keeps every other key", () => {
  const before = layout([{ id: "headline", type: "heading", text: "Hi", size: 48, custom: "kept" }, { id: "cta", type: "button", label: "Go", href: "" }]);
  const after = writeEmail(editEmail(readEmail(before), { kind: "field", block: "headline", field: "text", value: "Welcome aboard" }));
  const span = minimalEdit(before, after);
  assert.equal(before.slice(0, span.start) + span.text + before.slice(span.end), after);
  assert.equal(span.text, "Welcome aboard");
  assert.match(after, /"custom": "kept"/);
});

test("blocks move within their container and illegal moves are refused", () => {
  const doc = read([
    { id: "a", type: "heading", text: "A" },
    { id: "box", type: "section", blocks: [{ id: "b", type: "text", text: "B" }, { id: "c", type: "text", text: "C" }] },
    { id: "d", type: "button", label: "D", href: "" },
  ]);
  assert.equal(canMove(doc, "b", -1), false);
  assert.equal(canMove(doc, "b", 1), true);
  assert.equal(canMove(doc, "c", 1), false);
  assert.equal(canMove(doc, "a", -1), false);
  assert.equal(canMove(doc, "d", 1), false);
  assert.equal(canMove(doc, "d", -1), true);
  assert.equal(canMove(doc, "missing", 1), false);
  const up = editEmail(doc, { kind: "move", block: "c", by: -1 }) as LayoutEmail;
  assert.deepEqual(ids(up.blocks[1]?.blocks), ["c", "b"]);
  assert.deepEqual(ids(up.blocks), ["a", "box", "d"], "a block never leaves its container");
  const down = editEmail(doc, { kind: "move", block: "a", by: 1 }) as LayoutEmail;
  assert.deepEqual(ids(down.blocks), ["box", "a", "d"]);
  refuses(() => editEmail(doc, { kind: "move", block: "d", by: 1 }), /^This block can’t move further\.$/);
  refuses(() => editEmail(doc, { kind: "move", block: "b", by: -1 }), /can’t move further/);
  refuses(() => editEmail(doc, { kind: "move", block: "missing", by: 1 }), /^That block is no longer in the email\.$/);
  assert.deepEqual(ids(doc.blocks[1]?.blocks), ["b", "c"], "an edit returns a new document and leaves the old one alone");
});

test("add after makes a unique id with the type's defaults and remove takes children with it", () => {
  const doc = read([
    { id: "text-1", type: "text", text: "Hello" },
    { id: "box", type: "section", blocks: [{ id: "inner", type: "heading", text: "In" }] },
  ]);
  const addText = { kind: "add", after: "text-1", type: "text" } as const;
  assert.equal(addedBlockId(doc, addText), "text-2");
  const withText = editEmail(doc, addText) as LayoutEmail;
  assert.deepEqual(withText.blocks[1], { id: "text-2", type: "text", text: "" });
  assert.deepEqual(ids(withText.blocks), ["text-1", "text-2", "box"]);

  const addImage = { kind: "add", after: "inner", type: "image" } as const;
  assert.equal(addedBlockId(doc, addImage), "image-1");
  const withImage = editEmail(doc, addImage) as LayoutEmail;
  assert.deepEqual(withImage.blocks[1]?.blocks, [{ id: "inner", type: "heading", text: "In" }, { id: "image-1", type: "image", src: "", alt: "", width: 552 }]);

  // Every addable type, with its exact defaults and the smallest unused number.
  const defaults: Record<string, Record<string, unknown>> = {
    heading: { id: "heading-1", type: "heading", text: "" },
    text: { id: "text-2", type: "text", text: "" },
    highlight: { id: "highlight-1", type: "highlight", text: "" },
    button: { id: "button-1", type: "button", label: "Button", href: "" },
    image: { id: "image-1", type: "image", src: "", alt: "", width: 552 },
    divider: { id: "divider-1", type: "divider" },
    spacer: { id: "spacer-1", type: "spacer", height: 24 },
  };
  for (const [type, expected] of Object.entries(defaults)) {
    const edit = { kind: "add", after: "text-1", type } as Extract<EmailEdit, { kind: "add" }>;
    assert.equal(addedBlockId(doc, edit), expected.id, type);
    assert.deepEqual((editEmail(doc, edit) as LayoutEmail).blocks[1], expected, type);
  }
  // The smallest unused number is taken, not the next after the largest.
  const gaps = read([{ id: "spacer-2", type: "spacer", height: 4 }, { id: "spacer-3", type: "spacer", height: 4 }]);
  assert.equal(addedBlockId(gaps, { kind: "add", after: "spacer-2", type: "spacer" }), "spacer-1");
  const afterOne = editEmail(gaps, { kind: "add", after: "spacer-2", type: "spacer" }) as LayoutEmail;
  assert.equal(addedBlockId(afterOne, { kind: "add", after: "spacer-2", type: "spacer" }), "spacer-4");
  // An id used by a block of another type is taken too: ids are unique across the email.
  const taken = read([{ id: "image-1", type: "heading", text: "Not a picture" }]);
  assert.equal(addedBlockId(taken, { kind: "add", after: "image-1", type: "image" }), "image-2");
  // A block can be added after a column's block, and after a section itself (a sibling of the section).
  const columns = read([{ id: "pair", type: "columns", columns: [{ width: 50, blocks: [{ id: "left", type: "text", text: "L" }] }, { width: 50, blocks: [] }] }]);
  const inColumn = editEmail(columns, { kind: "add", after: "left", type: "divider" }) as LayoutEmail;
  assert.deepEqual(ids((inColumn.blocks[0]?.columns as { blocks: EmailBlock[] }[])[0]?.blocks), ["left", "divider-1"]);
  const afterSection = editEmail(doc, { kind: "add", after: "box", type: "divider" }) as LayoutEmail;
  assert.deepEqual(ids(afterSection.blocks), ["text-1", "box", "divider-1"]);

  const removed = editEmail(withImage, { kind: "remove", block: "box" }) as LayoutEmail;
  assert.deepEqual(ids(removed.blocks), ["text-1"]);
  assert.equal(findBlock(removed, "inner"), undefined);
  assert.equal(findBlock(removed, "image-1"), undefined);
  assert.deepEqual(ids((editEmail(withImage, { kind: "remove", block: "image-1" }) as LayoutEmail).blocks[1]?.blocks), ["inner"]);
  refuses(() => editEmail(doc, { kind: "remove", block: "missing" }), /^That block is no longer in the email\.$/);
  refuses(() => editEmail(doc, { kind: "add", after: "missing", type: "text" }), /^That block is no longer in the email\.$/);
  assert.throws(() => editEmail(doc, { kind: "add", after: "text-1", type: "footer" as never }), /footer/);
});

test("colours must be theme colours and links must be safe", () => {
  const doc = read([{ id: "headline", type: "heading", text: "Hi" }, { id: "cta", type: "button", label: "Go", href: "" }]);
  const colour = (value: string) => editEmail(doc, { kind: "field", block: "headline", field: "color", value });
  refuses(() => colour("#ff0000"), /^Choose one of the theme colours\.$/);
  refuses(() => colour("red"), /^Choose one of the theme colours\.$/);
  for (const token of THEME_TOKENS) assert.equal(((colour(token) as LayoutEmail).blocks[0] as EmailBlock).color, token);
  assert.ok(!("color" in ((colour("") as LayoutEmail).blocks[0] as EmailBlock)), "an empty colour is the layout's own colour");

  const link = (value: string) => editEmail(doc, { kind: "field", block: "cta", field: "href", value });
  const sentence = /^Enter a web address, an email address \(mailto:\), a phone number \(tel:\) or a link tag such as \{\{unsubscribeUrl\}\}\.$/;
  for (const bad of ["javascript:alert(1)", "{{fields.url}}", "https://a b", "https://{{fields.host}}/", "http://", "data:text/html,x"]) refuses(() => link(bad), sentence);
  for (const good of ["", "https://example.com/a?b=1", "mailto:hello@example.com", "tel:+15551234", "{{confirmUrl}}", "https://example.com/?e={{email}}"]) {
    assert.doesNotThrow(() => link(good), good);
  }
  assert.equal(((link("https://example.com") as LayoutEmail).blocks[1] as EmailBlock).href, "https://example.com");
  assert.ok(!("href" in ((link("") as LayoutEmail).blocks[1] as EmailBlock)), "an empty link deletes its key");

  refuses(() => editEmail(doc, { kind: "theme", key: "primary", value: "red" }), /^Enter a colour as #rrggbb\.$/);
  refuses(() => editEmail(doc, { kind: "theme", key: "page", value: "#12345" }), /#rrggbb/);
  refuses(() => editEmail(doc, { kind: "theme", key: "headingFont", value: "1x" }), /^Enter a font name using letters, digits, spaces or hyphens\.$/);
  refuses(() => editEmail(doc, { kind: "theme", key: "bodyFont", value: "Arial; evil" }), /font name/);
  const themed = editEmail(editEmail(doc, { kind: "theme", key: "primary", value: "#00AAff" }), { kind: "theme", key: "bodyFont", value: "Open Sans-Light" }) as LayoutEmail;
  assert.equal(themed.theme.primary, "#00AAff");
  assert.equal(themed.theme.bodyFont, "Open Sans-Light");
});

test("the role switch sets brand, and content only where it would otherwise inherit brand", () => {
  const doc = read([
    { id: "plain", type: "heading", text: "Top" },
    { id: "band", type: "section", role: "brand", blocks: [{ id: "inside", type: "text", text: "Inside" }, { id: "own", type: "text", text: "Own", role: "content" }] },
    { id: "tinted", type: "section", blocks: [{ id: "in-content", type: "text", text: "x" }] },
    { id: "pair", type: "columns", role: "brand", columns: [{ width: 100, blocks: [{ id: "in-column", type: "text", text: "y" }] }] },
  ]);
  const role = (source: LayoutEmail, block: string, brand: boolean) => editEmail(source, { kind: "role", block, brand }) as LayoutEmail;
  const roleOf = (source: LayoutEmail, block: string) => findBlock(source, block)?.block.role;

  // The effective role is the block's own, else its container's, else content.
  assert.equal(findBlock(doc, "plain")?.brand, false);
  assert.equal(findBlock(doc, "band")?.brand, true);
  assert.equal(findBlock(doc, "inside")?.brand, true);
  assert.equal(findBlock(doc, "own")?.brand, false);
  assert.equal(findBlock(doc, "in-content")?.brand, false);
  assert.equal(findBlock(doc, "in-column")?.brand, true);

  // At the top level and in a content container, off simply removes the key.
  const brandTop = role(doc, "plain", true);
  assert.equal(roleOf(brandTop, "plain"), "brand");
  const contentTop = role(brandTop, "plain", false);
  assert.ok(!("role" in (findBlock(contentTop, "plain")?.block ?? {})));
  assert.ok(!("role" in (findBlock(role(doc, "in-content", false), "in-content")?.block ?? {})));
  assert.equal(roleOf(role(doc, "in-content", true), "in-content"), "brand");
  // The top-level block that said content explicitly loses the key as well.
  const explicitTop = read([{ id: "x", type: "heading", text: "x", role: "content" }]);
  assert.ok(!("role" in (findBlock(role(explicitTop, "x", false), "x")?.block ?? {})));

  // Inside a brand container, off has to say content, and on says brand.
  assert.equal(roleOf(role(doc, "inside", false), "inside"), "content");
  assert.equal(roleOf(role(doc, "inside", true), "inside"), "brand");
  assert.equal(roleOf(role(doc, "own", true), "own"), "brand");
  assert.equal(roleOf(role(doc, "own", false), "own"), "content");
  assert.equal(roleOf(role(doc, "in-column", false), "in-column"), "content");
  // A brand container switched off at the top level loses the key, and its children follow.
  const unbranded = role(doc, "band", false);
  assert.ok(!("role" in (findBlock(unbranded, "band")?.block ?? {})));
  assert.equal(findBlock(unbranded, "inside")?.brand, false);
  refuses(() => role(doc, "missing", true), /^That block is no longer in the email\.$/);
});

test("a published document compares equal despite key order and omitted empty values", () => {
  const file = readEmail(layout([
    { id: "headline", type: "heading", text: "Hi", size: 0, italic: false },
    { id: "cta", type: "button", label: "Go", href: "", width: 300 },
    { id: "fine", type: "footer", lines: [], links: [{ label: "Unsubscribe", href: "{{unsubscribeUrl}}" }] },
  ]));
  const stored = {
    blocks: [
      { type: "heading", text: "Hi", id: "headline" },
      { width: 300, label: "Go", type: "button", id: "cta" },
      { links: [{ href: "{{unsubscribeUrl}}", label: "Unsubscribe" }], type: "footer", id: "fine" },
    ],
    theme: { ...theme, webFonts: false },
    layout: "builtin:article",
    preheader: "Soon",
    subject: "Hello",
  };
  assert.ok(sameEmail(file, stored));
  assert.ok(sameEmail(file, JSON.parse(JSON.stringify(stored))));
  assert.ok(!sameEmail(file, { ...stored, subject: "Changed" }));
  assert.ok(!sameEmail(editEmail(file, { kind: "subject", value: "Changed" }), stored));
  assert.ok(!sameEmail(file, { ...stored, blocks: stored.blocks.slice().reverse() }), "block order is content");
  assert.ok(!sameEmail(file, { ...stored, blocks: [{ ...stored.blocks[0], text: "Other" }, ...stored.blocks.slice(1)] }));
  assert.ok(!sameEmail(file, { ...stored, theme: { ...stored.theme, primary: "#000000" } }));
  assert.ok(!sameEmail(file, { ...stored, extra: 1 }));
  assert.ok(!sameEmail(file, null) && !sameEmail(file, "text") && !sameEmail(file, [stored]));
  // Zero is only "omitted" for the keys the engine writes without a pointer: a gap of 0 means no gap, not the default.
  const gapped = readEmail(layout([{ id: "pair", type: "columns", gap: 0, columns: [{ width: 100, blocks: [] }] }]));
  const ungapped = { subject: "Hello", preheader: "Soon", layout: "builtin:article", theme, blocks: [{ id: "pair", type: "columns", columns: [{ width: 100, blocks: [] }] }] };
  assert.ok(!sameEmail(gapped, ungapped));
  assert.ok(!sameEmail(readEmail(layout([{ id: "f", type: "footer", separator: "" }])), { subject: "Hello", preheader: "Soon", layout: "builtin:article", theme, blocks: [{ id: "f", type: "footer" }] }),
    "an empty separator is not the same as the default separator");
  // A custom email: the engine always writes htmlBody, even when it is empty.
  assert.ok(sameEmail(readEmail('{"subject":"Plain","textBody":"Hello","htmlBody":""}'), { subject: "Plain", textBody: "Hello", htmlBody: "" }));
  assert.ok(sameEmail(readEmail('{"subject":"Plain","textBody":"Hello","htmlBody":""}'), { subject: "Plain", textBody: "Hello" }));
  assert.ok(!sameEmail(readEmail('{"subject":"Plain","textBody":"Hello","htmlBody":""}'), { subject: "Plain", textBody: "Hello", htmlBody: "<p>Hello</p>" }));
});

test("Preview with lists the merge tags the email uses, never link tags", () => {
  const doc = read([{ id: "intro", type: "text", text: "Hi {{displayName}} at {{fields.company}} [stop]({{unsubscribeUrl}})" }]);
  assert.deepEqual(sampleKeys(editEmail(doc, { kind: "subject", value: "For {{accountName}}" })), ["accountName", "displayName", "fields.company"]);
  assert.deepEqual(sampleKeys(doc), ["displayName", "fields.company"]);
  assert.deepEqual(sampleKeys(readEmail(layout([{ id: "h", type: "heading", text: "Plain words" }]))), []);

  // Only the tags a recipient fills count: not the link tags, not unknown names, not a malformed tag.
  const links = read([
    { id: "a", type: "button", label: "Go", href: "{{confirmUrl}}" },
    { id: "b", type: "footer", links: [{ label: "Prefs", href: "{{preferencesUrl}}" }, { label: "View", href: "{{viewUrl}}?e={{email}}" }], lines: ["{{campaignName}}", "{{ displayName }}", "{{unknown}}", "{{fields.}}", "{{fields.a{b}}"] },
  ]);
  assert.deepEqual(sampleKeys(links), ["campaignName", "email"]);
  // Tags are found wherever text can be: alt text, titles and html blocks too.
  const wide = read([
    { id: "pic", type: "image", width: 100, alt: "Photo of {{fields.city}}" },
    { id: "follow", type: "social", title: "Follow {{accountName}}", links: [{ network: "x", href: "https://example.com/?u={{fields.handle}}" }] },
    { id: "raw", type: "html", html: "<p>{{fields.Company Name}} and {{fields.Company Name}}</p>" },
  ]);
  assert.deepEqual(sampleKeys(wide), ["accountName", "fields.Company Name", "fields.city", "fields.handle"]);

  const custom = readEmail(JSON.stringify({ subject: "For {{campaignName}}", preheader: "Hello {{displayName}}", textBody: "Write to {{email}} or {{fields.team}}", htmlBody: '<p>{{accountName}}</p><a href="{{unsubscribeUrl}}">stop</a>' }));
  assert.deepEqual(sampleKeys(custom), ["accountName", "campaignName", "displayName", "email", "fields.team"]);

  // At most 64 keys, in order.
  const many = read([{ id: "t", type: "text", text: Array.from({ length: 100 }, (_, n) => `{{fields.k${String(n).padStart(3, "0")}}}`).join(" ") }]);
  const keys = sampleKeys(many);
  assert.equal(keys.length, 64);
  assert.deepEqual(keys, [...keys].sort());
  assert.equal(keys[0], "fields.k000");
});

test("blocks are named by their words", () => {
  assert.equal(blockLabel({ id: "headline", type: "heading", text: "Thanks for joining us" }), "Heading · Thanks for joining us");
  assert.equal(blockLabel({ id: "headline", type: "heading", text: "" }), "Heading");
  assert.equal(blockLabel({ id: "headline", type: "heading" }), "Heading");
  // A text block is Markdown: the markers go, and the words are cut at 40 characters.
  assert.equal(blockLabel({ id: "t", type: "text", text: "**Bold** and *soft* and ***both***" }), "Text · Bold and soft and both");
  assert.equal(blockLabel({ id: "t", type: "text", text: "Read [our story](https://example.com/story) today" }), "Text · Read our story today");
  assert.equal(blockLabel({ id: "t", type: "text", text: "Intro\n\n- one\n- two\n\n1. first\n2. second" }), "Text · Intro one two first second");
  assert.equal(blockLabel({ id: "t", type: "text", text: "Stars \\*stay\\* and a * b stay" }), "Text · Stars *stay* and a * b stay");
  const long = "Welcome to **the** club, [read more](https://example.com/a) about everything we do";
  assert.equal(blockLabel({ id: "t", type: "text", text: long }), "Text · Welcome to the club, read more about eve…");
  assert.equal(blockLabel({ id: "t", type: "text", text: "x".repeat(40) }), `Text · ${"x".repeat(40)}`, "forty characters fit");
  assert.equal(blockLabel({ id: "t", type: "text", text: "x".repeat(41) }), `Text · ${"x".repeat(40)}…`);
  assert.equal(blockLabel({ id: "t", type: "text", text: "😀".repeat(41) }), `Text · ${"😀".repeat(40)}…`, "characters, not UTF-16 units");
  // Other blocks use the words they show.
  assert.equal(blockLabel({ id: "logo", type: "image", alt: "Logo" }), "Picture · Logo");
  assert.equal(blockLabel({ id: "logo", type: "image", alt: "" }), "Picture");
  assert.equal(blockLabel({ id: "logo", type: "image" }), "Picture");
  assert.equal(blockLabel({ id: "gap", type: "spacer", height: 24 }), "Space");
  assert.equal(blockLabel({ id: "cta", type: "button", label: "Ask our team" }), "Button · Ask our team");
  assert.equal(blockLabel({ id: "note", type: "highlight", text: "Make one decision\nthe night before." }), "Highlight · Make one decision the night before.");
  assert.equal(blockLabel({ id: "follow", type: "social", title: "Follow us" }), "Social links · Follow us");
  assert.equal(blockLabel({ id: "follow", type: "social" }), "Social links");
  assert.deepEqual(
    (["divider", "section", "columns", "footer", "html"] as const).map(type => blockLabel({ id: "x", type })),
    ["Divider", "Section", "Columns", "Footer", "HTML"]);
  assert.equal(blockLabel({ id: "x", type: "heading", text: 42 }), "Heading", "a value that is not text names nothing");
  // Naming a block reads a bounded part of its text, so an enormous or hostile block cannot stall the outline.
  const started = Date.now();
  assert.equal(blockLabel({ id: "t", type: "text", text: "[".repeat(1_000_000) }), `Text · ${"[".repeat(40)}…`);
  assert.equal(blockLabel({ id: "t", type: "text", text: "* ".repeat(500_000) }).startsWith("Text · * * * "), true);
  assert.ok(Date.now() - started < 2000, "a block of a million characters is named at once");
});

test("the JSON schema in media/ is generated from the field table", () => {
  const generated = JSON.stringify(emailSchema(), null, 2) + "\n";
  if (process.env.MEMQL_UPDATE_SCHEMA === "1") writeFileSync("media/email.schema.json", generated);
  assert.equal(readFileSync("media/email.schema.json", "utf8"), generated, "run MEMQL_UPDATE_SCHEMA=1 npm test and review the diff");
});

test("the built-in layouts use only fields this editor knows", { skip: existsSync("../../component/compose/emaildoc/layouts") ? false : "the built-in layouts (engine layouts plan, Task 4) are not merged into this branch yet" }, () => {
  const dir = "../../component/compose/emaildoc/layouts";
  const files = readdirSync(dir).filter(name => name.endsWith(".email.json"));
  assert.equal(files.length, 4);
  for (const name of files) {
    const doc = readEmail(readFileSync(`${dir}/${name}`, "utf8"));
    assert.ok(isLayoutEmail(doc), name);
    assert.deepEqual(unknownFields(doc as LayoutEmail), [], name);
  }
});

// ---------------------------------------------------------------------------
// Beyond the brief's cases: the engine's limits and sentences, the field tables
// against the engine's source, the links, the schema's behavior and the edits.
// ---------------------------------------------------------------------------

test("a refused email says which rule it broke, in the sentences of the table", () => {
  const custom = (extra: Record<string, unknown>) => JSON.stringify({ subject: "x", textBody: "y", htmlBody: "", ...extra });
  refuses(() => readEmail(custom({ subject: "a".repeat(2 * 1024 * 1024) })), /^Emails must be 2 MiB or smaller\.$/);
  refuses(() => readEmail("é".repeat(1024 * 1024 + 1)), /^Emails must be 2 MiB or smaller\.$/);
  refuses(() => readEmail(""), /^This email has invalid JSON\. Open Source and correct its formatting\.$/);
  refuses(() => readEmail('{"subject":"x","textBody":"y","htmlBody":""} {}'), /invalid JSON/);
  for (const text of ["[]", "null", "42", '"text"', "true"]) refuses(() => readEmail(text), /^An email must be a JSON object\.$/);
  refuses(() => readEmail(`{"subject":"x","blocks":[],"theme":${JSON.stringify(theme)},"htmlBody":""}`), /^An email is either a layout \(blocks\) or custom HTML \(textBody and htmlBody\), not both\.$/);
  refuses(() => readEmail('{"subject":"x","htmlBody":""}'), /^The email needs textBody and htmlBody strings\.$/);
  refuses(() => readEmail("{}"), /^An email needs blocks \(a layout\) or textBody and htmlBody \(custom HTML\)\.$/);
  refuses(() => readEmail(custom({ accountId: "c" })), /^Remove "accountId" in Source\. Emails don’t have that field\.$/);
  refuses(() => readEmail(custom({ layout: "builtin:letter" })), /^Remove "layout" in Source\./);
  refuses(() => readEmail(custom({ zebra: 1, apple: 2 })), /^Remove "apple" in Source\./);
  refuses(() => readEmail(layout([]).replace('"subject"', '"theme2": 1, "subject"')), /^Remove "theme2" in Source\./);
  refuses(() => readEmail(custom({ ["k".repeat(200)]: 1 })), /^Remove "k{60}\.\.\." in Source\./);

  for (const subject of ["", "  ", " ", "a\nb", "a\u0000b", 5, null]) refuses(() => readEmail(custom({ subject })), /^Enter a subject on one line\.$/);
  refuses(() => readEmail(JSON.stringify({ textBody: "y", htmlBody: "" })), /^Enter a subject on one line\.$/);
  refuses(() => readEmail(custom({ subject: "é".repeat(500) })), /^The subject must be under 999 bytes\.$/);
  assert.doesNotThrow(() => readEmail(custom({ subject: "a".repeat(998) })));
  assert.doesNotThrow(() => readEmail(custom({ subject: "é".repeat(499) })));
  assert.doesNotThrow(() => readEmail(custom({ subject: "﻿" })), "the engine trims only Unicode spaces, and a byte order mark is none");
  refuses(() => readEmail(custom({ subject: "a".repeat(999) })), /under 999 bytes/);

  for (const preheader of ["a\nb", 5, "p".repeat(201), "😀".repeat(201)]) refuses(() => readEmail(custom({ preheader })), /^The preheader must be one line of 200 characters or fewer\.$/);
  assert.doesNotThrow(() => readEmail(custom({ preheader: "p".repeat(200) })));
  assert.doesNotThrow(() => readEmail(custom({ preheader: "😀".repeat(200) })));

  refuses(() => readEmail('{"subject":"x","textBody":"y"}'), /^The email needs textBody and htmlBody strings\.$/);
  refuses(() => readEmail('{"subject":"x","textBody":5,"htmlBody":""}'), /^The email needs textBody and htmlBody strings\.$/);
  refuses(() => readEmail('{"subject":"x","textBody":"y","htmlBody":null}'), /^The email needs textBody and htmlBody strings\.$/);
  for (const textBody of ["", "  \n", " "]) refuses(() => readEmail(custom({ textBody })), /^Include a plain-text version\.$/);

  const shape = /^A layout email needs a theme and a list of blocks\.$/;
  refuses(() => readEmail(JSON.stringify({ subject: "x", blocks: [] })), shape);
  refuses(() => readEmail(JSON.stringify({ subject: "x", theme, blocks: null })), shape);
  refuses(() => readEmail(JSON.stringify({ subject: "x", theme, blocks: {} })), shape);
  refuses(() => readEmail(JSON.stringify({ subject: "x", theme: null, blocks: [] })), shape);
  refuses(() => readEmail(JSON.stringify({ subject: "x", theme: [], blocks: [] })), shape);
  refuses(() => readEmail(JSON.stringify({ subject: "x", theme: "dark", blocks: [] })), shape);
  refuses(() => readEmail(layout([{ id: "s", type: "section", blocks: "no" }])), shape);
  refuses(() => readEmail(layout([{ id: "c", type: "columns", columns: "no" }])), shape);
  refuses(() => readEmail(layout([{ id: "c", type: "columns", columns: ["no"] }])), shape);
  refuses(() => readEmail(layout([{ id: "c", type: "columns", columns: [{ width: 50, blocks: 7 }] }])), shape);
  assert.doesNotThrow(() => readEmail(layout([])), "an empty layout is still a layout");
  assert.doesNotThrow(() => readEmail(layout([{ id: "c", type: "columns", columns: [{ width: 50 }] }])), "a column without blocks has none");
  assert.doesNotThrow(() => readEmail(layout([{ id: "s", type: "section" }])));

  const needs = /^Every block needs an id and a type\.$/;
  for (const block of [null, "text", 7, [], {}, { id: "x" }, { type: "heading" }, { id: "", type: "heading" }, { id: 5, type: "heading" }, { id: "x", type: 5 }, { id: "x", type: "" }]) {
    refuses(() => readEmail(layout([block])), needs);
  }
  refuses(() => readEmail(layout([{ id: "s", type: "section", blocks: [{ id: "x" }] }])), needs);
  refuses(() => readEmail(layout([{ id: "c", type: "columns", columns: [{ width: 50, blocks: [{ type: "text" }] }] }])), needs);
  refuses(() => readEmail(layout([{ id: "m", type: "marquee" }])), /^Block "m" has a type MemQL doesn’t know: "marquee"\. Fix it in Source\.$/);
  refuses(() => readEmail(layout([{ id: "s", type: "section", blocks: [{ id: "deep", type: "carousel" }] }])), /^Block "deep" has a type MemQL doesn’t know: "carousel"\./);
  refuses(() => readEmail(layout([{ id: "x", type: "Heading" }])), /doesn’t know: "Heading"/);
  refuses(() => readEmail(layout([{ id: "x", type: "toString" }])), /doesn’t know: "toString"/);

  // Everything the table does not name stays untouched: unknown block keys, odd values, the layout id.
  const loose = layout([{ id: "x", type: "heading", text: 5, bogus: { nested: [1, 2] }, color: "#ff0000" }]).replace('"builtin:article"', "7");
  assert.equal(writeEmail(readEmail(loose)), loose);
});

test("an email that nests too deeply is refused rather than crashing the editor", () => {
  const depth = 20000;
  const deep = '{"subject":"x","theme":' + JSON.stringify(theme) + ',"blocks":[' + '{"id":"s","type":"section","blocks":['.repeat(depth) + "]}".repeat(depth) + "]}";
  assert.throws(() => readEmail(deep), (error: unknown) => error instanceof UserInputError);
});

test("every refusal is a UserInputError, so the host shows it as written", () => {
  const sources = ["{", "[]", "{}", '{"subject":"x"}', "{\"subject\":\"x\",\"textBody\":\"y\",\"htmlBody\":\"\",\"z\":1}", layout([{ id: "m", type: "marquee" }])];
  for (const source of sources) assert.throws(() => readEmail(source), (error: unknown) => error instanceof UserInputError);
  assert.equal(new UserInputError("x") instanceof Error, true);
});

test("the document model imports nothing but the user error, so view bundles stay free of the SDK", () => {
  const imports = (file: string) => [...readFileSync(file, "utf8").matchAll(/^\s*(?:import|export)\b[^;\n]*\bfrom\s+"([^"]+)"/gm)].map(match => match[1]);
  assert.deepEqual(imports("src/emailDocument.ts"), ["./userError.js"]);
  assert.deepEqual(imports("src/userError.ts"), []);
  assert.match(readFileSync("src/problems.ts", "utf8"), /^export \{ UserInputError \} from "\.\/userError\.js";$/m);
});

test("the field tables mirror the engine's parser", () => {
  const dir = "../../component/compose/emaildoc";
  const parse = readFileSync(`${dir}/parse.go`, "utf8");
  const doc = readFileSync(`${dir}/doc.go`, "utf8");
  const quoted = (text: string) => [...text.matchAll(/"([^"]+)"/g)].map(match => match[1] as string);
  const words = (text: string) => text.split(/\s+/).filter(Boolean);

  assert.deepEqual([...THEME_TOKENS], quoted(/colourTokens = \[\]string\{([^}]*)\}/.exec(parse)?.[1] ?? ""));
  assert.deepEqual([...LINK_TAGS], [...doc.matchAll(/^\s*Tag\w+URL\s*=\s*"([^"]+)"/gm)].map(match => match[1]));

  const own = Object.fromEntries([...(/own := map\[string\]string\{([^}]*)\}/.exec(parse)?.[1] ?? "").matchAll(/"(\w+)":\s*"([^"]*)"/g)].map(match => [match[1], words(match[2] as string)]));
  assert.deepEqual([...BLOCK_TYPES].sort(), Object.keys(own).sort());
  const order = [...(/type Block struct \{([^}]*)\}/.exec(doc)?.[1] ?? "").matchAll(/`json:"(\w+)(?:,omitempty)?"`/g)].map(match => match[1] as string);
  assert.ok(order.length > 30, "the Block struct was found");
  for (const type of BLOCK_TYPES) {
    const allowed = new Set(["id", "type", "role", ...(own[type] ?? []), ...(type === "spacer" ? [] : ["padding"])]);
    assert.deepEqual([...BLOCK_FIELDS[type]], order.filter(key => allowed.has(key)), `${type}: the keys a block may carry, in the order the engine writes them`);
    assert.ok(!(type === "spacer") || !BLOCK_FIELDS[type].includes("padding"), "a spacer has no padding");
    for (const field of EDITABLE_FIELDS[type]) assert.ok(BLOCK_FIELDS[type].includes(field), `${type} edits ${field}, which it may not carry`);
  }

  const keysOf = (name: string) => words(new RegExp(`${name}\\s*=\\s*wordSet\\("([^"]*)"\\)`).exec(parse)?.[1] ?? "");
  const customDoc = (extra: Record<string, unknown>) => JSON.stringify({ subject: "x", textBody: "y", htmlBody: "", ...extra });
  for (const key of keysOf("customKeys")) assert.doesNotThrow(() => readEmail(customDoc({ [key]: key === "preheader" ? "p" : key === "subject" ? "s" : "t" })), `custom ${key}`);
  assert.deepEqual(keysOf("customKeys").sort(), ["htmlBody", "preheader", "subject", "textBody"]);
  for (const key of keysOf("layoutKeys")) {
    const fields = { subject: "x", preheader: "p", layout: "builtin:letter", theme, blocks: [] };
    assert.ok(key in fields, `layout ${key}`);
    assert.doesNotThrow(() => readEmail(JSON.stringify(fields)), `layout ${key}`);
  }
  assert.deepEqual(keysOf("layoutKeys").sort(), ["blocks", "layout", "preheader", "subject", "theme"]);
  assert.deepEqual(keysOf("themeKeys").sort(), [...THEME_TOKENS.filter(token => token !== "white"), "bodyFont", "headingFont", "webFonts"].sort());
});

test("which fields the inspector edits, by block type", () => {
  assert.deepEqual(Object.keys(EDITABLE_FIELDS).sort(), [...BLOCK_TYPES].sort());
  assert.deepEqual(EDITABLE_FIELDS.heading, ["text", "color", "align"]);
  assert.deepEqual(EDITABLE_FIELDS.button, ["label", "href", "color", "align"]);
  assert.deepEqual(EDITABLE_FIELDS.image, ["src", "alt", "href", "align"]);
  assert.deepEqual(EDITABLE_FIELDS.spacer, ["height"]);
  assert.deepEqual(EDITABLE_FIELDS.html, ["html"]);
  // Sizes, spacing and padding stay in Source, and so does everything a layout decides.
  const sourceOnly = ["size", "lineHeight", "listLineHeight", "gap", "padding", "paddingY", "inset", "maxWidth", "cellPadding", "stackBelow", "width", "letterSpacing", "font", "weight", "italic", "bleed", "after", "separator", "textColor", "linkColor", "id", "type", "role"];
  for (const type of BLOCK_TYPES) for (const field of EDITABLE_FIELDS[type]) assert.ok(!sourceOnly.includes(field), `${type} ${field}`);
});

test("the document is walked in order, with depth, siblings, index and the effective role", () => {
  const doc = read([
    { id: "a", type: "heading", text: "A" },
    { id: "box", type: "section", role: "brand", blocks: [{ id: "b", type: "text", text: "B" }, { id: "c", type: "text", text: "C", role: "content" }] },
    { id: "pair", type: "columns", columns: [{ width: 50, blocks: [{ id: "d", type: "text", text: "D" }] }, { width: 50, blocks: [{ id: "e", type: "text", text: "E" }] }] },
    { id: "f", type: "footer", role: "brand" },
  ]);
  const seen: string[] = [];
  walkBlocks(doc.blocks, (block, depth, siblings, index, brand) => {
    assert.equal(siblings[index], block);
    seen.push(`${block.id}:${depth}:${index}:${brand ? "brand" : "content"}`);
  });
  assert.deepEqual(seen, ["a:0:0:content", "box:0:1:brand", "b:1:0:brand", "c:1:1:content", "pair:0:2:content", "d:1:0:content", "e:1:0:content", "f:0:3:brand"]);
  const found = findBlock(doc, "e");
  assert.equal(found?.depth, 1);
  assert.equal(found?.index, 0);
  assert.equal(found?.siblings, (doc.blocks[2]?.columns as { blocks: EmailBlock[] }[])[1]?.blocks);
  assert.equal(found?.block, found?.siblings[0]);
  assert.equal(findBlock(doc, "nope"), undefined);
  assert.deepEqual(unknownFields(doc), []);
});

test("a block that is not of a known type is handled, not looked up on Object.prototype", () => {
  const strange = { id: "x", type: "toString", text: "Words" } as unknown as EmailBlock;
  assert.equal(blockLabel(strange), "toString");
  const doc = { subject: "Hello", theme, blocks: [strange] } as unknown as LayoutEmail;
  assert.deepEqual(unknownFields(doc), ['block "x": id', 'block "x": type', 'block "x": text']);
  refuses(() => editEmail(doc, { kind: "field", block: "x", field: "text", value: "Changed" }), /^This block doesn’t have that field\.$/);
});

test("unknown fields are listed by block and kept", () => {
  const doc = read([
    { id: "headline", type: "heading", text: "Hi", custom: "kept", href: "https://example.com" },
    { id: "gap", type: "spacer", height: 4, padding: [0, 0, 0, 0] },
    { id: "box", type: "section", blocks: [{ id: "deep", type: "button", label: "Go", src: "x" }], text: "no" },
  ]);
  assert.deepEqual(unknownFields(doc), ['block "headline": custom', 'block "headline": href', 'block "gap": padding', 'block "box": text', 'block "deep": src']);
  assert.match(writeEmail(editEmail(doc, { kind: "field", block: "headline", field: "text", value: "Hello" })), /"custom": "kept"/);
});

test("an edit returns a new document and keeps every key it did not touch", () => {
  const source = layout([
    { id: "headline", type: "heading", text: "Hi", size: 48, custom: { nested: [1, 2, 3] }, role: "brand" },
    { id: "box", type: "section", future: true, blocks: [{ id: "intro", type: "text", text: "Body", extra: null }] },
  ]);
  const doc = readEmail(source);
  const before = JSON.stringify(doc);
  const next = editEmail(doc, { kind: "field", block: "intro", field: "text", value: "Changed" }) as LayoutEmail;
  assert.equal(JSON.stringify(doc), before, "the input is not changed");
  assert.notEqual(next, doc);
  assert.equal(writeEmail(editEmail(doc, { kind: "field", block: "intro", field: "text", value: "Body" })), source, "an edit that changes nothing writes the same bytes");
  assert.equal(writeEmail(next), source.replace('"text": "Body"', '"text": "Changed"'));
  // Edits never reorder keys, even on a document that is not in the engine's order.
  const odd = JSON.stringify({ blocks: [{ text: "Hi", type: "heading", id: "h" }], theme, subject: "Hello" }, null, 2) + "\n";
  const oddNext = writeEmail(editEmail(readEmail(odd), { kind: "field", block: "h", field: "text", value: "Hello" }));
  assert.equal(oddNext, odd.replace('"Hi"', '"Hello"'));
});

test("a new key is written where the engine writes it", () => {
  const doc = read([{ id: "headline", type: "heading", text: "Hi", size: 48 }, { id: "cta", type: "button", label: "Go" }]);
  const keys = (source: EmailDocument, block: string) => Object.keys((findBlock(source as LayoutEmail, block)?.block ?? {}));
  assert.deepEqual(keys(editEmail(doc, { kind: "role", block: "headline", brand: true }), "headline"), ["id", "type", "role", "text", "size"]);
  assert.deepEqual(keys(editEmail(doc, { kind: "field", block: "headline", field: "color", value: "primary" }), "headline"), ["id", "type", "text", "size", "color"]);
  assert.deepEqual(keys(editEmail(doc, { kind: "field", block: "headline", field: "align", value: "center" }), "headline"), ["id", "type", "text", "size", "align"]);
  // The engine writes a button's link before its label, so a link added here lands where the engine would put it.
  assert.deepEqual(keys(editEmail(doc, { kind: "field", block: "cta", field: "href", value: "https://example.com" }), "cta"), ["id", "type", "href", "label"]);
  const withColour = editEmail(doc, { kind: "field", block: "headline", field: "color", value: "primary" });
  assert.deepEqual(keys(editEmail(withColour, { kind: "role", block: "headline", brand: true }), "headline"), ["id", "type", "role", "text", "size", "color"]);
  // A key the engine does not know goes last, so the order of the known ones is the engine's.
  const odd = read([{ id: "h", type: "heading", text: "Hi", zzz: 1 }]);
  assert.deepEqual(keys(editEmail(odd, { kind: "field", block: "h", field: "color", value: "text" }), "h"), ["id", "type", "text", "color", "zzz"]);
  // The preheader sits right after the subject.
  const bare = readEmail(JSON.stringify({ subject: "Hello", theme, blocks: [] }));
  assert.deepEqual(Object.keys(editEmail(bare, { kind: "preheader", value: "Soon" })), ["subject", "preheader", "theme", "blocks"]);
  const plain = readEmail('{"subject":"Plain","textBody":"Hello","htmlBody":""}');
  assert.deepEqual(Object.keys(editEmail(plain, { kind: "preheader", value: "Soon" })), ["subject", "preheader", "textBody", "htmlBody"]);
});

test("subject and preheader edits follow the rules of the file", () => {
  const doc = read([{ id: "headline", type: "heading", text: "Hi" }]);
  assert.equal((editEmail(doc, { kind: "subject", value: "Welcome" }) as LayoutEmail).subject, "Welcome");
  refuses(() => editEmail(doc, { kind: "subject", value: "" }), /^Enter a subject on one line\.$/);
  refuses(() => editEmail(doc, { kind: "subject", value: "two\nlines" }), /^Enter a subject on one line\.$/);
  refuses(() => editEmail(doc, { kind: "subject", value: "é".repeat(500) }), /^The subject must be under 999 bytes\.$/);
  assert.equal((editEmail(doc, { kind: "preheader", value: "Later" }) as LayoutEmail).preheader, "Later");
  assert.ok(!("preheader" in editEmail(doc, { kind: "preheader", value: "" })), "an empty preheader deletes its key");
  refuses(() => editEmail(doc, { kind: "preheader", value: "p".repeat(201) }), /^The preheader must be one line of 200 characters or fewer\.$/);
  refuses(() => editEmail(doc, { kind: "preheader", value: "a\nb" }), /one line/);
  // A custom email has a subject and a preheader and nothing else to edit here.
  const plain = readEmail('{"subject":"Plain","textBody":"Hello","htmlBody":""}');
  assert.equal(editEmail(plain, { kind: "subject", value: "Changed" }).subject, "Changed");
  assert.equal(editEmail(plain, { kind: "preheader", value: "Soon" }).preheader, "Soon");
  const html = /^This email is written in HTML\. Change it in Source\.$/;
  refuses(() => editEmail(plain, { kind: "field", block: "x", field: "text", value: "y" }), html);
  refuses(() => editEmail(plain, { kind: "move", block: "x", by: 1 }), html);
  refuses(() => editEmail(plain, { kind: "add", after: "x", type: "text" }), html);
  refuses(() => editEmail(plain, { kind: "remove", block: "x" }), html);
  refuses(() => editEmail(plain, { kind: "role", block: "x", brand: true }), html);
  refuses(() => editEmail(plain, { kind: "theme", key: "page", value: "#ffffff" }), html);
});

test("a field edit keeps to the fields the block has and the values they take", () => {
  const doc = read([
    { id: "headline", type: "heading", text: "Hi" },
    { id: "cta", type: "button", label: "Go", href: "https://example.com" },
    { id: "logo", type: "image", src: "artifact:logo", alt: "Logo", width: 100, href: "https://example.com" },
    { id: "gap", type: "spacer", height: 24 },
    { id: "follow", type: "social", title: "Follow us", links: [{ network: "Example", href: "https://example.com/x" }] },
    { id: "fine", type: "footer", links: [{ label: "Unsubscribe", href: "{{unsubscribeUrl}}" }], lines: ["Acme"] },
    { id: "raw", type: "html", html: "<p>Hi</p>" },
  ]);
  const edit = (block: string, field: Extract<EmailEdit, { kind: "field" }>["field"], value: Extract<EmailEdit, { kind: "field" }>["value"]) =>
    findBlock(editEmail(doc, { kind: "field", block, field, value }) as LayoutEmail, block)?.block as EmailBlock;
  assert.equal(edit("cta", "label", "Start").label, "Start");
  assert.equal(edit("logo", "alt", "A different logo").alt, "A different logo");
  assert.ok(!("alt" in edit("logo", "alt", "")), "an empty description deletes its key");
  assert.ok(!("href" in edit("logo", "href", "")), "an empty link deletes its key");
  assert.equal(edit("logo", "src", "").src, "", "no picture is a placeholder, not a missing key");
  assert.equal(edit("logo", "src", "artifact:other").src, "artifact:other");
  assert.equal(edit("headline", "align", "right").align, "right");
  assert.ok(!("align" in edit("headline", "align", "")));
  assert.equal(edit("gap", "height", 48).height, 48);
  assert.equal(edit("gap", "height", "36").height, 36, "a number typed in a field arrives as text");
  assert.equal(edit("gap", "height", 0).height, 0);
  assert.equal(edit("headline", "text", "").text, "", "empty words stay as empty words");
  assert.ok(!("title" in edit("follow", "title", "")));
  assert.equal(edit("follow", "title", "Find us").title, "Find us");
  assert.equal(edit("raw", "html", "<p>New</p>").html, "<p>New</p>");
  assert.deepEqual(edit("fine", "lines", ["One", "Two"]).lines, ["One", "Two"]);
  assert.deepEqual(edit("fine", "lines", []).lines, []);
  assert.deepEqual(edit("fine", "links", [{ label: "Preferences", href: "{{preferencesUrl}}" }]).links, [{ label: "Preferences", href: "{{preferencesUrl}}" }]);
  assert.deepEqual(edit("follow", "links", [{ network: "Mail", href: "mailto:hello@example.com", icon: "artifact:mail" }]).links, [{ network: "Mail", href: "mailto:hello@example.com", icon: "artifact:mail" }]);
  refuses(() => edit("follow", "links", [{ network: "Bad", href: "javascript:alert(1)" }]), /^Enter a web address/);
  refuses(() => edit("fine", "links", [{ label: "Fine", href: "" }, { label: "Bad", href: "https://a b" }]), /^Enter a web address/);
  assert.doesNotThrow(() => edit("fine", "links", [{ label: "Later", href: "" }]));
  refuses(() => edit("gap", "height", -1), /^Enter a height between 0 and 2000 pixels\.$/);
  refuses(() => edit("gap", "height", 2001), /^Enter a height between 0 and 2000 pixels\.$/);
  refuses(() => edit("gap", "height", "tall"), /^Enter a height between 0 and 2000 pixels\.$/);
  refuses(() => edit("gap", "height", ""), /^Enter a height between 0 and 2000 pixels\.$/);
  refuses(() => edit("gap", "height", Number.NaN), /^Enter a height/);
  refuses(() => edit("headline", "align", "justify"), /^Choose left, center or right\.$/);
  refuses(() => edit("headline", "href", "https://example.com"), /^This block doesn’t have that field\.$/);
  refuses(() => edit("cta", "text", "Words"), /^This block doesn’t have that field\.$/);
  refuses(() => edit("gap", "color", "text"), /^This block doesn’t have that field\.$/);
  refuses(() => edit("missing", "text", "x"), /^That block is no longer in the email\.$/);
  // A value of the wrong kind is a mistake in the page, not something to tell the person.
  assert.throws(() => edit("headline", "text", 5 as never), (error: unknown) => error instanceof Error && !(error instanceof UserInputError));
  assert.throws(() => edit("fine", "lines", "one" as never), (error: unknown) => error instanceof Error && !(error instanceof UserInputError));
  assert.throws(() => edit("fine", "links", ["x"] as never), (error: unknown) => error instanceof Error && !(error instanceof UserInputError));
  assert.throws(() => editEmail(doc, { kind: "mystery" } as never), (error: unknown) => error instanceof Error && !(error instanceof UserInputError));
  assert.throws(() => editEmail(doc, { kind: "theme", key: "webFonts" as never, value: "true" }), (error: unknown) => error instanceof Error && !(error instanceof UserInputError));
});

test("a minimal edit is one span that rebuilds the text", () => {
  const cases: [string, string][] = [
    ["", ""], ["abc", "abc"], ["", "abc"], ["abc", ""], ["abc", "abxc"], ["abc", "ac"], ["abc", "xyz"], ["aaaa", "aaaaa"], ["aaaaa", "aaaa"],
    ["abcabc", "abcxabc"], ["ab", "aab"], ["aXa", "aXXa"], ["line one\nline two\n", "line one\nline 2\n"],
  ];
  for (const [before, after] of cases) {
    const span = minimalEdit(before, after);
    assert.equal(before.slice(0, span.start) + span.text + before.slice(span.end), after, `${before} -> ${after}`);
    assert.ok(span.start <= span.end && span.end <= before.length);
  }
  assert.deepEqual(minimalEdit("same", "same"), { start: 4, end: 4, text: "" });
  assert.deepEqual(minimalEdit("abc", "abxc"), { start: 2, end: 2, text: "x" });
  assert.deepEqual(minimalEdit("abc", "ac"), { start: 1, end: 2, text: "" });
  assert.deepEqual(minimalEdit("abc", "xyz"), { start: 0, end: 3, text: "xyz" });
  assert.deepEqual(minimalEdit("aaaa", "aaaaa"), { start: 4, end: 4, text: "a" }, "a repeated character is not counted twice");
  assert.deepEqual(minimalEdit('"text": "Hi",', '"text": "Welcome aboard",'), { start: 9, end: 11, text: "Welcome aboard" });
  // Two emoji that share their first UTF-16 unit: the span never cuts a character in half.
  const split = minimalEdit("a😀b", "a😁b");
  assert.deepEqual(split, { start: 1, end: 3, text: "😁" });
  assert.deepEqual(minimalEdit("😀", "😁"), { start: 0, end: 2, text: "😁" });
  assert.deepEqual(minimalEdit("x😀", "x🀀"), { start: 1, end: 3, text: "🀀" });
  // Random texts over a small alphabet, surrogate pairs included.
  let seed = 7;
  const next = (limit: number) => { seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0; return seed % limit; };
  const alphabet = ["a", "b", "\n", " ", "😀", "😁", "🀀", "é"];
  const text = () => Array.from({ length: next(9) }, () => alphabet[next(alphabet.length)]).join("");
  const isHigh = (unit: number) => unit >= 0xd800 && unit <= 0xdbff;
  const isLow = (unit: number) => unit >= 0xdc00 && unit <= 0xdfff;
  for (let n = 0; n < 4000; n++) {
    const before = text(), after = text();
    const span = minimalEdit(before, after);
    assert.equal(before.slice(0, span.start) + span.text + before.slice(span.end), after, JSON.stringify([before, after]));
    for (const [subject, at] of [[before, span.start], [before, span.end], [after, span.start], [after, span.start + span.text.length]] as const) {
      assert.ok(!(at > 0 && at < subject.length && isHigh(subject.charCodeAt(at - 1)) && isLow(subject.charCodeAt(at))), `cut a pair: ${JSON.stringify([before, after])}`);
    }
  }
});

test("a link is safe on the same terms as the engine's", () => {
  // Accepted and refused by the engine's own tests (parse_test.go: TestParseAcceptsLinkForms,
  // TestParseKeepsMergeTagsOutOfTheHost, TestParseRefusesCharactersAnAddressMustEncode and the link table).
  const accepted = [
    "", "https://example.com/a?b=1&c=2#top", "http://example.com", "HTTPS://EXAMPLE.COM/A", "mailto:hello@example.com?subject=Hi%20there", "tel:+15551234567",
    "https://example.com/?e={{email}}", ...LINK_TAGS, "https://example.com/" + "a".repeat(1980),
    "https://example.com?e={{email}}", "https://example.com/{{fields.slug}}", "https://example.com/a/{{fields.slug}}/b?x={{fields.y}}#{{fields.z}}",
    "https://example.com#{{fields.frag}}", "http://example.com:8080/?e={{email}}", "https://user@example.com/{{fields.p}}", "https://[::1]:8080/{{fields.p}}",
    "mailto:a@example.com?subject=Hello", "https://example.com/it's", "https:///", "https:////", "mailto:x", "tel:1",
  ];
  const refused = [
    "javascript:alert(1)", "{{fields.url}}", "https://a b", "https://example.com/a b", "https://example.com/a\nb", "https://example.com/" + "a".repeat(1981),
    "{{unsubscribeUrl}}?x=1", "x{{viewUrl}}", " {{viewUrl}}", "{{viewUrl}}{{confirmUrl}}", "{{unsubscribe}}", "https://", "mailto:", "tel:", "http://", "data:text/html;base64,AAAA", "ftp://x",
    "https://{{fields.host}}/x", "https://{{fields.url}}", "http://{{email}}", "https://example.com{{fields.p}}", "https://exa{{fields.part}}mple.com/", "https://example.com@{{fields.host}}/",
    "https://{{fields.user}}@example.com/", "https://user:{{fields.pw}}@example.com/", "https://example.com:{{fields.port}}/", "https://example.com:{{fields.port}}", "https://{{fields.host}}?x=1",
    "https://{{fields.host}}#x", "HTTPS://{{fields.host}}/", "https:///{{fields.host}}/x", "https:////{{fields.host}}/x", "mailto:{{email}}", "mailto:a@example.com?subject={{fields.subject}}",
    "mailto:{{unsubscribeUrl}}", "MAILTO:{{email}}", "tel:{{fields.phone}}", "tel:+1555{{fields.extension}}",
    'https://example.com/a"b', "https://example.com/a<b", "https://example.com/a>b", "https://example.com/a`b", "https://example.com/a\\b", 'mailto:a"b@example.com', 'tel:+1"5',
    "<https://example.com>", 'https://example.com/?q="x"', "https://example.com/ ", "https://example.com/ x", "https://example.com/\u0085", "https://example.com/\u007f", "https://example.com/\u0000",
  ];
  for (const href of accepted) assert.equal(validLink(href), true, JSON.stringify(href));
  for (const href of refused) assert.equal(validLink(href), false, JSON.stringify(href));
  assert.equal(validLink("https://example.com/" + "é".repeat(991)), false, "the limit is 2000 bytes, not 2000 characters");
  assert.equal(validLink("https://example.com/" + "é".repeat(990)), true);
  assert.equal(validLink("https://example.com/﻿x"), true, "a byte order mark is not a space to the engine");
  assert.equal(validLink(5 as never), false);
});

test("the schema's link pattern accepts exactly the links validLink does", () => {
  const schema = emailSchema() as { definitions: { footerLink: { properties: { href: { pattern?: string; maxLength?: number } } } } };
  const href = schema.definitions.footerLink.properties.href;
  const pattern = href.pattern ?? "";
  assert.ok(pattern, "a link in the schema has a pattern");
  assert.equal(href.maxLength, 2000);
  const patterns = [new RegExp(pattern), new RegExp(pattern, "u")];
  let seed = 11;
  const next = (limit: number) => { seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0; return seed % limit; };
  const pieces = ["http://", "https://", "HTTPS://", "Http://", "mailto:", "MAILTO:", "Tel:", "tel:", "javascript:", "data:", "ftp://", "//", "/", "?", "#", "@", ":", ".", "a", "example.com", "{{", "}}",
    "{{email}}", "{{fields.x}}", "{{unsubscribeUrl}}", "{{viewUrl}}", " ", "\t", "\n", " ", " ", "\u0085", "﻿", '"', "<", ">", "`", "\\", "'", "%20", "é", "ſ"];
  const exact = (href: string) => new TextEncoder().encode(href).length <= 2000 && validLink(href);
  let agreeing = 0;
  for (let n = 0; n < 40000; n++) {
    const href = Array.from({ length: next(7) }, () => pieces[next(pieces.length)]).join("");
    for (const expression of patterns) if (expression.test(href) !== exact(href)) assert.fail(`${JSON.stringify(href)}: the pattern says ${expression.test(href)}, validLink says ${validLink(href)}`);
    if (validLink(href)) agreeing++;
  }
  assert.ok(agreeing > 500, `the sample holds links of both kinds (${agreeing} accepted)`);
  for (const href of ["", ...LINK_TAGS, "https://example.com/a?b=1", "mailto:hello@example.com", "tel:+15551234", "HTTPS://{{fields.host}}/", "https:////{{fields.host}}/x", "https:///"]) {
    for (const expression of patterns) assert.equal(expression.test(href), validLink(href), href);
  }
});

// A small JSON Schema (draft-07) checker for the keywords the generated schema uses.
type Node = Record<string, unknown>;
function check(schema: Node, value: unknown, root: Node, at = "#"): string[] {
  if (typeof schema.$ref === "string") {
    const target = schema.$ref.slice(2).split("/").reduce<unknown>((node, key) => (node as Node)[key], root);
    return check(target as Node, value, root, at);
  }
  const errors: string[] = [];
  const fail = (message: string) => errors.push(`${at}: ${message}`);
  const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);
  const kind = (v: unknown) => Array.isArray(v) ? "array" : v === null ? "null" : typeof v;
  if ("const" in schema && !same(schema.const, value)) fail(`must be ${JSON.stringify(schema.const)}`);
  if (Array.isArray(schema.enum) && !schema.enum.some(option => same(option, value))) fail(`must be one of ${JSON.stringify(schema.enum)}`);
  if (typeof schema.type === "string") {
    const ok = schema.type === "integer" ? Number.isInteger(value) : schema.type === kind(value);
    if (!ok) { fail(`must be ${schema.type}`); return errors; }
  }
  if (typeof value === "string") {
    const length = Array.from(value).length;
    if (typeof schema.minLength === "number" && length < schema.minLength) fail("too short");
    if (typeof schema.maxLength === "number" && length > schema.maxLength) fail("too long");
    if (typeof schema.pattern === "string" && !new RegExp(schema.pattern, "u").test(value)) fail(`does not match ${schema.pattern}`);
  }
  if (typeof value === "number") {
    if (typeof schema.minimum === "number" && value < schema.minimum) fail("below the minimum");
    if (typeof schema.maximum === "number" && value > schema.maximum) fail("above the maximum");
    if (typeof schema.exclusiveMinimum === "number" && value <= schema.exclusiveMinimum) fail("not above the exclusive minimum");
  }
  if (Array.isArray(value)) {
    if (typeof schema.minItems === "number" && value.length < schema.minItems) fail("too few items");
    if (typeof schema.maxItems === "number" && value.length > schema.maxItems) fail("too many items");
    if (schema.items) value.forEach((item, index) => errors.push(...check(schema.items as Node, item, root, `${at}/${index}`)));
  }
  if (kind(value) === "object") {
    const object = value as Node, properties = (schema.properties ?? {}) as Record<string, Node>;
    for (const key of (schema.required ?? []) as string[]) if (!(key in object)) fail(`needs ${key}`);
    for (const [key, item] of Object.entries(object)) {
      if (properties[key]) errors.push(...check(properties[key], item, root, `${at}/${key}`));
      else if (schema.additionalProperties === false) fail(`has no property ${key}`);
    }
  }
  if (Array.isArray(schema.allOf)) for (const part of schema.allOf as Node[]) errors.push(...check(part, value, root, at));
  if (Array.isArray(schema.oneOf)) {
    const passing = (schema.oneOf as Node[]).filter(part => check(part, value, root, at).length === 0).length;
    if (passing !== 1) fail(`must match exactly one alternative (${passing} do)`);
  }
  if (schema.if && check(schema.if as Node, value, root, at).length === 0 && schema.then) errors.push(...check(schema.then as Node, value, root, at));
  return errors;
}

// One valid block of every type, using every key the type may carry (the test below checks that), so each property of each branch of the schema is met by a valid document.
const edges = { role: "content", padding: [4, 8, 12, 16] };
const everyBlock = (): unknown[] => [
  { id: "logo", type: "image", ...edges, src: "artifact:logo-1", alt: "Logo", width: 120, height: 40.5, align: "center", href: "https://example.com", bleed: true },
  { id: "headline", type: "heading", ...edges, text: "Thanks for joining us", size: 37.3, lineHeight: 37.3, color: "primary", align: "left", italic: true, weight: 700, font: "heading", letterSpacing: "-0.032em" },
  { id: "intro", type: "text", ...edges, text: "One **two** [three](https://example.com/a)", size: 16, lineHeight: 24, listLineHeight: 22, gap: 12, color: "text", linkColor: "accent", align: "left", italic: false, weight: 400, font: "body", letterSpacing: "0em" },
  { id: "note", type: "highlight", ...edges, text: "Make one decision the night before.", size: 17, lineHeight: 20, gap: 8, color: "white", background: "primary", align: "center", italic: false, weight: 0, font: "base", letterSpacing: "0.05em" },
  { id: "cta", type: "button", ...edges, label: "Go", href: "{{confirmUrl}}", color: "accent", textColor: "white", width: 240, align: "center", size: 16, paddingY: 12.5, font: "body" },
  { id: "rule", type: "divider", ...edges, color: "muted", height: 1 },
  { id: "gap", type: "spacer", role: "brand", height: 24 },
  { id: "band", type: "section", ...edges, background: "tint", inset: [16, 16, 16, 16], maxWidth: 502, cellPadding: 8, blocks: [{ id: "inside", type: "text", text: "Inside" }] },
  { id: "pair", type: "columns", ...edges, background: "canvas", inset: [0, 0, 0, 0], maxWidth: 552, gap: 8, stackBelow: 450, columns: [
    { width: 50, cellPadding: 4, blocks: [{ id: "left", type: "heading", text: "Left" }] },
    { width: 46.82, blocks: [{ id: "right", type: "image", width: 193, src: "", alt: "" }] },
  ] },
  { id: "follow", type: "social", ...edges, title: "Follow us", links: [{ network: "Example", href: "https://example.com/x", icon: "artifact:icon-1", width: 32 }, { network: "Mail", href: "mailto:hello@example.com" }], maxWidth: 300, gap: 8 },
  { id: "raw", type: "html", ...edges, html: "<p>Hello</p>" },
  { id: "fine", type: "footer", ...edges, links: [{ label: "Unsubscribe", href: "{{unsubscribeUrl}}" }, { label: "Plain", href: "" }], lines: ["Acme Inc.", "1 Example Street"], background: "page", inset: [16, 16, 16, 16], maxWidth: 560, cellPadding: 8, size: 12, lineHeight: 18, letterSpacing: "0.01em", color: "muted", linkColor: "primary", separator: " | ", gap: 4, after: 8, font: "base" },
];

test("the kitchen-sink document uses every key of every block type", () => {
  const used = new Map<string, Set<string>>();
  walkBlocks(everyBlock() as EmailBlock[], block => {
    const keys = used.get(block.type) ?? new Set<string>();
    Object.keys(block).forEach(key => keys.add(key));
    used.set(block.type, keys);
  });
  for (const type of BLOCK_TYPES) assert.deepEqual([...(used.get(type) ?? [])].sort(), [...BLOCK_FIELDS[type]].sort(), type);
  assert.deepEqual(unknownFields(read(everyBlock())), []);
});

test("the generated schema accepts what the engine accepts and rejects what the editor can tell is wrong", () => {
  const schema = emailSchema() as Node;
  const errorsFor = (value: unknown) => check(schema, value, schema);
  const document = (blocks: unknown[], extra: Record<string, unknown> = {}) => ({ subject: "Hello", preheader: "Soon", layout: "builtin:letter", theme, blocks, ...extra });
  const oneBlock = (block: Record<string, unknown>) => errorsFor(document([block]));

  assert.equal(schema.$schema, "http://json-schema.org/draft-07/schema#");
  assert.deepEqual(errorsFor(document(everyBlock())), []);
  assert.deepEqual(errorsFor({ subject: "Plain", textBody: "Hello", htmlBody: "" }), []);
  assert.deepEqual(errorsFor({ subject: "Plain", preheader: "Soon", textBody: "Hello", htmlBody: "<p>Hello</p>" }), []);
  assert.deepEqual(errorsFor(document([], { subject: "\ufeff", preheader: "" })), [], "what the engine takes as a subject");
  assert.deepEqual(errorsFor(document([], { subject: "é".repeat(499), preheader: "😀".repeat(200) })), []);
  assert.deepEqual(errorsFor(document([])), []);
  assert.deepEqual(errorsFor(document([], { theme: { ...theme, webFonts: true } })), []);
  // Each block type is covered by the document above; its own keys are what the field table says.
  const covered = new Set<string>();
  walkBlocks(everyBlock() as EmailBlock[], block => covered.add(block.type));
  assert.deepEqual([...covered].sort(), [...BLOCK_TYPES].sort());

  const refusals: [string, unknown][] = [
    ["a colour that is not a theme colour", oneBlock({ id: "h", type: "heading", text: "x", color: "#ff0000" })],
    ["a colour name", oneBlock({ id: "h", type: "heading", text: "x", color: "red" })],
    ["a background that is not a theme colour", oneBlock({ id: "s", type: "section", background: "#fff" })],
    ["a key the type does not have", oneBlock({ id: "h", type: "heading", text: "x", src: "artifact:a" })],
    ["a key that is nobody's", oneBlock({ id: "h", type: "heading", text: "x", bogus: 1 })],
    ["padding on a spacer", oneBlock({ id: "s", type: "spacer", height: 4, padding: [0, 0, 0, 0] })],
    ["an unknown type", oneBlock({ id: "m", type: "marquee" })],
    ["a missing type", oneBlock({ id: "m" })],
    ["a missing id", oneBlock({ type: "heading" })],
    ["an id with capitals", oneBlock({ id: "Hero_1", type: "spacer", height: 4 })],
    ["an id that starts with a hyphen", oneBlock({ id: "-a", type: "spacer", height: 4 })],
    ["an id of 65 characters", oneBlock({ id: "a".repeat(65), type: "spacer", height: 4 })],
    ["an alignment of justify", oneBlock({ id: "h", type: "heading", align: "justify" })],
    ["a font of fancy", oneBlock({ id: "h", type: "heading", font: "fancy" })],
    ["a weight of 500", oneBlock({ id: "h", type: "heading", weight: 500 })],
    ["a size of 7", oneBlock({ id: "h", type: "heading", size: 7 })],
    ["a size of 97", oneBlock({ id: "h", type: "heading", size: 97 })],
    ["letter spacing in pixels", oneBlock({ id: "h", type: "heading", letterSpacing: "2px" })],
    ["a script link", oneBlock({ id: "b", type: "button", label: "Go", href: "javascript:alert(1)" })],
    ["a link with a space", oneBlock({ id: "b", type: "button", label: "Go", href: "https://a b" })],
    ["a merge tag as a link", oneBlock({ id: "b", type: "button", label: "Go", href: "{{fields.url}}" })],
    ["a link that is not text", oneBlock({ id: "b", type: "button", label: "Go", href: 5 })],
    ["a button without a label", oneBlock({ id: "b", type: "button", href: "" })],
    ["a button label of 201 characters", oneBlock({ id: "b", type: "button", label: "l".repeat(201) })],
    ["a button wider than 600", oneBlock({ id: "b", type: "button", label: "Go", width: 601 })],
    ["an image without a width", oneBlock({ id: "i", type: "image" })],
    ["an image of width 0", oneBlock({ id: "i", type: "image", width: 0 })],
    ["an image of fractional width", oneBlock({ id: "i", type: "image", width: 10.5 })],
    ["a picture from the web", oneBlock({ id: "i", type: "image", width: 10, src: "https://cdn.example/a.png" })],
    ["an SVG picture", oneBlock({ id: "i", type: "image", width: 10, src: "data:image/svg+xml;base64,PHN2Zy8+" })],
    ["a Library file without an id", oneBlock({ id: "i", type: "image", width: 10, src: "artifact:" })],
    ["padding of three numbers", oneBlock({ id: "h", type: "heading", padding: [1, 2, 3] })],
    ["padding over 200", oneBlock({ id: "h", type: "heading", padding: [1, 2, 3, 201] })],
    ["a spacer taller than 2000", oneBlock({ id: "s", type: "spacer", height: 2001 })],
    ["a role of both", oneBlock({ id: "h", type: "heading", role: "both" })],
    ["columns without columns", oneBlock({ id: "c", type: "columns" })],
    ["five columns", oneBlock({ id: "c", type: "columns", columns: Array.from({ length: 5 }, () => ({ width: 20, blocks: [] })) })],
    ["a column of width 0", oneBlock({ id: "c", type: "columns", columns: [{ width: 0, blocks: [] }] })],
    ["a column with a key of its own", oneBlock({ id: "c", type: "columns", columns: [{ width: 50, blocks: [], bogus: 1 }] })],
    ["a bad block inside a section", oneBlock({ id: "s", type: "section", blocks: [{ id: "h", type: "heading", color: "red" }] })],
    ["a bad block inside a column", oneBlock({ id: "c", type: "columns", columns: [{ width: 50, blocks: [{ id: "m", type: "marquee" }] }] })],
    ["social without links", oneBlock({ id: "s", type: "social" })],
    ["social with an empty list of links", oneBlock({ id: "s", type: "social", links: [] })],
    ["nine social links", oneBlock({ id: "s", type: "social", links: Array.from({ length: 9 }, () => ({ network: "n", href: "" })) })],
    ["a social link with a label", oneBlock({ id: "s", type: "social", links: [{ network: "n", label: "x" }] })],
    ["a social link with a bad address", oneBlock({ id: "s", type: "social", links: [{ network: "n", href: "javascript:1" }] })],
    ["a footer link with a network", oneBlock({ id: "f", type: "footer", links: [{ label: "a", network: "b" }] })],
    ["a footer link without a label", oneBlock({ id: "f", type: "footer", links: [{ href: "" }] })],
    ["thirteen footer lines", oneBlock({ id: "f", type: "footer", lines: Array.from({ length: 13 }, () => "x") })],
    ["a footer line of 301 characters", oneBlock({ id: "f", type: "footer", lines: ["x".repeat(301)] })],
    ["a theme colour that is not a hex colour", errorsFor(document([], { theme: { ...theme, primary: "red" } }))],
    ["a theme without a font", errorsFor(document([], { theme: Object.fromEntries(Object.entries(theme).filter(([key]) => key !== "bodyFont")) }))],
    ["a font name that starts with a digit", errorsFor(document([], { theme: { ...theme, headingFont: "1x" } }))],
    ["a theme key of its own", errorsFor(document([], { theme: { ...theme, bogus: "x" } }))],
    ["no theme", errorsFor({ subject: "Hello", blocks: [] })],
    ["both modes", errorsFor(document([], { textBody: "y", htmlBody: "" }))],
    ["neither mode", errorsFor({ subject: "Hello" })],
    ["a custom email with a layout", errorsFor({ subject: "Plain", textBody: "Hello", htmlBody: "", layout: "builtin:letter" })],
    ["an account id", errorsFor({ subject: "Plain", textBody: "Hello", htmlBody: "", accountId: "c" })],
    ["an empty subject", errorsFor(document([], { subject: "" }))],
    ["a subject of spaces", errorsFor(document([], { subject: " \u00a0 " }))],
    ["a subject on two lines", errorsFor(document([], { subject: "a\nb" }))],
    ["a subject with a control character", errorsFor(document([], { subject: "a\u0001b" }))],
    ["a preheader of 201 characters", errorsFor(document([], { preheader: "p".repeat(201) }))],
    ["a preheader on two lines", errorsFor(document([], { preheader: "a\nb" }))],
    ["a plain-text body of spaces", errorsFor({ subject: "Plain", textBody: " \n ", htmlBody: "" })],
  ];
  for (const [name, errors] of refusals) assert.notDeepEqual(errors, [], name);

  // Every refusal above is specific: against the layout alone, the error points at the thing that is wrong.
  const layoutErrors = (block: Record<string, unknown>) => check((schema.definitions as Record<string, Node>).layoutEmail as Node, document([block]), schema);
  assert.match(layoutErrors({ id: "h", type: "heading", text: "x", color: "#ff0000" }).join("\n"), /\/blocks\/0\/color/);
  assert.match(layoutErrors({ id: "h", type: "heading", text: "x", src: "artifact:a" }).join("\n"), /has no property src/);
  assert.match(layoutErrors({ id: "s", type: "section", blocks: [{ id: "h", type: "heading", color: "red" }] }).join("\n"), /\/blocks\/0\/blocks\/0\/color/);
  assert.match(layoutErrors({ id: "c", type: "columns", columns: [{ width: 50, blocks: [{ id: "m", type: "marquee" }] }] }).join("\n"), /\/blocks\/0\/columns\/0\/blocks\/0\/type/);
});

test("a pattern in the schema tells the person what to do, not what the pattern is", () => {
  const sentences = new Map<string, string>();
  const visit = (node: unknown): void => {
    if (Array.isArray(node)) return node.forEach(visit);
    if (typeof node !== "object" || node === null) return;
    const schema = node as Record<string, unknown>;
    if (typeof schema.pattern === "string") {
      assert.equal(typeof schema.patternErrorMessage, "string", `no sentence for ${schema.pattern.slice(0, 40)}`);
      sentences.set(schema.pattern, schema.patternErrorMessage as string);
    }
    Object.values(schema).forEach(visit);
  };
  visit(emailSchema());
  assert.ok(sentences.size >= 8, `${sentences.size} patterns`);
  for (const sentence of sentences.values()) {
    assert.match(sentence, /^[A-Z][^]*\.$/, sentence);
    assert.doesNotMatch(sentence, /\^|\$|\\|\(\?/, "no regular expression syntax in a sentence");
    assert.doesNotMatch(sentence, /--|receipt|envelope|capabilit|console|portal/i, sentence);
  }
  assert.ok([...sentences.values()].includes("Enter a web address, an email address (mailto:), a phone number (tel:) or a link tag such as {{unsubscribeUrl}}."), "the link sentence is the one editing uses");
});

// Each pair is a block with a value on the edge of what the engine takes, and the same block one step outside it.
const edgeCases: [string, Record<string, unknown>, Record<string, unknown>][] = [
  ["size", { id: "x", type: "heading", size: 8 }, { id: "x", type: "heading", size: 7.99 }],
  ["size", { id: "x", type: "heading", size: 96 }, { id: "x", type: "heading", size: 96.01 }],
  ["line height", { id: "x", type: "text", lineHeight: 200, listLineHeight: 200 }, { id: "x", type: "text", lineHeight: 200.5 }],
  ["padding", { id: "x", type: "heading", padding: [0, 200, 0, 200] }, { id: "x", type: "heading", padding: [0, 200.01, 0, 0] }],
  ["padding below zero", { id: "x", type: "heading", padding: [0, 0, 0, 0] }, { id: "x", type: "heading", padding: [-1, 0, 0, 0] }],
  ["image width", { id: "x", type: "image", width: 1 }, { id: "x", type: "image", width: 0 }],
  ["image width above", { id: "x", type: "image", width: 600 }, { id: "x", type: "image", width: 601 }],
  ["button width", { id: "x", type: "button", label: "Go", width: 0 }, { id: "x", type: "button", label: "Go", width: 601 }],
  ["height", { id: "x", type: "spacer", height: 2000 }, { id: "x", type: "spacer", height: 2000.5 }],
  ["max width", { id: "x", type: "section", maxWidth: 2000 }, { id: "x", type: "section", maxWidth: 2001 }],
  ["button label", { id: "x", type: "button", label: "l".repeat(200) }, { id: "x", type: "button", label: "l".repeat(201) }],
  ["letter spacing", { id: "x", type: "heading", letterSpacing: "-12.345em" }, { id: "x", type: "heading", letterSpacing: "123em" }],
  ["a single column", { id: "x", type: "columns", columns: [{ width: 100, blocks: [] }] }, { id: "x", type: "columns", columns: [{ width: 100.02, blocks: [] }] }],
  ["four columns", { id: "x", type: "columns", columns: Array.from({ length: 4 }, () => ({ width: 25, blocks: [] })) }, { id: "x", type: "columns", columns: Array.from({ length: 5 }, () => ({ width: 20, blocks: [] })) }],
  ["eight social links", { id: "x", type: "social", links: Array.from({ length: 8 }, () => ({ network: "n" })) }, { id: "x", type: "social", links: Array.from({ length: 9 }, () => ({ network: "n" })) }],
  ["twelve footer lines", { id: "x", type: "footer", lines: Array.from({ length: 12 }, () => "l".repeat(300)) }, { id: "x", type: "footer", lines: ["l".repeat(301)] }],
  ["separator", { id: "x", type: "footer", separator: "s".repeat(40) }, { id: "x", type: "footer", separator: "s".repeat(41) }],
  ["id", { id: "a".repeat(64), type: "divider" }, { id: "a".repeat(65), type: "divider" }],
  ["social network", { id: "x", type: "social", links: [{ network: "n".repeat(40) }] }, { id: "x", type: "social", links: [{ network: "n".repeat(41) }] }],
  ["footer link label", { id: "x", type: "footer", links: [{ label: "l".repeat(80) }] }, { id: "x", type: "footer", links: [{ label: "l".repeat(81) }] }],
  ["social icon width", { id: "x", type: "social", links: [{ network: "n", width: 600 }] }, { id: "x", type: "social", links: [{ network: "n", width: 601 }] }],
];

test("the schema's bounds are the engine's, on both sides of each edge", () => {
  const schema = emailSchema() as Node;
  for (const [name, inside, outside] of edgeCases) {
    const document = (block: Record<string, unknown>) => ({ subject: "Hello", theme, blocks: [block] });
    assert.deepEqual(check(schema, document(inside), schema), [], `${name}: the edge itself`);
    assert.notDeepEqual(check(schema, document(outside), schema), [], `${name}: one step outside`);
  }
});

test("the schema is built from the field table", () => {
  const schema = emailSchema() as { definitions: { block: { allOf: { if: Node; then: { properties: Record<string, unknown>; additionalProperties: boolean } }[] } } };
  const branches = schema.definitions.block.allOf;
  assert.equal(branches.length, BLOCK_TYPES.length);
  BLOCK_TYPES.forEach((type, index) => {
    const branch = branches[index];
    assert.deepEqual(branch?.if, { properties: { type: { const: type } }, required: ["type"] }, type);
    assert.equal(branch?.then.additionalProperties, false, type);
    assert.deepEqual(Object.keys(branch?.then.properties ?? {}), [...BLOCK_FIELDS[type]], `${type}: one property for every field the block may carry`);
  });
  const text = JSON.stringify(emailSchema());
  const defined = Object.keys((emailSchema() as { definitions: Node }).definitions);
  const referenced = new Set([...text.matchAll(/"#\/definitions\/(\w+)"/g)].map(match => match[1] as string));
  assert.ok(referenced.size >= 6, "the shared definitions are used");
  for (const ref of referenced) assert.ok(defined.includes(ref), `${ref} is defined`);
  for (const ref of defined) assert.ok(referenced.has(ref) || ref === "layoutEmail" || ref === "customEmail", `${ref} is used`);
  assert.deepEqual(emailSchema(), emailSchema(), "generating twice gives the same schema");
});
