// The language reference's data: the two artifacts a cluster generates, and
// the one sentence about WHICH language they describe.
//
// WHY THE CLUSTER AND NOT A COMMITTED PAGE. `memqlGrammar()` renders the BNF
// from the parser, the annotation registry and the function catalog at call
// time, and `memqlVocabulary()` reads the tables that own each name. Both are
// therefore what THIS cluster accepts right now, which a page in docs/ is only
// as long as nobody has moved the grammar since it was regenerated. A drifted
// grammar does not fail loudly: it teaches a form the parser refuses, and the
// failure reads as the model being bad at MemQL.
//
// WHY THE EXTENSION STILL HAS A SIDE OF ITS OWN. The extension ships its own
// language server, so its completion, diagnostics and highlighting are the
// grammar it was PACKAGED with -- stated in package.json's `memql` block and
// held to the parser by cmd/memql-lsp/editorparity_test.go. With no cluster
// there is no grammar and no vocabulary to show, and the honest page says so
// and shows the pin, rather than an empty panel or a spinner that never ends.
//
// THE EDITION'S STATUS IS READ, NEVER ASSUMED. "frozen" is a fact about an
// edition that lives in test/conformance/<edition>/manifest.json and reaches
// this extension through the same pin, gated by
// cmd/memql-lsp/editorparity_test.go's TestExtensionPinsTheEditionStatus. It
// is a fact about the edition THIS EXTENSION RECORDS, so a cluster speaking a
// different edition gets no status at all rather than this one's -- see
// languageIdentity below.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import type { Row } from "@znasllc-io/memql-sdk-core/client";

/**
 * The two calls, as the internal query form spells them.
 *
 * `builtin <name>()` is the top-level builtin call the SDK's generated methods
 * build (sdk/ts/src/client/generated_builtins.ts) -- neither builtin carries
 * `@sdk`, so there is no generated method to call and the string is written
 * here instead. The reply is ONE id-keyed node whose payload is the artifact,
 * which `Result.rows()` unwraps and flattens into a single row.
 */
export const GRAMMAR_CALL = "builtin memqlGrammar()";
export const VOCABULARY_CALL = "builtin memqlVocabulary()";

/** The names the calls are reported under, for an error message that names one. */
export const GRAMMAR_NAME = "memqlGrammar";
export const VOCABULARY_NAME = "memqlVocabulary";

/**
 * What this extension was built against, from its own package.json.
 *
 * Four separate facts rather than one string: each can be absent on its own,
 * and an absent one must read as "not stated" rather than as a value.
 */
export interface LanguagePin {
  /** The edition, e.g. "2026". "" when the manifest carries no pin. */
  edition: string;
  /** The edition's status -- "frozen" or "draft". "" when none is recorded. */
  status: string;
  /** The grammar version inside the edition. A label: compared for equality only. */
  grammarVersion: string;
  /** This extension's own release. */
  version: string;
}

