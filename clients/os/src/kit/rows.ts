import type { Row } from "@znasllc-io/memql-sdk-core/client";

// Reading a wire row, before any app-specific projection.
//
// Promoted from the third copy (memql#4721): fleet, users and deployables each
// hand-rolled `flatten`, and users and deployables each hand-rolled `boolOr`,
// which is two past the kit's own rule -- a helper earns a place here when a
// SECOND surface needs it. The copies had not drifted yet; this is the move
// that keeps it that way.

/**
 * Unwrap a `payload`-nested row to the flat form the field helpers read.
 *
 * A row reaches a projection from two places -- a SEED (already
 * shape-flattened) and the SUBSCRIPTION fold (a CDC envelope whose concept
 * fields sit inside `payload`) -- and the two have to produce the same object,
 * or a row renders one way on load and another way the moment anything about
 * it changes. The envelope wins on a collision so `id` stays the ROW's id
 * rather than any `id` the payload happens to carry.
 */
export function flatten(row: Row): Row {
  const nested = row["payload"];
  if (nested && typeof nested === "object" && !Array.isArray(nested)) {
    return { ...(nested as Row), ...row };
  }
  return row;
}

/**
 * A boolean field that can be ABSENT, with the default the concept declares.
 *
 * The SDK's `rowBool` returns `false` for a missing key, which collapses
 * "absent" and "explicitly false" into one answer. A folded CDC event carries
 * only what the write touched, so reading through one helper that STATES the
 * default keeps a field whose default is true from silently inheriting the
 * wrong one.
 */
export function boolOr(row: Row, key: string, fallback: boolean): boolean {
  const v = row[key];
  return typeof v === "boolean" ? v : fallback;
}

/** A string-array field: keeps string members, drops everything else. */
export function stringsOf(row: Row, key: string): string[] {
  const v = row[key];
  if (!Array.isArray(v)) return [];
  return v.filter((m): m is string => typeof m === "string");
}
