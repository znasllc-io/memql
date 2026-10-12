import { describe, expect, it } from "vitest";
import type { LibraryFileFacts, LibraryFileLookup } from "@znasllc-io/memql-sdk-core/client";

import type { ComposeFormat } from "../../src/items/materialize";
import {
  FILE_KINDS,
  MAKE_STEPS,
  answerOf,
  blocker,
  describeAnswer,
  draftFromComposition,
  draftFromSetup,
  fileKindEntry,
  filedName,
  floorActs,
  formatOf,
  ingredientsPhrase,
  keepUsableTemplate,
  openingStep,
  pathOf,
  readableReference,
  readyWords,
  requestOf,
  stepState,
  stepsOf,
  templatesFor,
  withKind,
  type AnswerNames,
  type FileKind,
  type Ingredient,
  type MakeDraft,
  type MakeFacts,
  type MakeStepId,
} from "../../src/apps/materializer/make";
import type { CompositionRow, SourceRow, TemplateRow } from "../../src/apps/materializer/rows";

// The Make a file model, with no DOM: what the wizard asks, in what order, what it answers
// on the rail, what its floor says and offers, and the request a draft sends. Every string is
// the spec's (docs/superpowers/specs/2026-10-12-materializer-app-design.md, 2.1 to 2.10 and
// "Words on screen").

const KIB = 1024;
const MIB = 1024 * KIB;

// What the window has read of the person's Library files, by the id the tray picks each with.
// null is a file whose row cannot be read; an id that is not here is one the window has not read.
const LIBRARY: Record<string, LibraryFileFacts | null> = {
  "file-notes": { name: "notes.md", size: 2 * KIB, archived: false },
  "file-logo": { name: "logo.png", size: 300 * KIB, archived: false },
  "file-data": { name: "data.csv", size: 4 * KIB, archived: false },
  "file-big": { name: "minutes.md", size: 40 * KIB, archived: false },
  "file-old": { name: "old.md", size: 2 * KIB, archived: true },
  "file-gone": null,
};
const known: LibraryFileLookup = (id) => LIBRARY[id];

const facts = (over: Partial<MakeFacts> = {}): MakeFacts => ({ templates: [], templatesLoaded: true, inference: null, files: known, ...over });
const template = (id: string, name: string, format = "", over: Partial<TemplateRow> = {}): TemplateRow => ({
  id, createdAt: "2026-10-01T09:00:00Z", name, description: `The ${name} look`, fileId: `${id}-file`, format,
  placeholders: [], accountIds: [], archived: false, ...over,
});
const pick = (ref: string, label: string): Ingredient => ({ kind: "library_file", ref, label });
const NOTES = pick("file-notes", "notes.md");
const LOGO = pick("file-logo", "logo.png");
const DATA = pick("file-data", "data.csv");

const draft = (over: Partial<MakeDraft> = {}): MakeDraft => ({ ...draftFromSetup(null, "markdown"), ...over });
const makeDoc = (format: ComposeFormat, over: Partial<MakeDraft> = {}): MakeDraft =>
  draft({ fileKind: { kind: "document", format }, ...over });
const email = (over: Partial<MakeDraft> = {}): MakeDraft => draft({ fileKind: { kind: "emailTemplate" }, ...over });
const staticSite = (over: Partial<MakeDraft> = {}): MakeDraft =>
  draft({ fileKind: { kind: "package", deployableKind: "static" }, ...over });

const names: AnswerNames = {
  folder: (id) => ({ "f-projects": "Projects", "f-gone": null } as Record<string, string | null>)[id],
  template: (id) => ({ t1: "Board pack" } as Record<string, string>)[id] ?? id,
  account: (id) => ({ "acct-1": "Acme Ltd" } as Record<string, string>)[id] ?? id,
};

const composition = (over: Partial<CompositionRow> = {}): CompositionRow => ({
  id: "c1", createdAt: "2026-10-12T09:00:00Z", ownerUserId: "u1", name: "Q3 summary", statement: "", status: "ready",
  outputKind: "", format: "markdown", templateId: "", outputFileId: "f1", folderId: "", accountIds: [], goalId: "",
  runId: "", recipeId: "", provenanceEmbedded: null, provenanceNote: "", deployableKind: "", failureReason: "",
  archived: false, ...over,
});
const recorded = (kind: string, ref: string, label: string): SourceRow => ({ kind, ref, label, capturedAt: "2026-10-12T09:00:00Z" });

describe("MAKE_STEPS", () => {
  it("names each step and says what it asks, as the spec's table does", () => {
    expect(MAKE_STEPS).toEqual({
      kind: { name: "Kind of file", sentence: "What comes out." },
      template: { name: "Template", sentence: "How it looks." },
      from: { name: "From", sentence: "What to make it from." },
      describe: { name: "Describe", sentence: "What it should say." },
      name: { name: "Name", sentence: "What to call it." },
      folder: { name: "Folder", sentence: "Where it is filed." },
      organization: { name: "Organization", sentence: "Who it is for." },
      file: { name: "File", sentence: "What you get." },
    });
  });
});

describe("FILE_KINDS", () => {
  it("offers the kinds in the spec's order, in the words a person reads", () => {
    expect(FILE_KINDS.map((entry) => entry.label)).toEqual([
      "Markdown", "Web page", "Plain text", "Spreadsheet (CSV)", "Data (JSON)", "Word document", "PDF",
      "Single-page app package", "Static site package", "Shopify storefront package",
      "Email template",
    ]);
  });

  it("describes the packages and the email template, and nothing else", () => {
    expect(FILE_KINDS.filter((entry) => entry.description !== undefined).map((entry) => [entry.label, entry.description])).toEqual([
      ["Single-page app package", "A package for Deployables"],
      ["Static site package", "A package for Deployables"],
      ["Shopify storefront package", "A package for Deployables"],
      ["Email template", "Opens in the email editor"],
    ]);
  });

  it("is one choice for a package, never a format crossed with a deploy kind", () => {
    expect(FILE_KINDS.map((entry) => entry.kind)).toEqual([
      { kind: "document", format: "markdown" },
      { kind: "document", format: "html" },
      { kind: "document", format: "txt" },
      { kind: "document", format: "csv" },
      { kind: "document", format: "json" },
      { kind: "document", format: "docx" },
      { kind: "document", format: "pdf" },
      { kind: "package", deployableKind: "spa" },
      { kind: "package", deployableKind: "static" },
      { kind: "package", deployableKind: "shopify_storefront" },
      { kind: "emailTemplate" },
    ]);
  });

  it("gives every kind a value of its own, and names the kind a draft holds", () => {
    expect(new Set(FILE_KINDS.map((entry) => entry.value)).size).toBe(FILE_KINDS.length);
    for (const entry of FILE_KINDS) expect(fileKindEntry(entry.kind)).toEqual(entry);
    expect(fileKindEntry({ kind: "package", deployableKind: "static" }).label).toBe("Static site package");
  });
});

describe("formatOf", () => {
  it("is the document's format, HTML for a package and JSON for an email template", () => {
    expect(formatOf({ kind: "document", format: "pdf" })).toBe("pdf");
    expect(formatOf({ kind: "package", deployableKind: "spa" })).toBe("html");
    expect(formatOf({ kind: "package", deployableKind: "shopify_storefront" })).toBe("html");
    expect(formatOf({ kind: "emailTemplate" })).toBe("json");
  });
});

