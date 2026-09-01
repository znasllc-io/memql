// Turning the two Library reads into the one record every delivery decision is
// made from (memql#4748).
//
// TWO ROWS, BECAUSE THE INDEX OWNS NO CONTENT. `v1:library:artifact` is a thin
// index row (memql#693) carrying the provenance spine and a `sourceConceptRef`
// pointing at whatever actually holds the item; for `kind == "file"` that is a
// `v1:library:file`, which is where the NAME, the MIME TYPE and -- the one this
// epic turns on -- the SIZE live. So a file-backed artifact costs a second
// read, and everything else costs one.
//
// A MISSING SECOND ROW IS NOT A FAILURE. `libraryFileById` is owner-gated like
// every other Library read, so it answers with nothing for a row that is not
// the caller's -- which cannot happen here, since the index row was just read
// under the same actor -- and it also answers with nothing on a cluster where
// the file row has been archived out from under the index. Either way the
// index row's own `title` / `format` / `mimeType` are still true, and opening
// on those beats refusing over a field that only ever refines the answer.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #4748

import { buildLibraryArtifactById, buildLibraryFileById } from "@znasllc-io/memql-sdk-core/client";

import type { ArtifactMeta } from "./artifactDocument.js";

/** How a `kind == "file"` artifact names its backing row. */
export const LIBRARY_FILE_REF_PREFIX = "v1:library:file:";

/** A row as the query client materializes one: field name to value. */
export type RowLike = Record<string, unknown>;

/**
 * The file id a `sourceConceptRef` names, or undefined when it names something
 * else.
 *
 * PREFIX-MATCHED RATHER THAN SPLIT ON COLONS. The reference is a canonical node
 * id, whose concept part is itself colon-separated, so "the last segment" is
 * only the id by coincidence of that concept having exactly three parts. Asking
 * whether it starts with the one prefix this function is about says what is
 * meant and cannot be fooled by a concept with a different shape.
 */
export function fileIdFromSourceRef(sourceConceptRef: string): string | undefined {
  const ref = sourceConceptRef.trim();
  if (!ref.startsWith(LIBRARY_FILE_REF_PREFIX)) return undefined;
  const id = ref.slice(LIBRARY_FILE_REF_PREFIX.length).trim();
  return id === "" ? undefined : id;
}

/**
 * The metadata an artifact opens on, composed from the index row and (when
 * there is one) its backing file row.
 *
 * THE FILE ROW WINS WHERE IT SPEAKS. The index copies `format` and `mimeType`
 * from the backing row at promotion time and is not re-stamped afterwards, so
 * where the two disagree the backing row is the one describing the bytes that
 * are about to be streamed. Where it says nothing -- an empty mime type, no
 * size -- the index's copy stands.
 */
export function artifactMetaFrom(
  artifactId: string,
  artifactRow: RowLike,
  fileRow: RowLike | undefined,
): ArtifactMeta {
  const size = numberField(fileRow, "size");
  return {
    artifactId,
    title: stringField(artifactRow, "title"),
    kind: stringField(artifactRow, "kind"),
    format: firstNonBlank(stringField(fileRow, "format"), stringField(artifactRow, "format")),
    mimeType: firstNonBlank(stringField(fileRow, "mimeType"), stringField(artifactRow, "mimeType")),
    fileName: stringField(fileRow, "name"),
    ...(size === undefined ? {} : { sizeBytes: size }),
  };
}

/** Whether the artifact row says it has been archived. */
export function isArchived(artifactRow: RowLike): boolean {
  const value = artifactRow.archived;
  // A row written before the field existed has no key at all, and absent means
  // NOT archived -- the same reading the DSL's own `archived != true` filter
  // takes, and the reason this asks for `=== true` rather than for truthiness.
  return value === true;
}

/**
 * What resolving metadata needs from a connection: named query dispatch.
 *
 * STRUCTURAL, the way run/orchestrator.ts declares the same capability. A real
 * `QueryClient` satisfies it, and a plain object satisfies it in a test -- which
 * is what lets the rendered call strings be asserted without a cluster.
 */
export interface ArtifactMetaReader {
  executeNamed(name: string, call: string): Promise<{ rows(): RowLike[] }>;
}

export type ArtifactLookup = { found: true; meta: ArtifactMeta; archived: boolean } | { found: false };

/**
 * Reads an artifact's metadata, and its backing file row when it has one.
 *
 * THE CALLS ARE RENDERED BY THE GENERATED BUILDERS, not hand-written. A query
 * reaches the engine as MemQL TEXT, so a hand-built call string is a parse
 * failure waiting for production: `buildLibraryArtifactById` is the generated
 * renderer that knows how to quote an id containing colons, and dispatching it
 * through `executeNamed` is exactly what the generated method on QueryClient
 * does -- named here rather than through the prototype augmentation so the
 * dependency is an ordinary import rather than a module side effect.
 *
 * NOT FOUND AND NOT YOURS ARE THE SAME ANSWER, and that is the graph's doing
 * rather than a choice made here: `libraryArtifactById` filters on
 * `ownerUserId == actor.userId`, so a row belonging to someone else comes back
 * as no rows. The caller must not report the difference either -- there is
 * none to report.
 */
export async function resolveArtifactMeta(reader: ArtifactMetaReader, artifactId: string): Promise<ArtifactLookup> {
  const artifactResult = await reader.executeNamed(
    "libraryArtifactById",
    buildLibraryArtifactById({ artifactId }),
  );
  const artifactRow = artifactResult.rows()[0];
  if (artifactRow === undefined) return { found: false };

  let fileRow: RowLike | undefined;
  const fileId = fileIdFromSourceRef(stringField(artifactRow, "sourceConceptRef"));
  if (stringField(artifactRow, "kind") === "file" && fileId !== undefined) {
    // A file row that cannot be read leaves the index row's own answer standing
    // -- see the header. The read is still allowed to THROW: a transport
    // failure is not "the row is absent", and the caller classifies it.
    const fileResult = await reader.executeNamed("libraryFileById", buildLibraryFileById({ fileId }));
    fileRow = fileResult.rows()[0];
  }

  return {
    found: true,
    meta: artifactMetaFrom(artifactId, artifactRow, fileRow),
    archived: isArchived(artifactRow),
  };
}

function stringField(row: RowLike | undefined, key: string): string {
  if (row === undefined) return "";
  const value = row[key];
  return typeof value === "string" ? value.trim() : "";
}

/**
 * A numeric field, tolerating the string a JSON round trip can leave behind.
 *
 * NOT COERCED TO ZERO. A size that cannot be read is `undefined` -- "the row
 * did not say" -- because the cap treats a stated 0 and an unstated size
 * completely differently, and quietly turning one into the other would pass the
 * cap by accident.
 */
function numberField(row: RowLike | undefined, key: string): number | undefined {
  if (row === undefined) return undefined;
  const value = row[key];
  if (typeof value === "number") return Number.isFinite(value) ? value : undefined;
  if (typeof value === "string" && value.trim() !== "") {
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : undefined;
  }
  return undefined;
}

function firstNonBlank(...candidates: string[]): string {
  for (const candidate of candidates) {
    if (candidate !== "") return candidate;
  }
  return "";
}
