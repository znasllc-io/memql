// How another app opens the Materializer already pointed at a file to make
// (design: docs/superpowers/specs/2026-10-11-materializer-front-door-design.md,
// section 4).
//
// A SENDER IMPORTS THIS FILE AND NOTHING FROM apps/materializer/. The names a
// sender and the app must agree on -- the app id, the section a window opens
// on, the shape of the payload -- are written once, here, so an app that wants a
// file made never depends on another app's tree (the same reason
// apps/materializer/handoff.ts holds the Materializer's own outgoing names).
//
//   actions.openApp(MATERIALIZER_APP_ID, MATERIALIZER_COMPOSE_SECTION, composeIntent({
//     format: "markdown",
//     folderId,                                   // a bare id
//     returnTo: { appId: "files", sectionId: "browse" },
//   }));
//
// ===========================================================================
// A SETUP IS A STARTING POINT, NEVER A COMMAND
// ===========================================================================
// The Materializer opens its composing surface with these facts filled in. The
// person still names the file, says what they want and chooses an organization;
// nothing here makes a file by itself.
//
// IT IS VALIDATED ON THE WAY IN. An intent is a payload some other app built, so
// `readComposeIntent` keeps only the fields it can use and drops the rest: a
// format this cluster does not write, a source that is not one of the three the
// Materializer takes, a return target missing half of its address. What is
// dropped is dropped quietly, because the sender could not have been told while
// it was still composing the payload; what survives is typed.
//
// BARE IDS ONLY, per the identifiers contract: a sender hands over the ids it
// holds and never composes, parses or compares a canonical one.
//
// THE FOLDER IS THE CLUSTER'S TO ACCEPT. The composer carries `folderId` with the
// rest of the setup and sends it as it is; compose refuses a folder the person
// cannot read, or one that has been archived, before anything is written, and the
// refusal ("That folder isn't available. Choose another folder.") is shown as it
// was worded. The app does not check it first: a check of its own would turn that
// refusal into a file filed quietly at the top, somewhere nobody chose.
//
// A SETUP LIVES AND DIES WITH THE FORM IT FILLED IN. Its format, sources, folder
// and return target are applied once, together, to a fresh form; leaving the
// composer abandons that form, and the first file made from it uses them up.
//
// SECTION IDS ARE CONTRACT. `openApp` to a section an app does not declare moves
// nobody and says nothing; test/materializer/intent.test.tsx pins this one
// against the manifest's own list.

/**
 * The app id. It is `materializer` even though the DSL namespace is `compose`,
 * and that split is deliberate rather than an inconsistency: `materializer`
 * already named the engine's boot seeder, so the ROWS took a different word
 * while the app kept the name a person knows -- `v1:work:goal.requestedVia`
 * was already carrying this string before either epic.
 */
export const MATERIALIZER_APP_ID = "materializer";

/** The section that composes a new file (and the one that opens on a composition). */
export const MATERIALIZER_MADE_SECTION = "materialized";
export const MATERIALIZER_COMPOSE_SECTION = "composer";

/** The kinds of file the Materializer writes, in the order it offers them. */
export const COMPOSE_FORMATS = ["markdown", "html", "txt", "csv", "json", "docx", "pdf"] as const;
export type ComposeFormat = (typeof COMPOSE_FORMATS)[number];

const SOURCE_KINDS = ["concept_row", "library_file", "query"] as const;
/** One thing to make the file from, as the Materializer records it. */
export interface ComposeSource {
  kind: (typeof SOURCE_KINDS)[number];
  ref: string;
  label: string;
}

export interface ComposeSetup {
  format?: ComposeFormat;
  /** Where to file it: a bare Library folder id. The cluster refuses one the person cannot read or that is archived. */
  folderId?: string;
  sources?: ComposeSource[];
  /**
   * An email template, which is a JSON document: the composer chooses JSON when
   * no format is given, and the reader drops the kind when another format is
   * named (the sender's format is what it asked for by name).
   */
  outputKind?: "email_template";
  /** Where the person goes back to once the file exists. */
  returnTo?: { appId: string; sectionId: string };
}

/** The `openApp` payload for a setup. */
export function composeIntent(setup: ComposeSetup): Record<string, unknown> {
  return { compose: setup };
}

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

/** A string with something in it, or "". */
const text = (value: unknown): string => (typeof value === "string" ? value.trim() : "");

function oneOf<T extends string>(choices: readonly T[], value: unknown): T | undefined {
  return choices.find((choice) => choice === value);
}

function readSource(value: unknown): ComposeSource | null {
  if (!isRecord(value)) return null;
  const kind = oneOf(SOURCE_KINDS, value["kind"]);
  const ref = text(value["ref"]);
  const label = value["label"];
  if (kind === undefined || ref === "" || typeof label !== "string") return null;
  return { kind, ref, label };
}

function readReturnTo(value: unknown): ComposeSetup["returnTo"] | null {
  if (!isRecord(value)) return null;
  const appId = text(value["appId"]);
  const sectionId = text(value["sectionId"]);
  return appId !== "" && sectionId !== "" ? { appId, sectionId } : null;
}

/**
 * The setup an intent payload carries, or null when it carries none -- which
 * means the payload is some other instruction and is left standing for whatever
 * it was meant for.
 *
 * A payload that names a setup is one even when nothing in it survives
 * validation: the sender asked to compose, and an empty setup opens the
 * composer blank.
 */
export function readComposeIntent(payload: Record<string, unknown>): ComposeSetup | null {
  const raw = payload["compose"];
  if (!isRecord(raw)) return null;
  const setup: ComposeSetup = {};
  const format = oneOf(COMPOSE_FORMATS, raw["format"]);
  if (format !== undefined) setup.format = format;
  const folderId = text(raw["folderId"]);
  if (folderId !== "") setup.folderId = folderId;
  const sources = Array.isArray(raw["sources"])
    ? raw["sources"].flatMap((item) => {
        const source = readSource(item);
        return source === null ? [] : [source];
      })
    : [];
  if (sources.length > 0) setup.sources = sources;
  if (raw["outputKind"] === "email_template" && (format === undefined || format === "json")) setup.outputKind = "email_template";
  const returnTo = readReturnTo(raw["returnTo"]);
  if (returnTo !== null) setup.returnTo = returnTo;
  return setup;
}
