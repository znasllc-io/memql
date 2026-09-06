import type { CompositionRow, CompositionStatus, ModelRow, SourceRow } from "./rows";

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
export function statusTone(status: CompositionStatus): "neutral" | "accent" | "muted" {
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

export function isTerminal(status: CompositionStatus): boolean {
  return status === "ready" || status === "failed" || status === "cancelled";
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

/** The formats this cluster can write, in the order the Target offers them. */
export const FORMATS = ["markdown", "html", "txt", "csv", "json", "docx", "pdf"] as const;

/**
 * The two formats named in the brief that this cluster does not offer,
 * with the reason.
 *
 * THE SURFACE SAYS THIS RATHER THAN OMITTING IT. An absent option with
 * no account of itself reads as something nobody got round to building
 * -- the rule the Bin states about its missing retention control and
 * Domains states about its missing re-check button.
 */
export const UNOFFERED_FORMATS: { name: string; why: string }[] = [
  {
    name: "Audio",
    why: "needs a compose-then-speak pipeline with a spending ceiling of its own",
  },
  { name: "Video", why: "needs a generation provider this cluster does not have" },
];

/** The deployable kinds, matching what the cluster actually serves. */
export const DEPLOYABLE_KINDS = ["spa", "static", "shopify_storefront"] as const;

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
 * The provenance claim, as the one sentence the Composer and the record
 * panel both render.
 *
 * THREE ANSWERS, NEVER TWO. `null` is "nothing has decided yet" -- a
 * composition that has not reached the stamp step -- and saying "the
 * record is the only copy" there would be a claim about a file that does
 * not exist yet.
 */
export function provenanceClaim(embedded: boolean | null, format: string): string {
  if (embedded === null) return "";
  if (embedded) return "the file carries it";
  return `${formatWord(format)} has nowhere to put it — the record is the only copy`;
}

/** The empty state of the Materialized list: an invitation, not a mood. */
export const MATERIALIZED_EMPTY =
  "Nothing materialized yet. Pick some rows in Compose and make the first file.";

export const TEMPLATES_EMPTY =
  "No templates yet. Upload a document to your Library, then bind it here to make its shape repeatable.";

export const RECIPES_EMPTY =
  "No recipes yet. Materialize something, then save it as a recipe to make it again next quarter.";

/** What a failure says. It names the fix where there is one. */
export function failureSentence(c: CompositionRow): string {
  const reason = c.failureReason.trim();
  if (reason) return reason;
  return "This composition stopped and recorded no reason. The Logs section holds the lines for this app.";
}
