// A Library artifact's address, its delivery decision, and what a document
// says when the bytes are not there (memql#4748).
//
// THE ARTIFACT IS NOT ON THIS MACHINE, and this is the honest rendering of
// that: a read-only document whose bytes come from the cluster's own artifact
// content route. The shape mirrors constructs/clusterDocument.ts -- one module
// of decisions with no `vscode` import, and one adapter beside it that
// converts -- because the two are the same problem one domain apart.
//
// WRITE-BACK IS NOT BUILT, AND THE ABSENCE IS DELIBERATE (epic memql#4748,
// "read-only first"). A `TextDocumentContentProvider` document is read-only by
// construction: VS Code has no way to save one, so there is no save path to
// disable and no command to grey out. "Edit this and push it back to the
// Library" is a SEPARATE decision -- it needs an answer for what a save means
// when the row has moved underneath the buffer, for which of an artifact's
// backing concepts is writable at all (a `document` is not), and for whether a
// mirror concept may be written from an editor. None of those are settled, and
// half-answering them by adding a save command would be the worst outcome:
// an affordance that appears to work.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go);
// artifactDocuments.ts is the adapter.
//
// Refs: #4748 #4251 #4248

export const ARTIFACT_DOCUMENT_SCHEME = "memql-artifact";

/**
 * How much an artifact may weigh before it is offered as a file on disk
 * instead of an editor buffer.
 *
 * IT APPLIES TO TEXT TOO, which is the point. A text document is a string in
 * the extension host, a second string in the renderer, and a tokenized model
 * on top of both -- so the eight megabytes on the wire is the smallest of the
 * numbers this cap is really about. Something the editor cannot usefully open
 * is better handed over as a file, with the reason said out loud, than opened
 * into a window that stops responding.
 */
export const ARTIFACT_BUFFER_LIMIT_BYTES = 8 * 1024 * 1024;

/** Longest filename this composes, in characters, before it is cut. */
const MAX_FILE_NAME_CHARS = 120;

export interface ArtifactDocumentRef {
  /** The registry name of the cluster the bytes come from. */
  cluster: string;
  /** The artifact row's id, exactly as the link carried it. */
  artifactId: string;
  /** Sanitised, and the LAST path segment: it is what names the editor tab. */
  fileName: string;
}

/**
 * The uri a Library artifact opens at.
 *
 * THE PATH IS THE TAB TITLE. VS Code labels an editor from the last segment of
 * its uri path and offers no API to set a tab's title or description for a text
 * document -- so the filename goes in the path, and getting the path right is
 * the whole of getting the tab right. The cluster is the AUTHORITY (the shape
 * clusterDocumentUri established, and the reason a cluster name with a space or
 * a percent sign survives the round trip), and the artifact id is a QUERY
 * parameter because it addresses the row rather than naming the file.
 *
 * The rest of the provenance -- which cluster, what kind, how big -- is written
 * to the MemQL Connection channel when the document opens, and is visible on
 * the tab's own hover, which renders the whole uri.
 */
export function artifactDocumentUri(ref: ArtifactDocumentRef): string {
  const fileName = sanitizeArtifactFileName(ref.fileName);
  return (
    `${ARTIFACT_DOCUMENT_SCHEME}://${encodeURIComponent(ref.cluster)}/${encodeURIComponent(fileName)}` +
    `?id=${encodeURIComponent(ref.artifactId)}`
  );
}

/**
 * What a `memql-artifact:` uri says, or undefined when it says something this
 * provider cannot serve.
 *
 * `path` arrives DECODED (VS Code's `Uri.path` is), so it is read as-is --
 * decoding it a second time is what turns a legitimate `Q1 100% report.md`
 * into an empty string. The authority is left encoded by `Uri`, so that half
 * goes through the tolerant decoder, exactly as parseClusterDocumentUri does.
 */
export function parseArtifactDocumentUri(uri: {
  authority: string;
  path: string;
  query: string;
}): ArtifactDocumentRef | undefined {
  const cluster = safeDecode(uri.authority);
  const fileName = uri.path.replace(/^\/+/, "");
  if (cluster === "" || fileName === "") return undefined;
  const artifactId = new URLSearchParams(uri.query).get("id") ?? "";
  if (artifactId === "") return undefined;
  return { cluster, artifactId, fileName };
}

/**
 * What the handoff learned about an artifact from the graph, before any byte
 * of its content was fetched.
 *
 * EVERY DELIVERY DECISION IS MADE FROM THIS AND NOTHING ELSE. Buffering a file
 * to find out whether it is text is the one thing this must never do: it costs
 * the whole download to answer a question the row already answers, and it costs
 * it precisely in the case -- a large binary -- where the answer is "do not
 * download this into memory".
 */
