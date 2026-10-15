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

/** The finished record's acts; making belongs to the wizard. */
export function pageActs(
  c: CompositionRow,
  options: { backTo?: string; recipeCanRun: boolean; recipeSaved: boolean },
): ActSpec[] {
  if (c.archived)
    return [
      ...(c.outputFileId
        ? [{ id: "openFile" as const, label: "Open in editor", text: true }]
        : []),
      { id: "restore", label: "Restore", tone: "primary" },
    ];
  if (isRunning(c.status)) return [];
  if (c.status === "failed" || c.status === "cancelled")
    return [
      { id: "archive", label: "Archive", text: true },
      { id: "startOver", label: "Start over", tone: "primary" },
    ];
  if (c.status !== "ready")
    return [{ id: "archive", label: "Archive", text: true }];
  const wayBack: ActSpec = options.backTo
    ? { id: "returnToApp", label: `Back to ${options.backTo}`, text: true }
    : { id: "archive", label: "Archive", text: true };
  if (c.deployableKind)
    return [
      wayBack,
      ...(c.outputFileId
        ? [{ id: "openFile" as const, label: "Download", text: true }]
        : []),
      { id: "openDeployables", label: "Deploy it", tone: "primary" },
    ];
  const save = options.recipeCanRun && !options.recipeSaved && !c.recipeId;
  return [
    wayBack,
    ...(save
      ? [{ id: "saveRecipe" as const, label: "Save as recipe", text: true }]
      : options.backTo
        ? [{ id: "archive" as const, label: "Archive", text: true }]
        : []),
    ...(c.outputFileId
      ? [
          {
            id: "openFile" as const,
            label: "Open in editor",
            tone: "primary" as const,
          },
        ]
      : []),
  ];
}

export function pageState(c: CompositionRow): {
  word: string;
  detail?: string;
} {
  if (c.archived)
    return { word: "Archived", detail: "The file is still in Files." };
  return {
    word: statusWord(c.status),
    ...(c.status === "failed"
      ? { detail: c.failureReason || "No reason recorded." }
      : {}),
  };
}
