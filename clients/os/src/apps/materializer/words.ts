import type {
  CompositionRow,
  CompositionStatus,
  ModelRow,
  SourceRow,
} from "./rows";

// words.ts -- every state word and every sentence this app says, in one
// place, as pure functions.
//
// THE ONE VOCABULARY RULE THE DEPLOYABLES EPIC WROTE DOWN applies here:
// one name, one promise. A composition is Composing while a model is
// working and Writing while bytes are being produced -- two different
// situations that a single "Working" would hide, and the difference is
// exactly the one this app exists to make visible.
//
// The verbs are Materialize / Stop / Open / Save as recipe / Archive.
// "Generate" appears nowhere: it is what every other tool calls this,
// and it says nothing about where the content came from -- which is the
// whole subject here.

/** The state, in words. Read by the action bar and the list's chip. */
export function statusWord(status: CompositionStatus): string {
  switch (status) {
    case "draft":
      return "Draft";
    case "composing":
      return "Composing";
    case "rendering":
      return "Writing the file";
    case "ready":
      return "Ready";
    case "failed":
      return "Failed";
    case "cancelled":
      return "Stopped";
    default:
      // An UNKNOWN status is reported as unknown rather than folded into
      // the nearest word. A row written by a newer engine than this
      // bundle would otherwise read as something it is not, and "Draft"
      // is the value that would silently offer a Materialize button for
      // a composition already running.
      return "Unknown";
  }
}

/**
 * The tone a status chip takes.
 *
 * THE KIT'S CHIP HAS NO WARN OR ERROR TONE, and that is the shell's
 * decision rather than a gap this app should widen. Amber is `warn` and
 * red is `error` here, and both belong to things a person must act on --
 * a live alarm, a refusal beside the control that caused it. A red chip
 * on every failed row in a list turns a scan into an alarm board, and the
 * hue stops meaning anything.
 *
 * So the STATE IS A WORD and the tone only separates settled from
 * in-flight: `accent` for a composition that produced a file, `muted` for
 * one that will not, `neutral` while it is still going. The failure's own
 * sentence renders where the composition opens, beside the acts that
 * follow from it -- which is where somebody can do something about it.
 */
export function statusTone(
  status: CompositionStatus,
): "neutral" | "accent" | "muted" {
  switch (status) {
    case "ready":
      return "accent";
    case "failed":
    case "cancelled":
      return "muted";
    default:
      return "neutral";
  }
}

export function isRunning(status: CompositionStatus): boolean {
  return status === "composing" || status === "rendering";
}

/** What a format is called where a person chooses one. */
export function formatWord(format: string): string {
  switch (format) {
    case "markdown":
      return "Markdown";
    case "html":
      return "Web page";
    case "txt":
      return "Plain text";
    case "csv":
      return "Spreadsheet (CSV)";
    case "json":
      return "Data (JSON)";
    case "docx":
      return "Word document";
    case "pdf":
      return "PDF";
    default:
      return format || "Unknown";
  }
}

/** The formats this cluster can write, in the order the wizard offers them. */
export const FORMATS = [
  "markdown",
  "html",
  "txt",
  "csv",
  "json",
  "docx",
  "pdf",
] as const;

export function deployableWord(kind: string): string {
  switch (kind) {
    case "spa":
      return "Single-page app";
    case "static":
      return "Static site";
    case "shopify_storefront":
      return "Shopify storefront";
    default:
      return kind;
  }
}

/**
 * How the sources read as one phrase: "3 rows, 1 file".
 *
 * PLURALISED AND COUNTED BY KIND, because the kinds are not
 * interchangeable: "3 rows" and "1 query" are different claims about
 * whether this composition can be made again.
 */
export function sourcesPhrase(sources: SourceRow[]): string {
  if (sources.length === 0) return "no sources";
  const byKind = new Map<string, number>();
  for (const s of sources) byKind.set(s.kind, (byKind.get(s.kind) ?? 0) + 1);
  const parts: string[] = [];
  for (const kind of [...byKind.keys()].sort()) {
    const n = byKind.get(kind) ?? 0;
    parts.push(`${n} ${kindWord(kind, n)}`);
  }
  return parts.join(", ");
}

function kindWord(kind: string, n: number): string {
  const [one, many] =
    kind === "concept_row"
      ? ["row", "rows"]
      : kind === "library_file"
        ? ["file", "files"]
        : kind === "query"
          ? ["query", "queries"]
          : [kind, kind];
  return n === 1 ? one : many;
}

/**
 * How the models read: the product's headline claim, in words.
 *
 * AN EMPTY LIST IS "no model", NOT AN EM DASH. Everywhere else in this
 * shell an absent figure is a dash with a reason, because absence there
 * means "not measured". Here it means something stronger and TRUE: the
 * composition reached no provider, which is the claim the whole work
 * spine makes. Rendering that as "unknown" would throw away the one
 * result worth showing.
 */
