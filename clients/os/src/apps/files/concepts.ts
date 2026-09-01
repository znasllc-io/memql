// The two concepts this app reads, and the closed value sets its filters
// offer. The enums mirror `dsl/library/concepts.memql` -- the filter chips
// must offer exactly the concept's own values (epic #4721 AC), and a mirrored
// list is the only way a browser can offer them before any row exists.

export const ARTIFACT_CONCEPT = "v1:library:artifact";
export const FOLDER_CONCEPT = "v1:library:folder";

/**
 * The kinds this app shows (design D2): content-bearing only. Notes, todos,
 * calendar events, memories and live sources stay indexed and wait for their
 * own surface -- they never render here, under any filter combination.
 */
export const CONTENT_KINDS = ["file", "document", "generated_output"] as const;

/** The artifact concept's own `source` enum, verbatim and in its order. */
export const SOURCE_VALUES = [
  "uploaded",
  "exported",
  "workbench_generated",
  "computer_use",
  "agent_generated",
  "derived",
  "user_created",
  "live",
] as const;
