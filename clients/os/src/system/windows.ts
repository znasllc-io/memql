// A window is one running app on a desk. Window state is session-only --
// the DesktopStore persists desks, items and pins, never windows (spec D11):
// reopening the OS lands on your desks, not on half-restored sessions.

export type AppId = string;
export type WindowId = string;
export type DeskId = string;

export type WindowMode = "normal" | "minimized" | "fullscreen";

export interface OsWindow {
  id: WindowId;
  appId: AppId;
  mode: WindowMode;
  /** The app section the window is showing (manifest sections; "" = none). */
  sectionId: string;
}

export function newWindow(id: WindowId, appId: AppId, sectionId: string): OsWindow {
  return { id, appId, mode: "normal", sectionId };
}

export function isVisible(win: OsWindow): boolean {
  return win.mode !== "minimized";
}