describe("draftFromSetup", () => {
  it("starts the plus with a document of the default format and nothing answered or passed", () => {
    expect(draftFromSetup(null, "markdown")).toEqual({
      fileKind: { kind: "document", format: "markdown" }, templateId: "", ingredients: [], statement: "",
      name: "", folderId: "", accountId: "", passed: { from: false, describe: false },
    });
    expect(draftFromSetup(null, "csv").fileKind).toEqual({ kind: "document", format: "csv" });
  });

  it("takes a sender's format and folder, and counts From and Describe as passed", () => {
    expect(draftFromSetup({ format: "markdown", folderId: "f-projects", returnTo: { appId: "files", sectionId: "browse" } }, "pdf")).toEqual({
      fileKind: { kind: "document", format: "markdown" }, templateId: "", ingredients: [], statement: "",
      name: "", folderId: "f-projects", accountId: "", passed: { from: true, describe: true },
    });
  });

  it("starts on the default format when the setup names none", () => {
    expect(draftFromSetup({ folderId: "f-projects" }, "txt").fileKind).toEqual({ kind: "document", format: "txt" });
    expect(draftFromSetup({}, "html")).toMatchObject({ fileKind: { kind: "document", format: "html" }, folderId: "", passed: { from: true, describe: true } });
  });

  it("takes a sender's sources as ingredients, once each", () => {
    const sources = [
      { kind: "library_file" as const, ref: "file-notes", label: "notes.md" },
      { kind: "concept_row" as const, ref: "v1:x:invoice#a", label: "INV-1001" },
      { kind: "library_file" as const, ref: "file-notes", label: "notes.md" },
    ];
    expect(draftFromSetup({ sources }, "markdown").ingredients).toEqual([
      { kind: "library_file", ref: "file-notes", label: "notes.md" },
      { kind: "concept_row", ref: "v1:x:invoice#a", label: "INV-1001" },
    ]);
  });

  it("makes an email template of an output kind with JSON or no format, and keeps the sender's other format", () => {
    expect(draftFromSetup({ outputKind: "email_template" }, "markdown").fileKind).toEqual({ kind: "emailTemplate" });
    expect(draftFromSetup({ outputKind: "email_template", format: "json" }, "markdown").fileKind).toEqual({ kind: "emailTemplate" });
    expect(draftFromSetup({ outputKind: "email_template", format: "markdown" }, "pdf").fileKind).toEqual({ kind: "document", format: "markdown" });
  });
});

describe("draftFromComposition", () => {
  it("fills every step from the record, with From and Describe passed", () => {
    const record = composition({
      name: "Q3 summary", statement: "A one-page summary", templateId: "t1", folderId: "f-projects", accountIds: ["acct-1", "acct-2"], format: "pdf",
    });
    expect(draftFromComposition(record, [recorded("library_file", "file-notes", "notes.md"), recorded("query", "query openInvoices()", "Open invoices")])).toEqual({
      fileKind: { kind: "document", format: "pdf" }, templateId: "t1",
      ingredients: [
        { kind: "library_file", ref: "file-notes", label: "notes.md" },
        { kind: "query", ref: "query openInvoices()", label: "Open invoices" },
      ],
      statement: "A one-page summary", name: "Q3 summary", folderId: "f-projects", accountId: "acct-1",
      passed: { from: true, describe: true },
    });
  });

  it("reads a package from its deployable kind and an email template from its output kind", () => {
    expect(draftFromComposition(composition({ format: "html", deployableKind: "static" }), []).fileKind).toEqual({ kind: "package", deployableKind: "static" });
    expect(draftFromComposition(composition({ format: "html", deployableKind: "spa" }), []).fileKind).toEqual({ kind: "package", deployableKind: "spa" });
    expect(draftFromComposition(composition({ format: "json", outputKind: "email_template" }), []).fileKind).toEqual({ kind: "emailTemplate" });
  });

  it("falls back to the record's format when its deployable kind is one this app does not know", () => {
    expect(draftFromComposition(composition({ format: "html", deployableKind: "kiosk" }), []).fileKind).toEqual({ kind: "document", format: "html" });
  });

  it("has no organization when the record had none, and keeps only the kinds of source the wizard holds, once each", () => {
    const made = draftFromComposition(composition(), [recorded("audio", "x", "x"), recorded("library_file", "file-notes", "notes.md"), recorded("library_file", "file-notes", "notes.md")]);
    expect(made.accountId).toBe("");
    expect(made.ingredients).toEqual([{ kind: "library_file", ref: "file-notes", label: "notes.md" }]);
  });

  it("opens on Describe, because nothing is waiting", () => {
    expect(openingStep(draftFromComposition(composition({ statement: "A one-page summary" }), [recorded("library_file", "file-notes", "notes.md")]), facts())).toBe("describe");
  });
});

describe("stepsOf", () => {
  it("is the six steps of an ordinary file, File last", () => {
    expect(stepsOf(draft(), facts())).toEqual(["kind", "from", "describe", "name", "folder", "file"]);
  });

  it("adds Template only when a live template makes this kind: its format, or none named", () => {
    const board = template("t1", "Board pack", "markdown");
    const anyKind = template("t2", "House style", "");
    const page = template("t3", "Landing", "html");
    const old = template("t4", "Old pack", "markdown", { archived: true });
    const withTemplate = ["kind", "template", "from", "describe", "name", "folder", "file"];
    expect(stepsOf(draft(), facts({ templates: [board] }))).toEqual(withTemplate);
    expect(stepsOf(draft(), facts({ templates: [anyKind] }))).toEqual(withTemplate);
    expect(stepsOf(draft(), facts({ templates: [page] }))).not.toContain("template");
    expect(stepsOf(draft(), facts({ templates: [old] }))).not.toContain("template");
    expect(stepsOf(makeDoc("html"), facts({ templates: [page] }))).toEqual(withTemplate);
  });

  it("asks a package's template for HTML, and an email template's for JSON", () => {
    const page = template("t3", "Landing", "html");
    const data = template("t5", "Welcome", "json");
    expect(stepsOf(staticSite(), facts({ templates: [page] }))).toContain("template");
    expect(stepsOf(email(), facts({ templates: [page] }))).not.toContain("template");
    expect(stepsOf(email(), facts({ templates: [data] }))).toContain("template");
  });

  it("adds Organization for an email template, or for a draft that already carries one", () => {
    expect(stepsOf(email(), facts())).toEqual(["kind", "from", "describe", "name", "folder", "organization", "file"]);
    expect(stepsOf(draft({ accountId: "acct-1" }), facts())).toEqual(["kind", "from", "describe", "name", "folder", "organization", "file"]);
    expect(stepsOf(draft(), facts())).not.toContain("organization");
  });
});

describe("stepState", () => {
  const state = (step: MakeStepId, d: MakeDraft = draft()) => stepState(step, d, facts());

  it("answers Kind of file, Template and Folder by what they start as", () => {
    expect(state("kind")).toBe("done");
    expect(state("template")).toBe("done");
    expect(state("folder")).toBe("done");
    expect(state("folder", draft({ folderId: "f-projects" }))).toBe("done");
  });

  it("waits on From and Describe until they hold something or are passed", () => {
    expect(state("from")).toBe("waiting");
    expect(state("describe")).toBe("waiting");
    expect(state("from", draft({ passed: { from: true, describe: false } }))).toBe("skipped");
    expect(state("describe", draft({ passed: { from: false, describe: true } }))).toBe("skipped");
    expect(state("from", draft({ ingredients: [NOTES] }))).toBe("done");
    expect(state("describe", draft({ statement: "A one-page summary" }))).toBe("done");
  });

  it("holds something only when it is more than spaces, and passing does not undo it", () => {
    expect(state("describe", draft({ statement: "   \n " }))).toBe("waiting");
    expect(state("describe", draft({ statement: "  ", passed: { from: true, describe: true } }))).toBe("skipped");
    expect(state("from", draft({ ingredients: [NOTES], passed: { from: true, describe: true } }))).toBe("done");
  });

  it("answers Name when the SDK finds nothing wrong with it", () => {
    expect(state("name")).toBe("waiting");
    expect(state("name", draft({ name: "Plan" }))).toBe("done");
    expect(state("name", draft({ name: "a/b" }))).toBe("waiting");
    expect(state("name", draft({ name: ".md" }))).toBe("waiting");
    expect(state("name", draft({ name: "x".repeat(151) }))).toBe("waiting");
  });

  it("judges a name against the kind's own format", () => {
    expect(state("name", makeDoc("pdf", { name: ".pdf" }))).toBe("waiting");
    expect(state("name", makeDoc("pdf", { name: ".md" }))).toBe("done");
  });

  it("answers Organization when set, waits for an email template without one, and is otherwise skipped", () => {
    expect(state("organization", email({ accountId: "acct-1" }))).toBe("done");
    expect(state("organization", draft({ accountId: "acct-1" }))).toBe("done");
    expect(state("organization", email())).toBe("waiting");
    expect(state("organization", draft())).toBe("skipped");
  });

  it("never waits on File: the cluster's step is not the person's to answer", () => {
    expect(state("file")).not.toBe("waiting");
  });
});

