import { recipeKeeps, type CompositionRow, type SourceRow } from "./rows";
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
  /**
   * Drawn as a label rather than a button. Two buttons side by side ask to be
   * weighed against each other, and the way back to another app is not an
   * alternative to anything done here: it is a way out of the page.
   */
  text?: boolean;
}

export type ActId =
  | "materialize"
  | "stop"
  | "openFile"
  | "openGoal"
  | "openDeployables"
  | "saveRecipe"
  | "archive"
  | "restore"
  | "startOver"
  | "returnToApp";

/** The composer's own state, before anything has been written. */
export interface DraftState {
  /** How many sources are selected. */
  sourceCount: number;
  /** A non-empty brief can stand on its own. */
  hasContent: boolean;
  /** Whether a format has been chosen. */
  hasFormat: boolean;
  hasOrganization: boolean;
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
 *
 * `backTo` is the name of the app that sent the person here to make this
 * file, and is given only when they can go back to it. The way back is an
 * act of a FINISHED file, offered beside the one that opens it: before there
 * is a file there is nothing to come back from, and a composition that failed
 * or was stopped is not the errand the other app sent them on.
 *
 * `sources` are the composition's own (what its record says it was made from).
 * A recipe keeps the brief and the sources it can select again, and a recipe
 * run refuses one with neither, so Save as recipe is offered only where there
 * is one or the other: never on a blank file.
 */
export function actsFor(c: CompositionRow | null, draft: DraftState, backTo?: string, sources: SourceRow[] = []): ActSpec[] {
  // No composition yet: this is the compose form, and the only act is to
  // run it. It is ABSENT rather than disabled while there is nothing to
  // compose from, so the bar's state line is what explains the wait.
  if (c === null) {
    if ((draft.sourceCount === 0 && !draft.hasContent) || !draft.hasFormat || !draft.hasOrganization) return [];
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
      // act on a finished materialization is the next thing somebody does
      // with what it made. Archive is the quiet one: filing a record away is
      // housekeeping, and it is the act somebody reaches for least often
      // from this state.
      acts.push({ id: "archive", label: "Archive", tone: "quiet" });
      if (c.deployableKind) {
        // A PACKAGE'S NEXT STEP IS A DEPLOY, NOT A DOWNLOAD. The zip is in
        // the Library and openFile still reaches it, quietly -- but nobody
        // materializes a package source in order to read it.
        //
        // SAVE AS RECIPE IS ABSENT HERE, and that is the three-act limit
        // forcing a real choice rather than an arbitrary trim. A document's
        // repeatability is a recipe; a deployable's is the Deployables
        // pipeline's own redeploy, which rebuilds from the SOURCE and is
        // what that app exists to do. Offering both would be two answers to
        // one question, in the app that owns neither.
        if (c.outputFileId) acts.push({ id: "openFile", label: "Open the file", tone: "quiet" });
        acts.push({ id: "openDeployables", label: "Deploy it", tone: "primary" });
        break;
      }
      // ABSENT WHERE THE RECIPE COULD NOT RUN: with no brief and nothing a
      // recipe selects again (a blank file; rows picked one by one), the saved
      // recipe would be refused the first time it ran.
      if (!c.recipeId && (c.statement.trim() !== "" || sources.some(recipeKeeps))) {
        acts.push({ id: "saveRecipe", label: "Save as recipe", tone: "default" });
      }
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
  if (backTo !== undefined && c.status === "ready") {
    // In front of the primary act, which stays last.
    acts.splice(Math.max(acts.length - 1, 0), 0, { id: "returnToApp", label: `Back to ${backTo}`, tone: "quiet", text: true });
    // THE BAR HOLDS THREE, and the one that gives way is Archive: housekeeping,
    // and the act somebody who has just made a file reaches for least. It is NOT
    // offered anywhere else meanwhile (the Materialized list's rows carry no acts),
    // so this is Archive absent for as long as the trip is live. It comes back the
    // moment the person presses Compose another, which ends the trip: from then on
    // this composition offers Archive wherever it is opened.
    const archive = acts.findIndex((a) => a.id === "archive");
    if (acts.length > 3 && archive >= 0) acts.splice(archive, 1);
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
    if (draft.sourceCount === 0 && !draft.hasContent) return "Describe what to make or pick a source";
    if (!draft.hasFormat) return "Choose what kind of file to make";
    if (!draft.hasOrganization) return "Choose an organization";
    if (draft.sourceCount === 0) return "Ready to compose your file";
    return `Ready to compose from ${draft.sourceCount} ${draft.sourceCount === 1 ? "source" : "sources"}`;
  }
  if (c.status === "failed") return `${statusWord(c.status)} — ${c.failureReason || "no reason recorded"}`;
  return statusWord(c.status);
}
