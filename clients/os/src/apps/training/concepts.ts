// What the Training app is about: the three populations it reads, the one
// route it writes bytes to, and the closed lists it owns.
//
// ===========================================================================
// THE APP TEACHES FROM THE LIBRARY NOW, NOT FROM A SPACE (spec section G)
// ===========================================================================
// It used to upload into the caller's daily cognition space and watch
// `v1:planner:plan` rows. That path taught nothing: the plan's completion
// wrote a `v1:knowledge:document` through a mutation declared in no `.memql`
// file, with the error swallowed at the call site, so an upload produced a
// summary and NO knowledge chunks -- the app's own headline had never been
// true.
//
// The Library route is the one that works. A file lands as `v1:library:file`,
// the analysis pass reads it into `v1:library:fileChunk`, and
// `libraryTrainFile(fileId, domainId)` is the SECOND, deliberate act that
// ingests those chunks into a knowledge domain as `v1:knowledge:documentChunk`
// rows with `source: "fileUpload"` -- which is exactly what the Review queue
// on the next section is for. Upload and teach are two acts, and the surface
// says so.

/** The files, live (`dsl/library/concepts.memql`). */
export const FILE_CONCEPT = "v1:library:file";

/** The analysis work, live (`dsl/work/concepts.memql`). */
export const RUN_CONCEPT = "v1:work:run";

/**
 * What the pipeline learned (`dsl/knowledge/concepts.memql`).
 *
 * The knowledge namespace declares exactly ONE concept, and this is it.
 * `document`, `domainEntitySchema`, `entityIndex` and `validationEvent` are
 * named in prose across the repo and declared in no `.memql` file -- so the
 * reviewable unit this engine has is the CHUNK, and the entity-level
 * confirmation flow ("MemQL identified a new customer -- should I add it?")
 * belongs to the engine work that would declare those concepts. This app must
 * not fake them.
 */
export const CHUNK_CONCEPT = "v1:knowledge:documentChunk";

/**
 * The one `v1:work:run.automationName` the Library's analysis pass writes
 * (`integrations/library/analysis.go`, `AnalysisTemplate`).
 *
 * A FILTER THIS APP OWNS, not a narrowing of somebody else's query.
 * `workRunsForOwner` deliberately returns every run the caller owns, because
 * Nexus wants every run of a goal; this app shows analyses, so it picks them
 * out here. The constant is duplicated from Go on purpose and the Go side is
 * authoritative -- when they disagree the list goes empty rather than wrong,
 * which is a visible failure rather than a silent one.
 */
export const ANALYSIS_TEMPLATE = "libraryAnalyzeFile";

/**
 * `v1:library:file.status`, which is the axis this surface is built on.
 *
 * `stored` is the moment the bytes became durable and before the pass has
 * touched them. It is deliberately NOT shown as a state of its own: it lasts
 * milliseconds on a readable file, and a row that flickered through "stored"
 * on its way to "reading" would be a state nobody could act on.
 */
export const FILE_STORED = "stored";
export const FILE_ANALYZING = "analyzing";
export const FILE_READY = "ready";
export const FILE_FAILED = "failed";

/** `v1:library:file.embeddingStatus`. `complete` on a file with no text means
 *  there was nothing to embed, which is why `passages` is what the surface
 *  counts rather than this. */
export const EMBEDDING_NONE = "none";

/**
 * What a person is looking at, derived from the file row and its run.
 *
 * SIX STATES, and each one offers exactly one act or none (the OS's rule 12:
 * an act that is not legal is ABSENT, never disabled). They are derived
 * rather than stored because no single row holds them: `uploading` is this
 * browser's own knowledge, `reading` is the run, `unreadable` is the file row
 * plus the run's outcome, and `trained` is a list on the file row.
 */
export type FileStage =
  | "uploading"
  | "reading"
  | "unreadable"
  | "untrained"
  | "trained"
  | "failed";

// ---------------------------------------------------------------------------
// Upload
// ---------------------------------------------------------------------------

/**
 * The types the analysis pass can read, mirroring
 * `component/fileprocessor`'s own set.
 *
 * MIRRORED, NOT AUTHORITATIVE, and the Library route is more permissive than
 * this list: it accepts ANY type and stores it, and a type it cannot read
 * becomes a stored file with nothing to teach. So this list is what the
 * PICKER offers -- the types worth uploading here -- while a file dragged in
 * from elsewhere is still accepted and still lands in the Library. The
 * surface says which happened rather than refusing the drop.
 */
export const ACCEPTED_UPLOAD_TYPES: readonly string[] = [
  "application/pdf",
  "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
  "text/csv",
  "application/csv",
  "text/tab-separated-values",
  "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
  "application/vnd.ms-excel",
  "application/json",
  "text/plain",
  "text/markdown",
  "text/html",
  "image/jpeg",
  "image/png",
  "image/gif",
  "image/webp",
];

/** The `accept` attribute for the file input, from the same one list. */
export const UPLOAD_ACCEPT_ATTRIBUTE = ACCEPTED_UPLOAD_TYPES.join(",");

/**
 * `DefaultLibraryMaxUploadBytes` (`component/server/artifact_handler.go`),
 * mirrored for the caption and nothing else -- the server decides, and a file
 * over its own ceiling comes back with the server's own sentence.
 *
 * FOUR GIB, not the twenty-five MEGABYTES the space attachment route allowed.
 * That is the re-key's most visible gain and it is worth stating on screen:
 * the Library route chunks anything over 32 MiB into a resumable session, so
 * a re-drop after a dropped connection uploads only what is missing.
 */
export const MAX_UPLOAD_BYTES = 4 * 1024 * 1024 * 1024;

// ---------------------------------------------------------------------------
// Chunks
// ---------------------------------------------------------------------------

/** `documentChunk.validationStatus` -- the queue's own axis. */
export const UNVALIDATED = "unvalidated";
export const VALIDATED = "validated";
export const REJECTED = "rejected";

/** The two members `setChunkValidationStatus` accepts. There is no
 *  "unvalidated" decision: a chunk reaches the queue by default. */
export type ChunkDecision = typeof VALIDATED | typeof REJECTED;

/**
 * What each `documentChunk.source` member MEANS, in a reader's terms.
 *
 * The enum value itself is rendered too, in the data voice, because it is the
 * word somebody greps for; this is the sentence beside it. An UNRECOGNISED
 * value renders as itself with no sentence rather than as "unknown" -- a
 * newer engine writing a seventh member is a thing this app should show, not
 * a thing it should erase.
 */
export const SOURCE_MEANINGS: Readonly<Record<string, string>> = {
  fileUpload: "extracted from a file somebody uploaded",
  trainerAgent: "distilled by the Trainer Agent from sources it consulted",
  augment: "added from a chat, to close a retrieval gap",
  llmSeeded: "generated when the domain was first seeded",
  crossDomainBridge: "written to bridge two of an agent's domains",
  appStructure: "the product UI corpus, cited silently",
};

export function sourceMeaning(source: string): string {
  return SOURCE_MEANINGS[source] ?? "";
}

/**
 * The group a chunk with no `documentId` belongs to.
 *
 * `documentId` is populated for chunks the analyzer produced from an upload
 * and null for seeded corpus content, and the concept's own comment records
 * that `v1:knowledge:document` is declared nowhere -- so this is a LABEL for
 * an absent back-reference, never a lookup that failed.
 */
export const CORPUS_GROUP_ID = "";
export const CORPUS_GROUP_LABEL = "Seeded corpus";