describe("openingStep", () => {
  it("opens the plus on From, because the plus is for making something from things", () => {
    expect(openingStep(draftFromSetup(null, "markdown"), facts())).toBe("from");
  });

  it("opens a Files setup on Name, with From and Describe passed with nothing", () => {
    const files = draftFromSetup({ format: "markdown", folderId: "f-projects", returnTo: { appId: "files", sectionId: "browse" } }, "markdown");
    expect(openingStep(files, facts())).toBe("name");
    expect(stepState("from", files, facts())).toBe("skipped");
    expect(stepState("describe", files, facts())).toBe("skipped");
  });

  it("opens a setup that carried sources on Name too, with From answered rather than skipped", () => {
    const setup = draftFromSetup({ format: "markdown", sources: [{ kind: "library_file", ref: "file-notes", label: "notes.md" }] }, "markdown");
    expect(openingStep(setup, facts())).toBe("name");
    expect(stepState("from", setup, facts())).toBe("done");
    expect(stepState("describe", setup, facts())).toBe("skipped");
  });

  it("opens on the first step still waiting, in the order of the steps", () => {
    expect(openingStep(draft({ ingredients: [NOTES] }), facts())).toBe("describe");
    expect(openingStep(draft({ ingredients: [NOTES], statement: "A summary" }), facts())).toBe("name");
    expect(openingStep(draft({ ingredients: [NOTES], statement: "A summary", name: "Plan" }), facts())).toBe("describe");
  });

  it("asks an email template for its organization after its name", () => {
    const setup = draftFromSetup({ outputKind: "email_template" }, "markdown");
    expect(openingStep(setup, facts())).toBe("name");
    expect(openingStep({ ...setup, name: "Welcome" }, facts())).toBe("organization");
    expect(openingStep({ ...setup, name: "Welcome", accountId: "acct-1" }, facts())).toBe("describe");
  });

  it("opens on Describe when nothing waits, the answer most often changed before trying again", () => {
    const answered = draft({ name: "Plan", statement: "A summary", ingredients: [NOTES] });
    expect(openingStep(answered, facts())).toBe("describe");
  });
});

describe("answerOf", () => {
  const answer = (step: MakeStepId, d: MakeDraft = draft()) => answerOf(step, d, names);

  it("names the kind of file as the choice does", () => {
    expect(answer("kind")).toBe("Markdown");
    expect(answer("kind", staticSite())).toBe("Static site package");
    expect(answer("kind", email())).toBe("Email template");
    expect(answer("kind", makeDoc("csv"))).toBe("Spreadsheet (CSV)");
  });

  it("names the template, or says there is none", () => {
    expect(answer("template")).toBe("No template");
    expect(answer("template", draft({ templateId: "t1" }))).toBe("Board pack");
  });

  it("says what is picked, and nothing while From still waits", () => {
    expect(answer("from")).toBeUndefined();
    expect(answer("from", draft({ passed: { from: true, describe: false } }))).toBe("Nothing picked");
    expect(answer("from", draft({ ingredients: [NOTES] }))).toBe("notes.md");
  });

  it("says what is described, and nothing while Describe still waits", () => {
    expect(answer("describe")).toBeUndefined();
    expect(answer("describe", draft({ passed: { from: false, describe: true } }))).toBe("Not described");
    expect(answer("describe", draft({ statement: "A one-page summary" }))).toBe("A one-page summary");
  });

  it("shows the name as it will be filed, once it can be used", () => {
    expect(answer("name")).toBeUndefined();
    expect(answer("name", draft({ name: "a/b" }))).toBeUndefined();
    expect(answer("name", draft({ name: "Plan" }))).toBe("Plan.md");
    expect(answer("name", staticSite({ name: "Site" }))).toBe("Site.zip");
    expect(answer("name", email({ name: "Welcome" }))).toBe("Welcome.email.json");
  });

  it("says Library for the top level, the folder's name, Unavailable folder once the list has loaded without it, and nothing while it loads", () => {
    expect(answer("folder")).toBe("Library");
    expect(answer("folder", draft({ folderId: "f-projects" }))).toBe("Projects");
    expect(answer("folder", draft({ folderId: "f-gone" }))).toBe("Unavailable folder");
    expect(answer("folder", draft({ folderId: "f-loading" }))).toBeUndefined();
  });

  it("names the organization, says there is none, and has no answer while an email template still needs one", () => {
    expect(answer("organization", email({ accountId: "acct-1" }))).toBe("Acme Ltd");
    expect(answer("organization", draft({ accountId: "acct-1" }))).toBe("Acme Ltd");
    expect(answer("organization")).toBe("No organization");
    expect(answer("organization", email())).toBeUndefined();
  });

  it("leaves File to the component, which shows the filed name once the file is made", () => {
    expect(answer("file", draft({ name: "Plan" }))).toBeUndefined();
  });
});