export interface ArtifactMeta {
  artifactId: string;
  /** `v1:library:artifact.title`. */
  title: string;
  /** `v1:library:artifact.kind`: file / note / generated_output / memory / ... */
  kind: string;
  /** `v1:library:artifact.format`, or "" when the row states none. */
  format: string;
  /** The backing bytes' declared type, or "" when nothing states one. */
  mimeType: string;
  /** `v1:library:file.name`, when the artifact is file-backed. "" otherwise. */
  fileName: string;
  /**
   * The backing file's size in bytes, when a row states one.
   *
   * UNDEFINED IS NOT ZERO. Only a file-backed artifact has a size to state; a
   * note or a generated output is rendered to text by the server on the way
   * out, and its length is not knowable here. Treating "not stated" as 0 would
   * pass the cap by accident rather than by measurement, which is the right
   * outcome for the wrong reason -- and the wrong outcome the day a file row
   * arrives with no size.
   */
  sizeBytes?: number;
}

/**
 * How an artifact should be delivered: into an editor buffer, or onto disk.
 *
 * `reason` is the phrase a person is shown when the answer is disk. It exists
 * because "save this instead" with no explanation reads as the extension
 * refusing to do the thing that was asked -- and the two reasons (it is not
 * text, it is too big) call for completely different reactions.
 */
export type ArtifactDelivery = { kind: "editor" } | { kind: "saveToDisk"; reason: string };

/** Formats whose content is text whatever the mime type says. */
const TEXT_FORMATS = new Set(["markdown", "text", "conversation"]);

/**
 * The `application/*` types that are text in practice.
 *
 * A CLOSED LIST, not a heuristic. `application/*` is the half of the mime tree
 * that is binary by default, and the members here are the ones whose bytes are
 * a document a person reads: everything else in that tree stays binary, which
 * is the safe direction -- an editor buffer full of a PDF's bytes is a worse
 * answer than a save dialog for a JSON file.
 *
 * `toml` and `csv` are on the list and have no VS Code language id, which is
 * fine and deliberate: whether a thing is TEXT and what LANGUAGE it is are
 * different questions, and answering the first does not oblige an answer to
 * the second.
 */
const TEXTUAL_APPLICATION_TYPES = new Set([
  "application/json",
  "application/xml",
  "application/yaml",
  "application/x-yaml",
  "application/javascript",
  "application/typescript",
  "application/sql",
  "application/toml",
  "application/csv",
]);

/** The mime type without its parameters, lowercased. `""` when there is none. */
export function baseMimeType(mimeType: string): string {
  const semicolon = mimeType.indexOf(";");
  return (semicolon < 0 ? mimeType : mimeType.slice(0, semicolon)).trim().toLowerCase();
}

/**
 * Whether an artifact's content is text, decided from the row alone.
 *
 * A NON-FILE KIND IS ALWAYS TEXT, and that is a fact about the SERVER rather
 * than a guess: a note, a generated output or a memory has no stored bytes at
 * all, and the content route renders it to `text/markdown` or `text/plain` on
 * the way out (component/server/artifact_handler.go, serveRenderedBody). Only
 * `kind == "file"` streams stored bytes, so only it can be binary.
 */
export function isTextArtifact(meta: Pick<ArtifactMeta, "kind" | "format" | "mimeType">): boolean {
  if (meta.kind !== "file") return true;
  if (TEXT_FORMATS.has(meta.format)) return true;
  const mime = baseMimeType(meta.mimeType);
  if (mime.startsWith("text/")) return true;
  return TEXTUAL_APPLICATION_TYPES.has(mime);
}

/**
 * Where an artifact should land: an editor buffer, or a file the person picks
 * a home for.
 *
 * TWO GATES, IN THIS ORDER, because the reasons do not compose. A binary that
 * is also enormous is offered as a file for the first reason, and saying so is
 * more useful than a size that implies a smaller one would have opened.
 */
export function artifactDelivery(meta: ArtifactMeta): ArtifactDelivery {
  if (!isTextArtifact(meta)) {
    const mime = baseMimeType(meta.mimeType);
    return { kind: "saveToDisk", reason: mime === "" ? "its content is not text" : `its content is ${mime}, not text` };
  }
  const size = meta.sizeBytes;
  if (size !== undefined && size > ARTIFACT_BUFFER_LIMIT_BYTES) {
    return {
      kind: "saveToDisk",
      reason: `it is ${formatByteSize(size)}, past the ${formatByteSize(ARTIFACT_BUFFER_LIMIT_BYTES)} an editor buffer takes`,
    };
  }
  return { kind: "editor" };
}

