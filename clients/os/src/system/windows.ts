// A window is one running app on a desk. Window state is session-only --
// the DesktopStore persists desks, items and pins, never windows (spec D11):
// reopening the OS lands on your desks, not on half-restored sessions.

export type AppId = string;
export type WindowId = string;
export type DeskId = string;

export type WindowMode = "normal" | "minimized" | "fullscreen";

/**
 * A consumable instruction carried to an app by its window (epic memql#4842,
 * #4845): "open showing THIS" -- a desk folder double-click landing the Files
 * window on that folder is the first sender. The payload is OPAQUE to the
 * shell; the id is minted per send so a repeated identical payload is still a
 * new instruction, and consumption matches on it so a consume racing a newer
 * send can never eat the newer intent.
 *
 * Session-only by construction: windows never persist (spec D11), so an
 * intent needs no store version and never roams.
 */
export interface WindowIntent {
  id: string;
  payload: Record<string, unknown>;
}

export interface OsWindow {
  id: WindowId;
  appId: AppId;
  mode: WindowMode;
  /** The app section the window is showing (manifest sections; "" = none). */
  sectionId: string;
  /** A standing instruction the app has not yet consumed. */
  intent?: WindowIntent;
}

export function newWindow(id: WindowId, appId: AppId, sectionId: string, intent?: WindowIntent): OsWindow {
  return { id, appId, mode: "normal", sectionId, ...(intent ? { intent } : {}) };
}

export function isVisible(win: OsWindow): boolean {
  return win.mode !== "minimized";
}