describe("pathOf", () => {
  const path = (d: MakeDraft, f: MakeFacts = facts()) => pathOf(d, f);

  it("is blank for a document with no description, nothing picked and no template", () => {
    expect(path(draft())).toBe("blank");
    for (const format of ["markdown", "html", "txt", "csv", "json", "pdf"] as const) expect(path(makeDoc(format))).toBe("blank");
  });

  it("is blank whatever the name, the folder or the organization", () => {
    expect(path(draft({ name: "Plan", folderId: "f-projects", accountId: "acct-1" }))).toBe("blank");
  });

  // Only a document can start blank. (The floor asks for something before it consults the path
  // for these, but the path is the model's to get right on its own.)
  it("is composed for a package or an email template, which cannot start blank", () => {
    expect(path(staticSite())).toBe("composed");
    expect(path(email())).toBe("composed");
  });

  it("is described for Markdown with a description and nothing picked", () => {
    expect(path(draft({ statement: "A summary" }))).toBe("described");
  });

  it("is described when every picked file is one the Library can attach", () => {
    expect(path(draft({ statement: "A summary", ingredients: [NOTES, LOGO] }))).toBe("described");
  });

  it("is composed when a CSV comes along, however many Markdown files and pictures do too", () => {
    expect(path(draft({ statement: "A summary", ingredients: [NOTES, LOGO, DATA] }))).toBe("composed");
    expect(path(draft({ statement: "A summary", ingredients: [DATA] }))).toBe("composed");
  });

  it("is composed for a Markdown file past 32 KiB, an archived one and one the window has not read", () => {
    expect(path(draft({ statement: "A summary", ingredients: [pick("file-big", "minutes.md")] }))).toBe("composed");
    expect(path(draft({ statement: "A summary", ingredients: [pick("file-old", "old.md")] }))).toBe("composed");
    expect(path(draft({ statement: "A summary", ingredients: [pick("file-unread", "later.md")] }))).toBe("composed");
    expect(path(draft({ statement: "A summary", ingredients: [pick("file-gone", "gone.md")] }))).toBe("composed");
  });

  it("is composed for a record type or a row, which the Library cannot attach", () => {
    expect(path(draft({ statement: "A summary", ingredients: [{ kind: "query", ref: "query openInvoices()", label: "Open invoices" }] }))).toBe("composed");
    expect(path(draft({ statement: "A summary", ingredients: [NOTES, { kind: "concept_row", ref: "v1:x:invoice#a", label: "INV-1001" }] }))).toBe("composed");
  });

  it("is composed when something is picked and nothing is described", () => {
    expect(path(draft({ ingredients: [NOTES] }))).toBe("composed");
  });

  it("is composed with a template, even for a description alone, and for any other kind", () => {
    expect(path(draft({ templateId: "t1" }))).toBe("composed");
    expect(path(draft({ templateId: "t1", statement: "A summary" }))).toBe("composed");
    expect(path(makeDoc("html", { statement: "A summary" }))).toBe("composed");
    expect(path(staticSite({ statement: "A summary" }))).toBe("composed");
    expect(path(email({ statement: "A summary" }))).toBe("composed");
  });

  // The Library's rule is the SDK's to say (isDescribedMarkdown), so the model holds no list of
  // its own: a WebP picture is attached by the Library though the tray does not offer one.
  it("leaves the Library's rule to the SDK", () => {
    const lookup: LibraryFileLookup = (id) => ({
      "f-webp": { name: "photo.webp", size: 1 * MIB, archived: false },
      "f-upper": { name: "NOTES.MD", size: 2 * KIB, archived: false },
      "f-jpg": { name: "scan.jpg", size: 2 * MIB, archived: false },
      "f-huge": { name: "scan.png", size: 9 * MIB, archived: false },
      "f-empty": { name: "empty.md", size: 0, archived: false },
      "f-nosize": { name: "notes.md", size: null, archived: false },
    } as Record<string, LibraryFileFacts>)[id];
    const described = (ref: string, label: string) => path(draft({ statement: "A summary", ingredients: [pick(ref, label)] }), facts({ files: lookup }));
    expect(described("f-webp", "photo.webp")).toBe("described");
    expect(described("f-upper", "NOTES.MD")).toBe("described");
    expect(described("f-jpg", "scan.jpg")).toBe("described");
    expect(described("f-nosize", "notes.md")).toBe("described");
    expect(described("f-huge", "scan.png")).toBe("composed");
    expect(described("f-empty", "empty.md")).toBe("composed");
  });
});

describe("blocker", () => {
  const block = (d: MakeDraft, f: MakeFacts = facts()) => blocker(d, f);

  it("asks for a name first, whatever else is missing", () => {
    expect(block(draft())).toEqual({ reason: "name", words: { word: "Name the file" } });
    expect(block(makeDoc("pdf", { name: "a/b" }), facts({ inference: false }))).toEqual({ reason: "name", words: { word: "Name the file" } });
    expect(block(email({ name: ".json" }))).toEqual({ reason: "name", words: { word: "Name the file" } });
  });

  it("says a blank file can be Markdown, text, HTML, CSV or JSON, for a kind that cannot start blank", () => {
    const words = { word: "Describe it or pick what to make it from", detail: "A blank file can be Markdown, text, HTML, CSV or JSON." };
    expect(block(makeDoc("pdf", { name: "Report" }))).toEqual({ reason: "blank", words });
    expect(block(makeDoc("docx", { name: "Report" }))).toEqual({ reason: "blank", words });
    expect(block(staticSite({ name: "Site" }))).toEqual({ reason: "blank", words });
    expect(block(email({ name: "Welcome", accountId: "acct-1" }))).toEqual({ reason: "blank", words });
  });

  it("lets Markdown, text, HTML, CSV and JSON start blank, and no other kind", () => {
    for (const format of ["markdown", "txt", "html", "csv", "json"] as const) expect(block(makeDoc(format, { name: "Notes" }))).toBeNull();
    for (const format of ["docx", "pdf"] as const) expect(block(makeDoc(format, { name: "Notes" }))?.reason).toBe("blank");
  });

  it("says a template needs something to fill it, whatever the kind", () => {
    const words = { word: "Describe it or pick what to make it from", detail: "A template needs something to fill it." };
    expect(block(draft({ name: "Plan", templateId: "t1" }))).toEqual({ reason: "blank", words });
    expect(block(makeDoc("pdf", { name: "Report", templateId: "t1" }))).toEqual({ reason: "blank", words });
  });

  it("lets a template have a description or something picked to fill it", () => {
    expect(block(draft({ name: "Plan", templateId: "t1", statement: "A summary" }))).toBeNull();
    expect(block(draft({ name: "Plan", templateId: "t1", ingredients: [NOTES] }))).toBeNull();
  });

  it("is satisfied by a description or by anything picked, for any kind", () => {
    expect(block(makeDoc("pdf", { name: "Report", statement: "A summary" }))).toBeNull();
    expect(block(makeDoc("pdf", { name: "Report", ingredients: [DATA] }))).toBeNull();
    expect(block(staticSite({ name: "Site", statement: "A landing page" }))).toBeNull();
  });

  it("does not count a description of only spaces", () => {
    expect(block(makeDoc("pdf", { name: "Report", statement: "  \n  " }))?.reason).toBe("blank");
  });

  it("asks an email template for an organization", () => {
    expect(block(email({ name: "Welcome", statement: "A welcome email" }))).toEqual({ reason: "organization", words: { word: "Choose an organization" } });
    expect(block(email({ name: "Welcome", statement: "A welcome email", accountId: "acct-1" }))).toBeNull();
  });

  it("says Making this needs Inference only when readiness reports it not set up and a model is needed", () => {
    const needs = { reason: "inference", words: { word: "Making this needs Inference" } };
    const described = draft({ name: "Plan", statement: "A summary" });
    expect(block(described, facts({ inference: false }))).toEqual(needs);
    expect(block(draft({ name: "Plan", statement: "A summary", ingredients: [DATA] }), facts({ inference: false }))).toEqual(needs);
    expect(block(described, facts({ inference: true }))).toBeNull();
    // Not yet known does not block.
    expect(block(described, facts({ inference: null }))).toBeNull();
  });

  it("lets a blank file be made on a cluster with no Inference", () => {
    expect(block(draft({ name: "Plan" }), facts({ inference: false }))).toBeNull();
  });

  it("takes the first that matches, in the spec's order", () => {
    const pdf = makeDoc("pdf");
    expect(block(pdf, facts({ inference: false }))?.reason).toBe("name");
    expect(block({ ...pdf, name: "Report" }, facts({ inference: false }))?.reason).toBe("blank");
    expect(block(email({ name: "Welcome", statement: "A welcome email" }), facts({ inference: false }))?.reason).toBe("organization");
    expect(block(email({ name: "Welcome", statement: "A welcome email", accountId: "acct-1" }), facts({ inference: false }))?.reason).toBe("inference");
  });
});