function pinString(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/**
 * This extension's side, read from `context.extension.packageJSON`.
 *
 * Anything that is not a string reads as ABSENT, and every caller treats an
 * absent value as "not stated". The fake contexts the activation tests build
 * carry no `extension` at all, so a manifest that is undefined must produce a
 * pin of four empty strings rather than throwing -- a reference panel that
 * cannot open is worse than one that says it knows nothing.
 */
export function languagePin(packageJSON: unknown): LanguagePin {
  const manifest = isRecord(packageJSON) ? packageJSON : {};
  const pin = isRecord(manifest["memql"]) ? manifest["memql"] : {};
  return {
    edition: pinString(pin["edition"]),
    status: pinString(pin["status"]),
    grammarVersion: pinString(pin["grammarVersion"]),
    version: pinString(manifest["version"]),
  };
}

/** The generated BNF, as `memqlGrammar()` answers it. */
export interface GrammarArtifact {
  /** The notation the content is written in; "ebnf" today. */
  format: string;
  edition: string;
  grammarVersion: string;
  /** The productions. Never empty -- an empty content is read as no artifact. */
  content: string;
}

/** One named thing in the language, as `memqlVocabulary()` answers it. */
export interface VocabularyEntry {
  /** construct | annotation | keyword | operator | fieldType | function | builtin. */
  kind: string;
  /** The one spelling: `query`, `@cache`, `??`. An annotation carries its `@`. */
  name: string;
  /** How it is WRITTEN -- the declaration form, the call signature, the operator's form. */
  signature: string;
  /** The owning table's doc. */
  description: string;
  /** A catalog function's tier: "P" pushes down to SQL, "M" runs in process. "" elsewhere. */
  tier: string;
}

/** The vocabulary, as `memqlVocabulary()` answers it. */
export interface VocabularyArtifact {
  edition: string;
  grammarVersion: string;
  /** The kinds this cluster has, in page order. */
  kinds: string[];
  entries: VocabularyEntry[];
}

function rowString(row: Row, key: string): string {
  const value = row[key];
  return typeof value === "string" ? value : "";
}

/**
 * The grammar artifact from a builtin reply, or undefined.
 *
 * UNDEFINED RATHER THAN AN EMPTY ARTIFACT when the content is missing or
 * blank. "This cluster has no grammar" is not a thing a MemQL cluster can be,
 * so a blank content means the call did not answer what it was supposed to --
 * and a page that rendered an empty grammar pane would report that as a
 * language with no productions.
 */
export function grammarFromRows(rows: readonly Row[]): GrammarArtifact | undefined {
  const row = rows[0];
  if (row === undefined) return undefined;
  const content = rowString(row, "content");
  if (content.trim() === "") return undefined;
  return {
    format: rowString(row, "format"),
    edition: rowString(row, "edition"),
    grammarVersion: rowString(row, "grammarVersion"),
    content,
  };
}

/**
 * The vocabulary artifact from a builtin reply, or undefined.
 *
 * Entries are narrowed one at a time and an entry with no NAME is dropped: the
 * name is the only field a reader can act on, and an unnamed row would render
 * as a blank line that looks like a rendering bug. Everything else survives
 * empty, because an entry the cluster describes badly is still an entry it
 * has, and hiding it would under-report the language.
 */
export function vocabularyFromRows(rows: readonly Row[]): VocabularyArtifact | undefined {
  const row = rows[0];
  if (row === undefined) return undefined;
  const raw = row["entries"];
  if (!Array.isArray(raw)) return undefined;
  const entries: VocabularyEntry[] = [];
  for (const item of raw) {
    if (!isRecord(item)) continue;
    const name = typeof item["name"] === "string" ? item["name"] : "";
    if (name === "") continue;
    entries.push({
      kind: typeof item["kind"] === "string" ? item["kind"] : "",
      name,
      signature: typeof item["signature"] === "string" ? item["signature"] : "",
      description: typeof item["description"] === "string" ? item["description"] : "",
      tier: typeof item["tier"] === "string" ? item["tier"] : "",
    });
  }
  if (entries.length === 0) return undefined;
  const kinds = Array.isArray(row["kinds"])
    ? row["kinds"].filter((k): k is string => typeof k === "string")
    : [];
  return {
    edition: rowString(row, "edition"),
    grammarVersion: rowString(row, "grammarVersion"),
    kinds,
    entries,
  };
}

/**
 * The entries of one kind, in the order the cluster sent them.
 *
 * Grouped by the KINDS THE CLUSTER NAMED, with any kind it did not name
 * appended after them. A client that dropped an entry because it had not heard
 * of its kind would be silently under-reporting the language, which is the one
 * thing this page exists to be trusted about.
 */
export function vocabularyByKind(
  artifact: VocabularyArtifact,
): { kind: string; entries: VocabularyEntry[] }[] {
  const groups = new Map<string, VocabularyEntry[]>();
  for (const kind of artifact.kinds) groups.set(kind, []);
  for (const entry of artifact.entries) {
    const bucket = groups.get(entry.kind);
    if (bucket === undefined) groups.set(entry.kind, [entry]);
    else bucket.push(entry);
  }
  return [...groups.entries()]
    .filter(([, entries]) => entries.length > 0)
    .map(([kind, entries]) => ({ kind, entries }));
}

/** Where the facts on the page came from. */
export type LanguageSource = "cluster" | "extension";

/**
 * Which language the page is showing, and on whose authority.
 *
 * Every field is a sentence's worth of fact, resolved once here so the
 * renderer states it and decides nothing.
 */
export interface LanguageIdentity {
  source: LanguageSource;
  /** The cluster whose answer this is. "" when source is "extension". */
  cluster: string;
  /** "" when the side that spoke did not state one. */
  edition: string;
  /** "" when the side that spoke did not state one. */
  grammarVersion: string;
  /** "frozen" / "draft", or "" when nothing here can state it for this edition. */
  status: string;
  /** Why the status is what it is -- including why there is none. Never empty. */
  statusNote: string;
  /** One sentence naming where the facts above came from. Never empty. */
  origin: string;
}

/** What a connected cluster says about its own language. */
export interface ClusterLanguage {
  name: string;
  edition: string;
  grammarVersion: string;
}

/**
 * The cluster's language, preferring what the ARTIFACTS were generated from.
 *
 * The handshake states the node's edition and grammar version at connect; the
 * artifacts carry the ones they were rendered from. They agree in every
 * ordinary case, and when they do not the artifact's answer is the one that
 * describes what is on the page -- a header that named the handshake's version
 * over a grammar rendered from another would be labelling the wrong thing.
 */
export function clusterLanguage(
  name: string,
  handshake: { edition?: string | undefined; grammarVersion?: string | undefined },
  ...artifacts: ({ edition: string; grammarVersion: string } | undefined)[]
): ClusterLanguage {
  const stated = artifacts.find((a) => a !== undefined && a.edition !== "");
  return {
    name,
    edition: (stated?.edition ?? handshake.edition ?? "").trim(),
    grammarVersion: (
      artifacts.find((a) => a !== undefined && a.grammarVersion !== "")?.grammarVersion ??
      handshake.grammarVersion ??
      ""
    ).trim(),
  };
}

/**
 * Which language the page is showing.
 *
 * THE STATUS IS A FACT ABOUT AN EDITION, and this extension records exactly
 * one edition's. So it is stated when the cluster is on that edition and
 * WITHHELD when it is not: telling a reader that edition 2027 is frozen
 * because 2026 is would be inventing the one fact on the page that a reader
 * cannot check from the artifacts below it.
 */
export function languageIdentity(input: {
  pin: LanguagePin;
  cluster?: ClusterLanguage | undefined;
}): LanguageIdentity {
  const { pin, cluster } = input;

  if (cluster === undefined) {
    return {
      source: "extension",
      cluster: "",
      edition: pin.edition,
      grammarVersion: pin.grammarVersion,
      status: pin.edition === "" ? "" : pin.status,
      statusNote: pinStatusNote(pin),
      origin:
        pin.version === ""
          ? "This extension's own pin. No cluster is connected, so nothing has been asked."
          : `This extension's own pin (MemQL for Visual Studio Code and Cursor ${pin.version}). No cluster is connected, so nothing has been asked.`,
    };
  }

  const sameEdition = cluster.edition !== "" && cluster.edition === pin.edition;
  return {
    source: "cluster",
    cluster: cluster.name,
    edition: cluster.edition,
    grammarVersion: cluster.grammarVersion,
    status: sameEdition ? pin.status : "",
    statusNote: sameEdition
      ? pinStatusNote(pin)
      : cluster.edition === ""
        ? "This cluster did not state an edition, so no status applies to it."
        : pin.edition === ""
          ? `This extension records no edition, so it cannot say whether edition ${cluster.edition} is frozen.`
          : `This extension records the status of edition ${pin.edition} only, and this cluster speaks edition ${cluster.edition}.`,
    origin: `Read from the cluster ${cluster.name}, which generates both from its own parser.`,
  };
}

/** What the pin can say about its own edition's status. */
function pinStatusNote(pin: LanguagePin): string {
  if (pin.edition === "") return "This extension records no edition, so it records no status either.";
  if (pin.status === "") return `This extension records no status for edition ${pin.edition}.`;
  return `Recorded by this extension for edition ${pin.edition}.`;
}

// -----------------------------------------------------------------------------
// The grammar, in blocks
// -----------------------------------------------------------------------------

/** One production, with every continuation line that belongs to it. */
export interface GrammarBlock {
  /** The production's name without its angle brackets, or "" for a note. */
  name: string;
  /** The block's lines, exactly as the cluster rendered them. */
  lines: string[];
}

/** A banner and the blocks under it. */
export interface GrammarSection {
  /** The banner's text, or "" for the blocks before the first banner. */
  title: string;
  blocks: GrammarBlock[];
}

/** A production's head: `<name>` followed by `::=`. */
const PRODUCTION = /^<([^>]+)>\s*::=/;
/** A banner: a comment whose first line opens `(* ----`. */
const BANNER = /^\(\*\s*----/;

/**
 * The grammar, split into the sections and productions it is already written
 * as.
 *
 * WHY BLOCKS AND NOT LINES. A production wraps onto continuation lines that
 * open with `|`, so a search that filtered LINES would show the first half of
 * an alternation and hide the rest -- a grammar that is wrong rather than
 * short. The block is the unit a reader means when they search for a name.
 *
 * The sections are the cluster's own `(* ---- … ---- *)` banners, so the
 * grouping on the page is the grouping in the artifact rather than one
 * invented here. Anything before the first banner -- the header comment naming
 * the edition and the notation -- lands in a leading section with no title.
 */
export function grammarSections(content: string): GrammarSection[] {
  const sections: GrammarSection[] = [{ title: "", blocks: [] }];
  let block: GrammarBlock | undefined;

  const push = (): void => {
    if (block !== undefined) {
      const section = sections[sections.length - 1];
      if (section !== undefined) section.blocks.push(block);
      block = undefined;
    }
  };

  const lines = content.split("\n");
  for (let i = 0; i < lines.length; i += 1) {
    const line = lines[i] ?? "";
    if (line.trim() === "") {
      push();
      continue;
    }
    if (line.startsWith("(*")) {
      push();
      // A comment runs to its closing `*)`, which may be several lines away:
      // both the file header and the precedence-ladder banner wrap.
      const comment: string[] = [];
      for (; i < lines.length; i += 1) {
        const commentLine = lines[i] ?? "";
        comment.push(commentLine);
        if (commentLine.includes("*)")) break;
      }
      if (BANNER.test(line)) sections.push({ title: bannerTitle(comment), blocks: [] });
      else sections[sections.length - 1]?.blocks.push({ name: "", lines: comment });
      continue;
    }
    const head = PRODUCTION.exec(line);
    if (head !== null) {
      push();
      block = { name: head[1] ?? "", lines: [line] };
      continue;
    }
    // A continuation of the production above, or -- before any production --
    // a stray line kept rather than dropped, because dropping part of a
    // generated artifact is the one thing this page must not do.
    if (block === undefined) block = { name: "", lines: [] };
    block.lines.push(line);
  }
  push();
  return sections.filter((section) => section.title !== "" || section.blocks.length > 0);
}

/** A banner's words: `(* ---- A file ---- *)` is "A file". */
function bannerTitle(comment: string[]): string {
  return comment
    .join(" ")
    .replace(/^\(\*/, "")
    .replace(/\*\)$/, "")
    .replace(/-{2,}/g, " ")
    .replace(/\s+/g, " ")
    .trim();
}

/** Every production the grammar declares, for the count the page states. */
export function productionCount(sections: readonly GrammarSection[]): number {
  return sections.reduce(
    (total, section) => total + section.blocks.filter((b) => b.name !== "").length,
    0,
  );
}

// -----------------------------------------------------------------------------
// Search
// -----------------------------------------------------------------------------

/**
 * The text one item is matched against, lowercased.
 *
 * BUILT HERE, MATCHED IN THE PAGE. A webview script cannot import a module
 * under this CSP, so the search runs in the browser -- but only the rule
 * `index.includes(term)` does. Everything with a decision in it, which is
 * WHICH text a term may match, is this function, and it is what the tests
 * exercise. The page's half is pinned textually by the render tests.
 */
export function searchIndex(parts: readonly string[]): string {
  // Whitespace is COLLAPSED, because a production wraps: `<declaration>`'s
  // alternation carries a newline and a run of alignment spaces in the middle
  // of it, and a reader searching for two adjacent words of it would otherwise
  // be told the grammar does not contain a line the page is showing them.
  return parts
    .filter((part) => part !== "")
    .join(" ")
    .replace(/\s+/g, " ")
    .toLowerCase();
}

/**
 * The search rule: a case-insensitive substring of the index.
 *
 * An empty term matches everything, which is what makes "clear the box" mean
 * "show me all of it" rather than "show me nothing".
 */
export function matchesSearch(index: string, term: string): boolean {
  const needle = term.trim().toLowerCase();
  return needle === "" || index.includes(needle);
}

// -----------------------------------------------------------------------------
// What the copy buttons put on the clipboard
// -----------------------------------------------------------------------------

/**
 * The vocabulary as text, for handing to a model.
 *
 * TAB-SEPARATED, with a header naming the columns. The descriptions are prose
 * written by whoever owns each table, and several of them contain `|`, `,` and
 * `:` -- a tab is the one separator none of them carries, so a reader (human
 * or model) can split a line back into its five fields without guessing.
 *
 * The header states the edition and the grammar version, because the whole
 * point of asking a cluster rather than reading a page is knowing which
 * language the answer describes -- and a block of text pasted into a prompt
 * has nothing else to carry that.
 */
export function vocabularyText(artifact: VocabularyArtifact): string {
  const lines = [
    `# MemQL vocabulary. Edition ${artifact.edition || "unstated"}, grammar version ${
      artifact.grammarVersion || "unstated"
    }. ${artifact.entries.length} entries.`,
    "# kind\tname\twritten\tmeans\ttier",
  ];
  for (const entry of artifact.entries) {
    lines.push(
      [entry.kind, entry.name, entry.signature, entry.description, entry.tier].join("\t"),
    );
  }
  return lines.join("\n") + "\n";
}
