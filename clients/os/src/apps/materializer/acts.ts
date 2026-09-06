import type { CompositionRow } from "./rows";
import { isRunning, statusWord } from "./words";

// acts.ts -- DESIGN.md rule 12, as a PURE function so the table is what
// gets asserted rather than a rendered DOM.
//
// AN ILLEGAL ACT IS ABSENT, NEVER DISABLED. That is not a preference, it
// is the bug the Deployables recomposition found: a draft rendered an
// ENABLED archive control the server refuses, with no control anywhere
// that could reach the state the guard demands. Six greyed-out buttons
// are six controls somebody has to read past to learn they are not for
// them; one enabled button the server refuses is worse, because they
// find out by being told no.
//
// AT MOST THREE, PRIMARY LAST.

/** What an act is, before it is a button. */
export interface ActSpec {
  id: ActId;
  label: string;
  tone?: "default" | "primary" | "quiet" | "danger";
}

export type ActId =
  | "materialize"
  | "stop"
  | "openFile"
  | "openGoal"
  | "saveRecipe"
  | "archive"
  | "restore"
  | "startOver";

/** The composer's own state, before anything has been written. */
export interface DraftState {
  /** How many sources are selected. */
  sourceCount: number;
  /** Whether a format has been chosen. */
  hasFormat: boolean;
  /** Whether a materialize call is in flight from this window. */
  submitting: boolean;
}

/**
 * The acts a composition offers, from its state alone.
 *
 * A RUNNING COMPOSITION OFFERS NO MATERIALIZE, which is the case worth
 * stating: re-materializing would open a second composition against the
 * same sources and produce a second file, and nothing on the page would
 * say which one was the deliverable.
 */
export function actsFor(c: CompositionRow | null, draft: DraftState): ActSpec[] {
  // No composition yet: this is the compose form, and the only act is to
  // run it. It is ABSENT rather than disabled while there is nothing to
  // compose from, so the bar's state line is what explains the wait.
  if (c === null) {
    if (draft.sourceCount === 0 || !draft.hasFormat) return [];
    return [{ id: "materialize", label: "Materialize", tone: "primary" }];
  }

  if (isRunning(c.status)) {
    return [{ id: "stop", label: "Stop", tone: "quiet" }];
  }

  const acts: ActSpec[] = [];
  if (c.archived) {
    // An archived RECORD offers exactly one act, and it is the inverse of
    // the one that archived it. Opening its file is still legal and
    // still offered -- archiving the record never touched the file.
    if (c.outputFileId) acts.push({ id: "openFile", label: "Open the file", tone: "quiet" });
    acts.push({ id: "restore", label: "Restore", tone: "default" });
    return acts;
  }

  switch (c.status) {
    case "ready":
      // PRIMARY LAST, because that is where the eye lands -- and the primary
      // act on a finished materialization is opening the thing it made.
      // Archive is the quiet one: filing a record away is housekeeping, and
      // it is the act somebody reaches for least often from this state.
      acts.push({ id: "archive", label: "Archive", tone: "quiet" });
      if (!c.recipeId) acts.push({ id: "saveRecipe", label: "Save as recipe", tone: "default" });
      if (c.outputFileId) acts.push({ id: "openFile", label: "Open the file", tone: "primary" });
      break;
    case "failed":
    case "cancelled":
      // NO RETRY, and the absence is deliberate. A retry would need the
      // sources, the template and the target as they were, and the
      // record holds the sources it RESOLVED rather than the form that
      // produced them -- so "retry" would silently mean something
      // slightly different from what was asked for. Start over opens the
      // composer seeded from this record, where a person can see what
      // they are re-running before they run it.
      acts.push({ id: "startOver", label: "Start over from this", tone: "default" });
      acts.push({ id: "archive", label: "Archive", tone: "quiet" });
      break;
    default:
      acts.push({ id: "archive", label: "Archive", tone: "quiet" });
      break;
  }
  return acts.slice(0, 3);
}

/**
 * The bar's state line: the state in words, plus the one fact that makes
 * it actionable.
 *
 * WHILE THERE IS NOTHING TO COMPOSE FROM, THIS IS WHERE THE WAIT IS
 * EXPLAINED. The act is absent, so without a line here the bar would be
 * empty and a person would have no idea what the app wanted.
 */
export function stateLine(c: CompositionRow | null, draft: DraftState): string {
  if (c === null) {
    if (draft.sourceCount === 0) return "Pick at least one source to compose from";
    if (!draft.hasFormat) return "Choose what kind of file to make";
    return `Ready to compose from ${draft.sourceCount} ${draft.sourceCount === 1 ? "source" : "sources"}`;
  }
  if (c.status === "failed") return `${statusWord(c.status)} — ${c.failureReason || "no reason recorded"}`;
  return statusWord(c.status);
}