describe("readyWords", () => {
  it("says what the file will be named and that a blank one starts blank", () => {
    expect(readyWords(draft({ name: "Meeting notes" }), facts())).toEqual({ word: "Ready to make Meeting notes.md", detail: "It starts blank." });
  });

  it("says a described file is reviewed in the editor", () => {
    expect(readyWords(draft({ name: "Plan", statement: "A summary", ingredients: [NOTES, LOGO] }), facts())).toEqual({
      word: "Ready to make Plan.md", detail: "You review its draft in the editor.",
    });
  });

  it("says a model writes anything else, a CSV among the picked files included", () => {
    expect(readyWords(draft({ name: "Plan", statement: "A summary", ingredients: [NOTES, LOGO, DATA] }), facts())).toEqual({
      word: "Ready to make Plan.md", detail: "A model writes it.",
    });
    expect(readyWords(staticSite({ name: "Site", statement: "A landing page" }), facts())).toEqual({ word: "Ready to make Site.zip", detail: "A model writes it." });
  });

  it("says an email template is filed as .email.json", () => {
    const welcome = email({ name: "Welcome", statement: "A welcome email", accountId: "acct-1" });
    expect(readyWords(welcome, facts())).toEqual({ word: "Ready to make Welcome.email.json", detail: "A model writes it." });
  });
});

// The brief's own case, varied one way at a time: a description with notes.md (2 KiB) and
// logo.png, both read, is reviewed in the editor; change one thing about it and a model writes it.
describe("a described Markdown file with notes.md and logo.png, and then one thing different", () => {
  const base = draft({ name: "Plan", statement: "A summary", ingredients: [NOTES, LOGO] });
  const query: Ingredient = { kind: "query", ref: "query openInvoices()", label: "Open invoices" };
  const writtenByAModel = [
    ["a CSV added", [NOTES, LOGO, DATA]],
    ["a Markdown file of 40 KiB", [pick("file-big", "minutes.md"), LOGO]],
    ["an archived Markdown file", [pick("file-old", "old.md"), LOGO]],
    ["a file the window has not read", [NOTES, LOGO, pick("file-unread", "later.md")]],
    ["a record type added", [NOTES, LOGO, query]],
  ] as const;

  it("is reviewed in the editor, and both files are sent with content: true", () => {
    expect(pathOf(base, facts())).toBe("described");
    expect(readyWords(base, facts())).toEqual({ word: "Ready to make Plan.md", detail: "You review its draft in the editor." });
    expect(requestOf(base).sources).toStrictEqual([
      { kind: "library_file", ref: "file-notes", label: "notes.md", content: true },
      { kind: "library_file", ref: "file-logo", label: "logo.png", content: true },
    ]);
  });

  it.each(writtenByAModel)("is written by a model with %s", (_what, ingredients) => {
    const changed = { ...base, ingredients: [...ingredients] };
    expect(pathOf(changed, facts())).toBe("composed");
    expect(readyWords(changed, facts())).toEqual({ word: "Ready to make Plan.md", detail: "A model writes it." });
  });

  it("sends a Library file with content: true whichever way the file goes", () => {
    for (const [, ingredients] of writtenByAModel) {
      const sent = requestOf({ ...base, ingredients: [...ingredients] }).sources ?? [];
      expect(sent.filter((source) => source.kind === "library_file").every((source) => source.content === true)).toBe(true);
      expect(sent.filter((source) => source.kind !== "library_file").some((source) => "content" in source)).toBe(false);
    }
  });
});

describe("floorActs", () => {
  const acts = (d: MakeDraft, open: MakeStepId, over: { f?: MakeFacts; making?: boolean; canSetUpInference?: boolean } = {}) =>
    floorActs({ draft: d, facts: over.f ?? facts(), open, making: over.making ?? false, canSetUpInference: over.canSetUpInference ?? false });

  it("offers Stop and Leave while the file is being made, whatever the draft", () => {
    expect(acts(draft(), "file", { making: true })).toEqual(["stop", "leave"]);
    expect(acts(draft({ name: "Plan" }), "name", { making: true })).toEqual(["stop", "leave"]);
  });

  it("offers Make the file the moment the floor says Ready, from any step", () => {
    const ready = draft({ name: "Meeting notes" });
    for (const step of stepsOf(ready, facts())) expect(acts(ready, step)).toEqual(["cancel", "make"]);
  });

  it("walks the plus with Next while a later step waits, and then offers nothing more", () => {
    const plus = draftFromSetup(null, "markdown");
    expect(acts(plus, "from")).toEqual(["cancel", "next"]);
    expect(acts(plus, "describe")).toEqual(["cancel", "next"]);
    expect(acts(plus, "name")).toEqual(["cancel"]);
    expect(acts({ ...plus, name: "Meeting notes" }, "name")).toEqual(["cancel", "make"]);
  });

  it("offers a sender's setup only Cancel until the name is usable", () => {
    const files = draftFromSetup({ format: "markdown", folderId: "f-projects" }, "markdown");
    expect(acts(files, "name")).toEqual(["cancel"]);
    expect(acts({ ...files, name: "Meeting notes" }, "name")).toEqual(["cancel", "make"]);
  });

  it("offers Next from an earlier step even when only the name is left", () => {
    const files = draftFromSetup({ format: "markdown" }, "markdown");
    expect(acts(files, "kind")).toEqual(["cancel", "next"]);
  });

  // From still waits here, so from the first step Next is legal as well; Set up Inference
  // comes ahead of it for those who can reach it, and Next stays for those who cannot.
  it("offers Set up Inference to those who can reach it, ahead of Next", () => {
    const described = draft({ name: "Plan", statement: "A summary" });
    const none = facts({ inference: false });
    expect(acts(described, "kind", { f: none, canSetUpInference: true })).toEqual(["cancel", "setUpInference"]);
    expect(acts(described, "kind", { f: none, canSetUpInference: false })).toEqual(["cancel", "next"]);
  });

  it("offers nothing past Cancel when Inference is missing, nobody here can set it up, and no later step waits", () => {
    const described = draft({ name: "Plan", statement: "A summary" });
    const none = facts({ inference: false });
    expect(acts(described, "describe", { f: none, canSetUpInference: true })).toEqual(["cancel", "setUpInference"]);
    expect(acts(described, "describe", { f: none, canSetUpInference: false })).toEqual(["cancel"]);
  });

  it("does not offer Set up Inference for anything but the Inference blocker", () => {
    expect(acts(draft(), "name", { canSetUpInference: true })).toEqual(["cancel"]);
  });

  // Across every kind, answer and place the wizard can be in: the bar holds Cancel and one
  // forward act, and the forward act is Make the file only when nothing is missing.
  it("never offers more than two acts, nor Make the file while something is missing", () => {
    const statements = ["", "Summarise Q3"];
    const picks: Ingredient[][] = [[], [NOTES], [DATA], [{ kind: "query", ref: "query openInvoices()", label: "Open invoices" }]];
    const templates = ["", "t1"];
    const nameChoices = ["", "Plan", "a/b"];
    const accounts = ["", "acct-1"];
    const inferences: (boolean | null)[] = [null, true, false];
    const passes = [{ from: false, describe: false }, { from: true, describe: true }];
    const open: MakeStepId[] = ["kind", "template", "from", "describe", "name", "folder", "organization", "file"];
    // Collected and asserted once: a hundred thousand cases are not a hundred thousand expectations.
    const violations: string[] = [];
    let checked = 0;
    for (const entry of FILE_KINDS) for (const statement of statements) for (const ingredients of picks) for (const templateId of templates)
      for (const name of nameChoices) for (const accountId of accounts) for (const inference of inferences) for (const passed of passes) {
        const d: MakeDraft = { fileKind: entry.kind, templateId, ingredients, statement, name, folderId: "", accountId, passed };
        const f = facts({ inference, templates: [template("t1", "Board pack")] });
        const missing = blocker(d, f);
        for (const step of open) for (const canSetUpInference of [true, false]) {
          const given = floorActs({ draft: d, facts: f, open: step, making: false, canSetUpInference });
          checked++;
          // Built only for a violation: it is the report, not part of the check.
          const where = () => `${entry.label} / ${JSON.stringify({ statement, ingredients: ingredients.length, templateId, name, accountId, inference, passed })} / open ${step} / canSetUp ${canSetUpInference}`;
          if (given.length > 2) violations.push(`${where()}: ${given.length} acts`);
          if (given[0] !== "cancel") violations.push(`${where()}: does not begin with cancel`);
          if (given.includes("make") !== (missing === null)) violations.push(`${where()}: make is ${given.includes("make")} while the blocker is ${missing?.reason ?? "none"}`);
          if (given.includes("setUpInference") !== (missing?.reason === "inference" && canSetUpInference)) violations.push(`${where()}: setUpInference is ${given.includes("setUpInference")}`);
          if (given.includes("stop") || given.includes("leave")) violations.push(`${where()}: offers a making act`);
        }
      }
    expect(violations).toEqual([]);
    expect(checked).toBeGreaterThan(10000);
  });
});