export function modelsPhrase(models: ModelRow[]): string {
  if (models.length === 0) return "no model";
  return models
    .map((m) => {
      const name = m.model.trim() || "unnamed model";
      const labelled = m.provider.trim() ? `${m.provider}/${name}` : name;
      return m.calls > 1 ? `${labelled} (${m.calls} calls)` : labelled;
    })
    .join(", ");
}

/**
 * The provenance claim the format alone can make, for a record that carries no
 * sentence of its own: the chain renders the record's `provenanceNote` when it
 * has one, because only the stamp step knows which branch it took. A blank file
 * carries no front matter because it was filed blank, not because Markdown has
 * nowhere to put it, and a claim worked out from the format would say the
 * opposite.
 *
 * THREE ANSWERS, NEVER TWO. `null` is "nothing has decided yet" -- a
 * composition that has not reached the stamp step -- and saying "the
 * record is the only copy" there would be a claim about a file that does
 * not exist yet.
 */
export function provenanceClaim(
  embedded: boolean | null,
  format: string,
): string {
  if (embedded === null) return "";
  if (embedded) return "the file carries it";
  return `${formatWord(format)} has nowhere to put it — the record is the only copy`;
}

// ---------------------------------------------------------------------------
// What a finished file is
// ---------------------------------------------------------------------------
//
// THE CLUSTER FILES TWO KINDS OF FILE EMPTY, and "the composed text is in the
// file" is false of both: a blank one (nothing was composed) and a Markdown file
// asked for by describing it (empty until the person approves the Library's
// draft). So the caption under what was asked for says what is known.
//
// The row tells them apart: a blank file has no statement, no sources and no
// model call; a described one has a statement, no model call, only Library files
// for sources, and does not carry its provenance (it was filed blank, so it has
// no front matter -- the same fact as its "Created blank." note). A Markdown
// file a model wrote carries its provenance and records the model. This window
// knows more about a file it asked for by describing it: whether it has sent the
// description, and whether that worked.

export const CREATED_BLANK = "This file was created blank.";
export const DESCRIPTION_SENT =
  "Your description was sent to be written. Open it in the editor to review and approve the draft.";
export const CREATED_FROM_DESCRIPTION =
  "This file was created from a description. Open it in the editor to continue.";
export const COMPOSED_IN_FILE = "The composed text is in the file.";

/**
 * The caption under "What was asked for" on a finished file, or "" for none:
 * nothing is said of a file that does not exist yet or failed to (the state
 * line and the failure say that), nor of a described file whose description
 * this window could not send (the notice that says so is the whole of it).
 *
 * `describedHere` is whether this window asked for the file by describing it;
 * `outcome` is what became of its hand-off, absent while this window does not
 * know (it has not heard back, or the file was opened later from the list).
 */
export function madeCaption(
  c: CompositionRow,
  sources: SourceRow[],
  models: ModelRow[],
  describedHere: boolean,
  outcome?: { failure: string },
): string {
  if (c.status !== "ready") return "";
  const statement = c.statement.trim() !== "";
  if (!statement && sources.length === 0 && models.length === 0)
    return CREATED_BLANK;
  const described =
    describedHere ||
    (statement &&
      c.format === "markdown" &&
      models.length === 0 &&
      c.provenanceEmbedded === false &&
      sources.every((source) => source.kind === "library_file"));
  if (!described) return COMPOSED_IN_FILE;
  if (outcome === undefined) return CREATED_FROM_DESCRIPTION;
  return outcome.failure === "" ? DESCRIPTION_SENT : "";
}

// ---------------------------------------------------------------------------
// A Markdown file asked for by describing it
// ---------------------------------------------------------------------------
//
// The file is filed empty and the description is handed to the Library's
// revision workflow, which writes it. When that hand-off could not be made the
// file is still there and still good, and the sentence says so FIRST: a person
// who reads "failed" over a file they can open would go looking for a loss.

/** The description could not be handed on. The cluster's own reason rides beside it, verbatim. */
export const DESCRIPTION_NOT_SENT =
  "The file was created, but the description was not sent for writing.";

/** What to do about it: the editor's empty page is where a description is sent from. */
export const DESCRIPTION_NOT_SENT_NEXT =
  "Open it in the editor and choose Describe document to send it again.";

// The SDK opens a hand-off's failure with this (handOffDescription's
// revisionFailure). The notice's own sentence already says it, first.
const NOT_SENT_LEAD = "The description could not be sent for writing: ";

/** A hand-off's failure as the notice's detail: the cluster's reason, without the SDK's lead-in saying again what the notice says. */
export function notSentDetail(failure: string): string {
  return failure.startsWith(NOT_SENT_LEAD)
    ? failure.slice(NOT_SENT_LEAD.length)
    : failure;
}

/**
 * What is said of a reference that went without being attached, named beside it
 * by the label the person gave it.
 *
 * IT CLAIMS ONLY WHAT IS KNOWN. A reference is left off for its kind, for its
 * size, for the Library's limits on a comment, for not being found or being in
 * the Bin, or because the Library refused the attachments as a set -- and the
 * SDK's `unattached` entry is the label alone, carrying no reason to show. So
 * the sentence says that it was not attached, and not why.
 */
export const NOT_ATTACHED = "This reference wasn't attached.";