/**
 * The VS Code language id an artifact's own metadata names, or undefined when
 * nothing does.
 *
 * ONLY LANGUAGES EVERY EDITOR SHIPS. `setTextDocumentLanguage` rejects an id no
 * extension has contributed, so mapping `application/toml` to `toml` would
 * throw on a stock editor and succeed on the author's -- the worst kind of
 * difference. A type with no built-in language simply gets no answer.
 *
 * `text/plain` DELIBERATELY MAPS TO NOTHING. Plain text is what VS Code falls
 * back to anyway, so setting it can only ever DISCARD the language the
 * filename's extension had already implied -- an uploaded `schema.sql` served
 * as `text/plain` would lose its highlighting to a statement that told the
 * editor nothing it had not assumed.
 */
const LANGUAGE_BY_MIME: Record<string, string> = {
  "text/markdown": "markdown",
  "text/x-markdown": "markdown",
  "application/json": "json",
  "text/json": "json",
  "application/xml": "xml",
  "text/xml": "xml",
  "application/yaml": "yaml",
  "application/x-yaml": "yaml",
  "text/yaml": "yaml",
  "text/x-yaml": "yaml",
  "application/javascript": "javascript",
  "text/javascript": "javascript",
  "application/typescript": "typescript",
  "text/typescript": "typescript",
  "application/sql": "sql",
  "text/sql": "sql",
  "text/html": "html",
  "text/css": "css",
};

export function languageIdFor(format: string, mimeType: string): string | undefined {
  const byMime = LANGUAGE_BY_MIME[baseMimeType(mimeType)];
  if (byMime !== undefined) return byMime;
  // The format enum is coarser than a mime type and only one of its values
  // names a language: `text` and `conversation` are "this is readable", not
  // "this is plaintext", and `document` / `spreadsheet` / `pdf` / `image` never
  // reach an editor buffer at all.
  return format === "markdown" ? "markdown" : undefined;
}

/**
 * The filename an artifact opens under.
 *
 * IT MIRRORS THE SERVER'S OWN NAMING, and has to be composed here rather than
 * read from the response's `Content-Disposition`: the name is part of the uri,
 * and the uri exists before the fetch. component/server/artifact_handler.go is
 * the other half -- `sanitizeLibraryFileName(firstNonBlank(file.Name, title))`
 * for a file, `exportFileName(title, ext)` for a rendered body -- and the two
 * agreeing is what stops a tab being called one thing and the saved file
 * another.
 */
export function artifactFileName(meta: Pick<ArtifactMeta, "kind" | "title" | "fileName" | "format" | "mimeType">): string {
  if (meta.kind === "file") {
    const fromFile = sanitizeArtifactFileName(meta.fileName);
    if (fromFile !== "") return fromFile;
    const fromTitle = sanitizeArtifactFileName(meta.title);
    return fromTitle === "" ? "artifact" : fromTitle;
  }
  return exportFileName(meta.title, renderedExtension(meta));
}

/**
 * Which extension the server's rendered export will carry.
 *
 * A note is always markdown and a memory never is (they are a document and a
 * sentence respectively); everything else is believed when it says markdown.
 * Mirrors ExportBody's `Markdown` flag.
 */
function renderedExtension(meta: Pick<ArtifactMeta, "kind" | "format" | "mimeType">): string {
  if (meta.kind === "note") return ".md";
  if (meta.kind === "memory") return ".txt";
  return meta.format === "markdown" || baseMimeType(meta.mimeType) === "text/markdown" ? ".md" : ".txt";
}

/**
 * A title turned into a filename: whitespace to hyphens, then the extension.
 *
 * The hyphens are the server's choice and are kept for parity -- a name that
 * survives a shell without quoting is worth more than one that reads slightly
 * better in a tab.
 */
function exportFileName(title: string, ext: string): string {
  const collapsed = title.trim().split(/\s+/).filter((part) => part !== "").join("-");
  const name = sanitizeArtifactFileName(collapsed === "" ? "artifact" : collapsed);
  const base = name === "" ? "artifact" : name;
  return base.toLowerCase().endsWith(ext) ? base : `${base}${ext}`;
}

/**
 * A filename with everything that is not a filename taken out.
 *
 * THE NAME COMES FROM THE CLUSTER, so it is treated as input rather than as a
 * value this editor produced: a directory component would put the document at
 * a path the uri did not name, and a control character would render into a tab
 * title, a save dialog and a channel line. Both are removed rather than
 * rejected -- there is a real artifact behind the name, and refusing to open it
 * over its own title would be a worse answer than opening it under a tidied
 * one.
 *
 * Leading and trailing dots go for the reason the server drops them: `.` and
 * `..` are directories, and a leading dot hides the file on every unix a save
 * dialog might write it to.
 */