describe("requestOf", () => {
  it("sends only the name and the format for a blank Markdown file", () => {
    expect(requestOf(draft({ name: "Notes" }))).toStrictEqual({ name: "Notes", format: "markdown" });
  });

  it("trims the name and the description, and sends neither when there is nothing to send", () => {
    expect(requestOf(draft({ name: "  Notes  ", statement: "   " }))).toStrictEqual({ name: "Notes", format: "markdown" });
    expect(requestOf(draft({ name: "Notes", statement: "  A summary of Q3\n" }))).toStrictEqual({ name: "Notes", format: "markdown", statement: "A summary of Q3" });
  });

  it("sends the name as typed, for the server to drop one trailing extension", () => {
    expect(requestOf(makeDoc("pdf", { name: "Report.pdf" }))).toStrictEqual({ name: "Report.pdf", format: "pdf" });
  });

  it("sends every answered optional, and no other", () => {
    expect(requestOf(draft({ name: "Plan", statement: "A summary", templateId: "t1", folderId: "f-projects", accountId: "acct-1" }))).toStrictEqual({
      name: "Plan", format: "markdown", statement: "A summary", templateId: "t1", folderId: "f-projects", accountIds: ["acct-1"],
    });
  });

  it("leaves out the folder at the top level and the organization when there is none", () => {
    const sent = requestOf(draft({ name: "Plan", folderId: "", accountId: "" }));
    expect(Object.keys(sent)).toEqual(["name", "format"]);
  });

  it("sends a described file's Markdown and picture with content: true, as the cluster gave their ids", () => {
    expect(requestOf(draft({ name: "Plan", statement: "A summary", ingredients: [NOTES, LOGO] }))).toStrictEqual({
      name: "Plan", format: "markdown", statement: "A summary",
      sources: [
        { kind: "library_file", ref: "file-notes", label: "notes.md", content: true },
        { kind: "library_file", ref: "file-logo", label: "logo.png", content: true },
      ],
    });
  });

  it("sends content: true on every Library file whichever path the file takes, and on nothing else", () => {
    const query = { kind: "query" as const, ref: "query openInvoices()", label: "Open invoices" };
    const row = { kind: "concept_row" as const, ref: "v1:x:invoice#a", label: "INV-1001" };
    expect(requestOf(draft({ name: "Plan", statement: "A summary", ingredients: [NOTES, DATA, query, row] })).sources).toStrictEqual([
      { kind: "library_file", ref: "file-notes", label: "notes.md", content: true },
      { kind: "library_file", ref: "file-data", label: "data.csv", content: true },
      { kind: "query", ref: "query openInvoices()", label: "Open invoices" },
      { kind: "concept_row", ref: "v1:x:invoice#a", label: "INV-1001" },
    ]);
    expect(requestOf(makeDoc("pdf", { name: "Report", ingredients: [DATA] })).sources).toStrictEqual([{ kind: "library_file", ref: "file-data", label: "data.csv", content: true }]);
  });

  it("sends a static package as HTML with its deployable kind", () => {
    expect(requestOf(staticSite({ name: "Site", statement: "A landing page" }))).toStrictEqual({
      name: "Site", format: "html", statement: "A landing page", deployableKind: "static",
    });
    expect(requestOf(draft({ fileKind: { kind: "package", deployableKind: "shopify_storefront" }, name: "Shop" }))).toMatchObject({ format: "html", deployableKind: "shopify_storefront" });
  });

  it("sends an email template as JSON with its output kind and its organization", () => {
    expect(requestOf(email({ name: "Welcome", statement: "A welcome email", accountId: "acct-1" }))).toStrictEqual({
      name: "Welcome", format: "json", statement: "A welcome email", outputKind: "email_template", accountIds: ["acct-1"],
    });
  });

  it("sends what a Start over drafted from the record", () => {
    const record = composition({ name: "Q3 summary", statement: "A summary", format: "markdown", folderId: "f-projects" });
    expect(requestOf(draftFromComposition(record, [recorded("library_file", "file-notes", "notes.md")]))).toStrictEqual({
      name: "Q3 summary", format: "markdown", statement: "A summary", folderId: "f-projects",
      sources: [{ kind: "library_file", ref: "file-notes", label: "notes.md", content: true }],
    });
  });

  it("is the same request for the same draft, key for key, so the ask keeps its id", () => {
    const d = draft({ name: "Plan", statement: "A summary", templateId: "t1", folderId: "f-projects", accountId: "acct-1", ingredients: [NOTES] });
    expect(JSON.stringify(requestOf(d))).toBe(JSON.stringify(requestOf({ ...d })));
  });
});

