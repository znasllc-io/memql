// What the Training app is about: the two populations it reads, the one route
// it writes bytes to, and the closed lists it owns.
//
// Naming a concept id here is what a DESIGNED surface does -- this app is
// about the caller's analysis plans and about knowledge chunks specifically,
// not about whatever concept a browser happened to be pointed at.

/** The analysis work, live (`dsl/planner/concepts.memql`). */
export const PLAN_CONCEPT = "v1:planner:plan";

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
 * The one `v1:planner:plan.kind` the attachment route stamps
 * (`CreateQueuedAnalyzePlan`, `component/server/plan_store.go`).
 *
 * Everything else on that concept is other work the same person may have
 * running -- a userGoal, a trainSpecialist run -- and this app shows analysis,
 * so the kind is a filter rather than a label.
 */
export const ANALYZE_PLAN_KIND = "analyzeFile";

/**
 * The plan statuses this surface treats as finished.
 *
 * Read as a SET rather than as "not queued and not running": the nine-value
 * status enum carries parked states (`awaitingFeedback`, `waitingForSlot`)
 * that are neither running nor over, and calling those terminal would retire a
 * plan on screen that is still waiting for somebody.
 */
export const TERMINAL_PLAN_STATUSES: readonly string[] = [
  "succeeded",
  "failed",
  "cancelled",
];

// ---------------------------------------------------------------------------
// Upload
// ---------------------------------------------------------------------------

/**
 * The MIME types the analyzer accepts, mirroring `allowedAttachmentMIMETypes`
 * (`component/server/attachment_handler.go`).
 *
 * MIRRORED, NOT AUTHORITATIVE. The handler refuses an unsupported type with
 * its own 415 and its own sentence whatever this list says; the list is here
 * so the file picker offers what stands a chance rather than letting somebody
 * choose a `.zip` and learn about it from a refusal. When the two disagree the
 * SERVER is right, and its sentence is what renders -- which is why a file the
 * picker would not have offered is still uploaded rather than blocked here.
 *
 * `application/octet-stream` is deliberately absent for the reason it is
 * absent server-side: it is what a file with no usable type falls back to, so
 * accepting it would turn "the analyzer reads these" into "anything".
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

/** `maxAttachmentBytes` (`component/server/attachment_handler.go`), mirrored
 *  for the same reason and with the same caveat: the server decides. */
export const MAX_ATTACHMENT_BYTES = 25 * 1024 * 1024;

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
