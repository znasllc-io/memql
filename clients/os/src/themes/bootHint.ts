// THE FIRST FRAME WEARS THE PERSON'S THEME (José, Nov 20 2026 call, 33:19-35:28).
//
// A person's theme pack (Graphite, Vellum, Cobalt, or one from the
// marketplace) roams with their desktop, which arrives from the cluster after
// sign-in. Until then the page drew the built-in Graphite ground, so a person
// on another theme saw that colour first and then a switch, which the loading
// screen softened into a fade from black. José: "we shouldn't even have that
// other color ... the default for memql os is graphite ... once the user logs
// in ... and they change it ... when they go back to log in or the loading
// screen, it should follow".
//
// So the last theme this browser showed is remembered here, as its id AND its
// compiled rules (a marketplace pack's CSS only arrives with the desktop), and
// main.tsx puts both on the page before the first paint. With nothing
// remembered the page stays on Graphite, light or dark as the computer is set
// (the light/dark choice is app/theme.ts, already per browser). The desktop
// still decides once it arrives (chrome/state.tsx), and remembers again.
//
// Browser storage is a convenience: a private window or blocked storage just
// starts on Graphite, as before.

import { BUILT_IN_THEME_ID, THEME_STYLE_ID } from "./registry";

const STORAGE_KEY = "memql-os-theme-pack";

interface Remembered { id: string; css: string }

/** Remember the pack on screen, with the rules that draw it ("" for Graphite, the base tokens). */
export function rememberThemePack(id: string, css: string): void {
  try {
    if (id === BUILT_IN_THEME_ID) globalThis.localStorage?.removeItem(STORAGE_KEY);
    else globalThis.localStorage?.setItem(STORAGE_KEY, JSON.stringify({ id, css } satisfies Remembered));
  } catch {
    // Non-fatal: the page starts on Graphite.
  }
}

/** Put the remembered pack on the page before the first paint. Returns its id, or "" for none. */
export function applyRememberedThemePack(doc: Document | undefined = globalThis.document): string {
  if (!doc?.documentElement || !doc.head) return "";
  let remembered: Remembered | null = null;
  try {
    const raw = globalThis.localStorage?.getItem(STORAGE_KEY) ?? null;
    const parsed: unknown = raw ? JSON.parse(raw) : null;
    if (parsed && typeof parsed === "object" && typeof (parsed as Remembered).id === "string" && typeof (parsed as Remembered).css === "string") {
      remembered = parsed as Remembered;
    }
  } catch {
    return "";
  }
  if (!remembered || !remembered.id || remembered.id === BUILT_IN_THEME_ID) return "";
  // The same style element the installed packs are written to
  // (registry.applyPackStyles), so the desktop's own write replaces this one
  // rather than leaving two copies of a pack's rules on the page.
  let style = doc.getElementById(THEME_STYLE_ID) as HTMLStyleElement | null;
  if (!style) {
    style = doc.createElement("style");
    style.id = THEME_STYLE_ID;
    doc.head.appendChild(style);
  }
  style.textContent = remembered.css;
  doc.documentElement.setAttribute("data-os-theme", remembered.id);
  return remembered.id;
}