describe("filedName", () => {
  const filed = (name: string, kind: FileKind = { kind: "document", format: "markdown" }) => filedName(draft({ name, fileKind: kind }));

  it("adds the extension of the kind, md for Markdown", () => {
    expect(filed("Plan")).toBe("Plan.md");
    expect(filed("Todo", { kind: "document", format: "txt" })).toBe("Todo.txt");
    expect(filed("Orders", { kind: "document", format: "csv" })).toBe("Orders.csv");
    expect(filed("Landing", { kind: "document", format: "html" })).toBe("Landing.html");
    expect(filed("Settings", { kind: "document", format: "json" })).toBe("Settings.json");
    expect(filed("Letter", { kind: "document", format: "docx" })).toBe("Letter.docx");
    expect(filed("Report", { kind: "document", format: "pdf" })).toBe("Report.pdf");
    expect(filed("Welcome", { kind: "emailTemplate" })).toBe("Welcome.email.json");
  });

  it("does not add a second extension when the name already ends with the kind's", () => {
    expect(filed("Plan.md")).toBe("Plan.md");
    expect(filed("Plan.MD")).toBe("Plan.md");
    expect(filed("Plan.markdown")).toBe("Plan.md");
    expect(filed("Report.pdf", { kind: "document", format: "pdf" })).toBe("Report.pdf");
  });

  it("keeps another format's extension as part of the name", () => {
    expect(filed("Report.pdf")).toBe("Report.pdf.md");
    expect(filed("Plan.md.md")).toBe("Plan.md");
  });

  // The engine files an email template as <stem>.email.json (the filing site in
  // integrations/compose/materialize.go): it makes the .json file name as it does for any JSON
  // document, takes a ".json" off it, case-sensitively, and adds ".email.json".
  it("files an email template as .email.json", () => {
    const email = { kind: "emailTemplate" } as const;
    expect(filed("Welcome", email)).toBe("Welcome.email.json");
    expect(filed("Welcome.json", email)).toBe("Welcome.email.json");
    expect(filed("Welcome.JSON", email)).toBe("Welcome.email.json");
    expect(filed("Mixed.Json", email)).toBe("Mixed.email.json");
    expect(filed("Plan.md.json", email)).toBe("Plan.md.email.json");
  });

  it("takes only a lower-case .json off an email template's stem, as the engine does", () => {
    const email = { kind: "emailTemplate" } as const;
    // The name's own ".JSON" is cleaned away first, and the one before it stays: not lower case.
    expect(filed("a.JSON.JSON", email)).toBe("a.JSON.email.json");
    // After the cleaning, a lower-case .json is still on the stem, and it goes too.
    expect(filed("x.json.JSON", email)).toBe("x.email.json");
    // A person who knows the engine's suffix still gets it added.
    expect(filed("Welcome.email", email)).toBe("Welcome.email.email.json");
    expect(filed("Welcome.email.json", email)).toBe("Welcome.email.email.json");
  });

  it("files a package as a ZIP", () => {
    expect(filed("Site", { kind: "package", deployableKind: "static" })).toBe("Site.zip");
    expect(filed("Site.zip", { kind: "package", deployableKind: "static" })).toBe("Site.zip");
    expect(filed("Site.ZIP", { kind: "package", deployableKind: "spa" })).toBe("Site.ZIP");
    expect(filed("Site.html", { kind: "package", deployableKind: "static" })).toBe("Site.zip");
    expect(filedName(staticSite({ name: "Site" }))).toBe("Site.zip");
  });

  it("trims the name", () => {
    expect(filed("  Plan  ")).toBe("Plan.md");
  });

  // The cluster files the stem sanitised (sanitiseFileStem): dots at either end go, and no more
  // than 120 bytes are kept.
  it("names the file as the cluster files it, not as it was typed, where those differ", () => {
    expect(filed("Notes.")).toBe("Notes.md");
    expect(filed(".hidden")).toBe("hidden.md");
    expect(filed("...")).toBe("composition.md");
    expect(filed("a".repeat(130))).toBe(`${"a".repeat(120)}.md`);
    expect(filed("é".repeat(100))).toBe(`${"é".repeat(60)}.md`);
  });
});

describe("ingredientsPhrase", () => {
  const phrase = (...labels: string[]) => ingredientsPhrase(labels.map((label, index) => pick(`f${index}`, label)));

  it("says nothing is picked", () => {
    expect(ingredientsPhrase([])).toBe("Nothing picked");
  });

  it("names one, joins two with and, and counts the rest from the third", () => {
    expect(phrase("notes.md")).toBe("notes.md");
    expect(phrase("notes.md", "Orders")).toBe("notes.md and Orders");
    expect(phrase("A", "B", "C")).toBe("A, B and 1 more");
    expect(phrase("A", "B", "C", "D", "E")).toBe("A, B and 3 more");
  });
});

describe("describeAnswer", () => {
  it("says Not described for nothing, or for only spaces", () => {
    expect(describeAnswer("")).toBe("Not described");
    expect(describeAnswer("  \n  ")).toBe("Not described");
  });

  it("gives a short first line as it is", () => {
    expect(describeAnswer("A one-page summary of the Q3 results for the board")).toBe("A one-page summary of the Q3 results for the board");
    expect(describeAnswer("word ".repeat(11) + "words")).toBe("word ".repeat(11) + "words");
  });

  it("gives only the first line", () => {
    expect(describeAnswer("Summarise Q3\nand mention churn")).toBe("Summarise Q3");
    expect(describeAnswer("Summarise Q3\r\nand mention churn")).toBe("Summarise Q3");
    expect(describeAnswer("\n\n  Summarise Q3  \nand mention churn")).toBe("Summarise Q3");
  });

  // A text area keeps the characters it was pasted: the Unicode line and paragraph separators end a line too.
  it("ends the first line at a Unicode line or paragraph separator", () => {
    expect(describeAnswer("Summarise Q3\u2028and mention churn")).toBe("Summarise Q3");
    expect(describeAnswer("Summarise Q3\u2029and mention churn")).toBe("Summarise Q3");
    expect(describeAnswer("\u2028Summarise Q3\u2029and mention churn")).toBe("Summarise Q3");
  });

  it("cuts a long line at a word within 60 characters and says it was cut", () => {
    expect(describeAnswer("Draft a one-page summary of the Q3 results for the board, covering revenue, churn and hiring"))
      .toBe("Draft a one-page summary of the Q3 results for the board,…");
    // The window ends on a space, so the last whole word is the one before it.
    expect(describeAnswer("Summarise the quarterly results for the board and highlight the three biggest risks"))
      .toBe("Summarise the quarterly results for the board and highlight…");
  });

  it("keeps a word that ends exactly at the 60th character", () => {
    expect(describeAnswer("word ".repeat(11) + "words more text")).toBe("word ".repeat(11) + "words…");
  });

  it("cuts a line with no spaces at 60 characters, never inside a character", () => {
    expect(describeAnswer("x".repeat(70))).toBe(`${"x".repeat(60)}…`);
    expect(describeAnswer("😀".repeat(70))).toBe(`${"😀".repeat(60)}…`);
  });
});

describe("readableReference", () => {
  it.each([".md", ".markdown", ".txt", ".html", ".htm", ".css", ".json", ".csv", ".svg", ".png", ".jpg", ".jpeg", ".gif", ".zip"])(
    "takes %s, which the composer reads",
    (extension) => {
      expect(readableReference(`file${extension}`)).toBe(true);
      expect(readableReference(`file${extension.toUpperCase()}`)).toBe(true);
    },
  );

  it.each(["report.pdf", "letter.docx", "sheet.xlsx", "photo.webp", "song.mp3", "archive.tar.gz", "notes.md.bak", "README", "notes.", ""])(
    "does not take %j, which the composer refuses",
    (name) => {
      expect(readableReference(name)).toBe(false);
    },
  );

  it("reads the extension of the last name in a path, and a name that is only an extension", () => {
    expect(readableReference("folder.md/readme")).toBe(false);
    expect(readableReference("folder/readme.md")).toBe(true);
    expect(readableReference(".md")).toBe(true);
  });

  // Go lower-cases a capital dotted I to an ordinary i; JavaScript adds a combining dot.
  it("reads an extension as the composer's Go does", () => {
    expect(readableReference("photo.G\u0130F")).toBe(true);
  });
});

