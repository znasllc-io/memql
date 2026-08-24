// Offering the MemQL editor theme, once (memql#4421, D4).
//
// WHY AN OFFER EXISTS AT ALL. memql#4420 ships two colour themes because the
// workbench chrome -- tree rows, activity bar, tabs, status bar -- can only
// wear the brand if VS Code's OWN theme is a MemQL theme. But a theme sitting
// unmentioned in the picker is a theme nobody finds, and the operator most
// affected is the one who just noticed their MemQL panels look different from
// the sidebar around them. So: one message, at most once, ever.
//
// AND WHY IT IS AN OFFER RATHER THAN A DEFAULT. Changing somebody's editor
// theme without asking is hostile -- it is the most visible setting in the
// application, and an extension that moves it has broken an expectation no
// amount of branding is worth. The extension never switches without the click.
//
// THE CASES THAT MUST NOT OFFER are the substance here:
//   * already answered -- either way. "Not now" that returns tomorrow is not
//     "not now", it is "every day", and that is how a prompt becomes one people
//     dismiss without reading. src/auth/passkeyOffer.ts's header makes the same
//     point about the same failure.
//   * already on a MemQL theme -- there is nothing to offer.
//   * high contrast -- an accessibility choice, and neither MemQL theme is a
//     high-contrast theme. Inviting somebody out of high contrast is the one
//     version of this prompt that could do actual harm. Same stance D1 takes
//     for the panels: high contrast wins.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go):
// the decision is the part worth testing, and src/extension.ts supplies the
// notification, the globalState and the settings write.

import { COLOR_THEME_KIND } from "../webview/appearance.js";
import { THEME_NAMES } from "./editorThemes.js";

/**
 * The globalState key recording that the operator has answered.
 *
 * globalState rather than workspaceState: the answer is about this person's
 * editor, not about a folder they happen to have open, and asking again per
 * repository is exactly the nagging D4 forbids.
 */
export const THEME_OFFER_ANSWERED_KEY = "memql.themeOffer.answered";

/** The notification body. Names what the operator GAINS, not what is wrong. */
export const OFFER_MESSAGE =
  "MemQL panels already follow your appearance setting. For the full MemQL look -- the sidebar, tree rows, tabs and status bar too -- switch to the MemQL Dark or MemQL Light colour theme.";

/** The accept action. */
export const OFFER_SWITCH = "Switch";

/** The decline action. Recorded exactly like an accept: both are an answer. */
export const OFFER_DISMISS = "Not now";

/** What the offer needs to know, all of it read by the caller from the editor. */
export interface ThemeOfferInputs {
  /** Has this operator already answered? Read from globalState. */
  answered: boolean;
  /** The `workbench.colorTheme` label currently in effect. */
  activeThemeLabel: string;
  /** The editor's ColorThemeKind, mapped through src/webview/theme.ts. */
  editorKind: number;
}

/**
 * Is the label one of OURS?
 *
 * Exact match, not a prefix or a substring. `workbench.colorTheme` holds a
 * theme's label verbatim, and a fuzzy match would treat somebody else's
 * "MemQL Dark Pro" as ours and suppress the offer for a theme we did not ship.
 */
export function isMemqlTheme(label: string): boolean {
  return label === THEME_NAMES.dark || label === THEME_NAMES.light;
}

/**
 * Which MemQL theme suits the editor's current kind.
 *
 * Matching MATTERS: someone on a light editor who accepts must land on MemQL
 * Light. Offering the dark one and switching them into it is a takeover
 * wearing a button. An unrecognised kind resolves to dark, the same direction
 * appearance.ts takes and for the same reason -- it is the safer of the two to
 * be wrong about.
 */
export function memqlThemeFor(editorKind: number): string {
  return editorKind === COLOR_THEME_KIND.light ? THEME_NAMES.light : THEME_NAMES.dark;
}

/** Whether to show the offer at all. See the header for each `false`. */
export function shouldOfferMemqlTheme(inputs: ThemeOfferInputs): boolean {
  if (inputs.answered) return false;
  if (isMemqlTheme(inputs.activeThemeLabel)) return false;
  // Stated in the POSITIVE, which is what closes the unknown-kind hole: offer
  // only when we KNOW the editor is plain light or plain dark. Written as
  // "not high contrast" it would offer under any kind this build has never
  // heard of, and the last kind VS Code added was HighContrastLight -- a
  // second high-contrast variant. So the likeliest unknown kind is exactly
  // the one where offering does harm.
  //
  // The asymmetry with effectiveTheme is deliberate. There a forced setting
  // wins over an unknown kind, because the user said what they wanted. Here
  // nobody has said anything and WE are initiating, so declining costs a
  // nicety -- the themes are still one keystroke away in the picker -- while
  // offering wrongly invites somebody out of high contrast.
  return (
    inputs.editorKind === COLOR_THEME_KIND.light || inputs.editorKind === COLOR_THEME_KIND.dark
  );
}
