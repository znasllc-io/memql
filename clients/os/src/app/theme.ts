export type ThemeChoice = "light" | "dark" | "system";

const STORAGE_KEY = "memql-os-theme";

function isThemeChoice(value: string | null): value is ThemeChoice {
  return value === "light" || value === "dark" || value === "system";
}

export function readStoredTheme(): ThemeChoice {
  try {
    const raw = globalThis.localStorage?.getItem(STORAGE_KEY) ?? null;
    return isThemeChoice(raw) ? raw : "system";
  } catch {
    return "system";
  }
}

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
    // Non-fatal: a theme preference is not worth failing boot over.
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