describe("templatesFor and keepUsableTemplate", () => {
  const board = template("t1", "Board pack", "markdown");
  const house = template("t2", "House style", "");
  const page = template("t3", "Landing", "html");
  const old = template("t4", "Old pack", "markdown", { archived: true });
  const all = facts({ templates: [board, house, page, old] });

  it("lists the live templates that make the kind: its format, or none named", () => {
    expect(templatesFor(draft(), all).map((t) => t.id)).toEqual(["t1", "t2"]);
    expect(templatesFor(makeDoc("html"), all).map((t) => t.id)).toEqual(["t2", "t3"]);
    expect(templatesFor(staticSite(), all).map((t) => t.id)).toEqual(["t2", "t3"]);
    expect(templatesFor(makeDoc("pdf"), all).map((t) => t.id)).toEqual(["t2"]);
  });

  it("lets go of a template the kind cannot use, and keeps one it can", () => {
    expect(keepUsableTemplate(makeDoc("html", { templateId: "t1" }), all).templateId).toBe("");
    expect(keepUsableTemplate(draft({ templateId: "t4" }), all).templateId).toBe("");
    expect(keepUsableTemplate(draft({ templateId: "t-missing" }), all).templateId).toBe("");
    expect(keepUsableTemplate(draft({ templateId: "t1" }), all).templateId).toBe("t1");
    expect(keepUsableTemplate(makeDoc("html", { templateId: "t2" }), all).templateId).toBe("t2");
  });

  it("returns the same draft when there is nothing to let go", () => {
    const d = draft({ templateId: "t1" });
    expect(keepUsableTemplate(d, all)).toBe(d);
    const none = draft();
    expect(keepUsableTemplate(none, all)).toBe(none);
  });

  it("changes only the template", () => {
    const d = makeDoc("html", { templateId: "t1", name: "Plan", statement: "A summary", ingredients: [NOTES] });
    expect(keepUsableTemplate(d, all)).toEqual({ ...d, templateId: "" });
  });

  // A feed that has not settled, or that is between two reads, lists nothing; no template can be
  // judged unusable from that, and a valid choice must not be lost to it.
  it("keeps the choice while the templates are not loaded", () => {
    const unsettled = facts({ templates: [], templatesLoaded: false });
    const chosen = draft({ templateId: "t1" });
    expect(keepUsableTemplate(chosen, unsettled)).toBe(chosen);
    expect(keepUsableTemplate(makeDoc("html", { templateId: "t1" }), unsettled).templateId).toBe("t1");
    // Loaded, and listing nothing that makes the kind, it is let go.
    expect(keepUsableTemplate(chosen, facts({ templates: [], templatesLoaded: true })).templateId).toBe("");
  });

  it("does not treat a partly listed feed as loaded", () => {
    expect(keepUsableTemplate(draft({ templateId: "t1" }), facts({ templates: [house], templatesLoaded: false })).templateId).toBe("t1");
  });
});

// A kind changes only through withKind. requestOf holds no facts, so it sends whatever template the
// draft holds, and the Template step shows only the templates that make the kind: a kind change that
// left the template behind would send one from a step that is no longer there.
describe("withKind", () => {
  const board = template("t1", "Board pack", "markdown");
  const house = template("t2", "House style", "");
  const loaded = facts({ templates: [board, house] });
  const PDF: FileKind = { kind: "document", format: "pdf" };

  // The only template there is makes Markdown: a PDF has none to offer, so the step goes.
  const onlyBoard = facts({ templates: [board] });

  it("takes a template away everywhere when the new kind cannot use it", () => {
    const chosen = draft({ name: "Report", statement: "A summary", templateId: "t1" });
    // Before: the step is there, the rail names the template, the request sends it.
    expect(stepsOf(chosen, onlyBoard)).toContain("template");
    expect(answerOf("template", chosen, names)).toBe("Board pack");
    expect(requestOf(chosen).templateId).toBe("t1");

    const changed = withKind(chosen, PDF, onlyBoard);
    // After: no step, no answer, nothing sent.
    expect(changed.fileKind).toEqual(PDF);
    expect(changed.templateId).toBe("");
    expect(stepsOf(changed, onlyBoard)).not.toContain("template");
    expect(answerOf("template", changed, names)).toBe("No template");
    expect(requestOf(changed)).toStrictEqual({ name: "Report", format: "pdf", statement: "A summary" });
    // Left to the caller, the draft is as the review found it: the step gone and the template still sent.
    const careless = { ...chosen, fileKind: PDF };
    expect(stepsOf(careless, onlyBoard)).not.toContain("template");
    expect(requestOf(careless).templateId).toBe("t1");
  });

  it("stops the floor speaking of a template the rail no longer shows", () => {
    const needsSomething = draft({ name: "Report", templateId: "t1" });
    expect(blocker(needsSomething, onlyBoard)?.words.detail).toBe("A template needs something to fill it.");
    expect(blocker(withKind(needsSomething, PDF, onlyBoard), onlyBoard)?.words.detail).toBe("A blank file can be Markdown, text, HTML, CSV or JSON.");
  });

  it("keeps a template the new kind can use", () => {
    expect(withKind(draft({ templateId: "t2" }), PDF, loaded).templateId).toBe("t2");
    const page = template("t3", "Landing", "html");
    const withPage = facts({ templates: [page] });
    expect(withKind(makeDoc("html", { templateId: "t3" }), { kind: "package", deployableKind: "static" }, withPage).templateId).toBe("t3");
    expect(withKind(makeDoc("html", { templateId: "t3" }), { kind: "document", format: "txt" }, withPage).templateId).toBe("");
  });

  it("keeps the choice while the templates are not loaded", () => {
    const unsettled = facts({ templates: [], templatesLoaded: false });
    expect(withKind(draft({ templateId: "t1" }), PDF, unsettled).templateId).toBe("t1");
    // Loaded, the same change lets it go.
    expect(withKind(draft({ templateId: "t1" }), PDF, onlyBoard).templateId).toBe("");
  });

  it("drops the organization an email template carried when it becomes an ordinary file", () => {
    const welcome = email({ name: "Welcome", statement: "A welcome email", accountId: "acct-1" });
    expect(stepsOf(welcome, loaded)).toContain("organization");
    expect(requestOf(welcome).accountIds).toEqual(["acct-1"]);

    for (const ordinary of [PDF, { kind: "document", format: "markdown" }, { kind: "package", deployableKind: "static" }] as const) {
      const changed = withKind(welcome, ordinary, loaded);
      expect(changed.accountId).toBe("");
      expect(stepsOf(changed, loaded)).not.toContain("organization");
      expect(requestOf(changed)).not.toHaveProperty("accountIds");
      expect(requestOf(changed)).not.toHaveProperty("outputKind");
    }
  });

  it("keeps the organization of an ordinary file that already carried one, as an older record's does", () => {
    const older = draft({ name: "Q3 summary", accountId: "acct-1" });
    expect(withKind(older, PDF, loaded).accountId).toBe("acct-1");
    expect(withKind(older, { kind: "package", deployableKind: "spa" }, loaded).accountId).toBe("acct-1");
    // And an email template that stays one keeps its own.
    const welcome = email({ accountId: "acct-1" });
    expect(withKind(welcome, { kind: "emailTemplate" }, loaded).accountId).toBe("acct-1");
  });

  it("changes nothing else the person has answered", () => {
    const answered = draft({ name: "Plan", statement: "A summary", ingredients: [NOTES], folderId: "f-projects", templateId: "t1", passed: { from: true, describe: true } });
    expect(withKind(answered, PDF, loaded)).toEqual({ ...answered, fileKind: PDF, templateId: "" });
  });
});

// The spec's own walk, 2.3: from the plus, then from Files.
describe("the floor, walked as the spec walks it", () => {
  it("from the plus, From is open and the bar reads Name the file with Cancel and Next", () => {
    const plus = draftFromSetup(null, "markdown");
    expect(openingStep(plus, facts())).toBe("from");
    expect(blocker(plus, facts())?.words.word).toBe("Name the file");
    expect(floorActs({ draft: plus, facts: facts(), open: "from", making: false, canSetUpInference: false })).toEqual(["cancel", "next"]);
  });

  it("from Files, typing a usable name turns the bar into Ready to make Meeting notes.md, It starts blank.", () => {
    const sent = draftFromSetup({ format: "markdown", folderId: "f-projects", returnTo: { appId: "files", sectionId: "browse" } }, "markdown");
    expect(openingStep(sent, facts())).toBe("name");
    const named = { ...sent, name: "Meeting notes" };
    expect(blocker(named, facts())).toBeNull();
    expect(readyWords(named, facts())).toEqual({ word: "Ready to make Meeting notes.md", detail: "It starts blank." });
    expect(floorActs({ draft: named, facts: facts(), open: "name", making: false, canSetUpInference: false })).toEqual(["cancel", "make"]);
    expect(requestOf(named)).toStrictEqual({ name: "Meeting notes", format: "markdown", folderId: "f-projects" });
  });
});
