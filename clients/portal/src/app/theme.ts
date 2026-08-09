// Theme selection: the small amount of imperative code the CSS token layer
// (src/styles/tokens.css) cannot express.
//
// The tokens resolve three states -- "light", "dark", and "whatever the OS
// says". CSS handles the third on its own via prefers-color-scheme; this
// module exists only to write the `data-theme` attribute that overrides it,
// and to remember the choice.
//
// applyStoredTheme() runs at MODULE IMPORT time (main.tsx imports it before
// createRoot) rather than in an effect. An effect runs after the first paint,
// which is one frame of the wrong palette on every load -- the flash the
// attribute exists to prevent.

export type ThemeChoice = "light" | "dark" | "system";

const STORAGE_KEY = "memql-portal-theme";

function isThemeChoice(value: string | null): value is ThemeChoice {
  return value === "light" || value === "dark" || value === "system";
}

export function readStoredTheme(): ThemeChoice {
  // localStorage throws in a hardened browser profile (Safari private mode
  // historically, any origin with storage blocked today). A theme preference
  // is not worth failing boot over, so an unreadable store means "system".
  try {
    const raw = globalThis.localStorage?.getItem(STORAGE_KEY) ?? null;
    return isThemeChoice(raw) ? raw : "system";
  } catch {
    return "system";
  }
}

// applyTheme writes the override attribute. "system" REMOVES it rather than
// writing "system": the token layer keys off the attribute's absence to fall
// through to prefers-color-scheme, so a literal value there would pin the
// light palette on a dark OS.
export function applyTheme(choice: ThemeChoice): void {
  const root = globalThis.document?.documentElement;
  if (!root) return;
  if (choice === "system") {
    root.removeAttribute("data-theme");
  } else {
    root.setAttribute("data-theme", choice);
  }
}

export function storeTheme(choice: ThemeChoice): void {
  try {
    globalThis.localStorage?.setItem(STORAGE_KEY, choice);
  } catch {
    // Non-fatal, same reasoning as readStoredTheme.
  }
}

export function setTheme(choice: ThemeChoice): void {
  applyTheme(choice);
  storeTheme(choice);
}

export function applyStoredTheme(): ThemeChoice {
  const choice = readStoredTheme();
  applyTheme(choice);
  return choice;
}