export function sanitizeArtifactFileName(name: string): string {
  const lastSeparator = Math.max(name.lastIndexOf("/"), name.lastIndexOf("\\"));
  const base = lastSeparator < 0 ? name : name.slice(lastSeparator + 1);
  const printable = [...base]
    .filter((ch) => {
      const code = ch.codePointAt(0) ?? 0;
      return code >= 0x20 && code !== 0x7f;
    })
    .join("");
  const trimmed = printable.trim().replace(/^\.+/, "").replace(/\.+$/, "").trim();
  if (trimmed === "") return "";
  const runes = [...trimmed];
  return runes.length > MAX_FILE_NAME_CHARS ? runes.slice(0, MAX_FILE_NAME_CHARS).join("") : trimmed;
}

/** A byte count as a person reads it. */
export function formatByteSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "an unknown size";
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value.toFixed(1)} ${units[unit]}`;
}

/**
 * The one line that records where an opened artifact came from.
 *
 * Written to the MemQL Connection channel on every artifact handoff, whatever
 * the outcome -- the same "one line for every handoff" the construct path keeps,
 * and for the same reason: "I clicked the file and got the wrong thing" is
 * reconstructed afterwards or not at all.
 */
export function artifactProvenanceLine(cluster: string, meta: ArtifactMeta): string {
  const parts = [meta.kind, meta.format, baseMimeType(meta.mimeType)].filter((p) => p !== "");
  const size = meta.sizeBytes === undefined ? "" : `, ${formatByteSize(meta.sizeBytes)}`;
  return `${meta.artifactId} from ${cluster} (${parts.join(", ")}${size})`;
}

/** The bff route an artifact's bytes come from, against an https base. */
export function artifactContentUrl(apiBaseUrl: string, artifactId: string): string {
  return `${apiBaseUrl.replace(/\/+$/, "")}/artifacts/${encodeURIComponent(artifactId)}/content`;
}

// -----------------------------------------------------------------------------
// Notices
//
// What the BUFFER says when there are no bytes to put in it. Every one of them
// is comment-prefixed and none of them carries a raw error, for the reason
// clusterDocument.ts states: this text is rendered INTO a document, the raw
// detail belongs in the MemQL Connection channel through the redactor, and a
// notice that looks like content is worse than no document at all.
//
// `//` in a buffer that may be markdown or JSON is deliberate. It is not a
// comment in either, and that is the point -- a notice must not blend into the
// artifact's own words, and in a language that DOES have line comments it reads
// exactly as the aside it is.
// -----------------------------------------------------------------------------

export function notConnectedNotice(cluster: string): string {
  return (
    `// Not connected to ${cluster}.\n` +
    `// This artifact is served from the cluster; reconnect to ${cluster} and reopen it.\n`
  );
}

export function noAddressNotice(cluster: string): string {
  return (
    `// No https address is known for ${cluster}.\n` +
    `// Give the cluster a domain in ~/.memql/clusters.yaml and reopen this artifact.\n`
  );
}

export function noContentNotice(cluster: string, fileName: string): string {
  return (
    `// ${cluster} served no content for ${fileName}.\n` +
    `// The artifact row is there and is yours -- the cluster has no downloadable body for it.\n`
  );
}

export function fetchFailedNotice(cluster: string, fileName: string): string {
  return (
    `// ${cluster} could not be read for ${fileName}.\n` +
    `// Reconnect and reopen it; the failure is recorded in the MemQL Connection output channel.\n`
  );
}

export function tooLargeNotice(fileName: string, bytes: number): string {
  return (
    `// ${fileName} is ${formatByteSize(bytes)}, past the ${formatByteSize(ARTIFACT_BUFFER_LIMIT_BYTES)} this editor buffers.\n` +
    `// Open it again from MemQL OS to save it to disk instead.\n`
  );
}

/**
 * `decodeURIComponent` that answers "" instead of throwing.
 *
 * The same tolerant decoder constructs/clusterDocument.ts exports, and a local
 * copy rather than an import ACROSS the two domains: this module's only
 * dependency would otherwise be a construct-browser detail, and a `library/`
 * module reaching into `constructs/` for four lines is a coupling that reads
 * as the two being related when they are not.
 */
function safeDecode(value: string): string {
  try {
    return decodeURIComponent(value);
  } catch {
    return "";
  }
}
