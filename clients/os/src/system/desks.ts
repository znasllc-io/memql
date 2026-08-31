// The desk machine: desks hold at most DESK_CAP windows; opening past the
// cap spills onto a new desk created immediately after the current one
// (spec D2). Pure reducers over a serializable ShellState -- no React, no
// DOM, no ids from the engine. Every action returns the next state plus a
// typed effect so the chrome can play the right cue without re-deriving
// what happened.

import { isVisible, newWindow, type AppId, type DeskId, type OsWindow, type WindowId } from "./windows";

export const DESK_CAP = 2;

export interface Desk {
  id: DeskId;
  /** "user" desks never garbage-collect; "auto" desks do when left empty. */
  createdBy: "user" | "auto";
  /** Ordered window ids; index 0 renders left (or solo/centered). */
  windows: WindowId[];
}

export interface ShellState {
  desks: Desk[];
  activeDeskId: DeskId;
  windows: Record<WindowId, OsWindow>;
  focusedWindowId: WindowId | null;
}

export type ShellEffect =
  | { kind: "placed"; deskId: DeskId; windowId: WindowId }
  | { kind: "focused-existing"; deskId: DeskId; windowId: WindowId }
  | { kind: "spilled"; deskId: DeskId; windowId: WindowId }
  | { kind: "refused-full"; deskId: DeskId }
  | { kind: "none" };

export interface ShellResult {
  state: ShellState;
  effect: ShellEffect;
}

let counter = 0;
/** Local, session-scoped ids. Deterministic prefix keeps tests readable. */
export function nextId(prefix: string): string {
  counter += 1;
  return `${prefix}-${counter}`;
}
export function resetIdsForTest(): void {
  counter = 0;
}

/**
 * A fresh id that is not one of `taken` (epic memql#4746).
 *
 * THE COUNTER ABOVE STARTS AT 0 ON EVERY PAGE LOAD, and the document that
 * loads with the page does not. A reload therefore mints `item-1` -- an id
 * the restored desktop is already using -- and the maps items and positions
 * live in are keyed by id, so the new folder does not collide loudly: it
 * REPLACES the item that had that id, and the thing that was there is gone
 * with no error anywhere. The seeded Ask widget is `item-1`, so the very
 * first folder created after any reload ate it.
 *
 * Roaming turns that from a reload bug into the normal case: every machine
 * mints from the same low numbers, so every document arriving from elsewhere
 * is full of ids this session is about to hand out again.
 *
 * The counter stays -- `desk-1` in a failure message is worth keeping, and
 * every test that pins an id keeps passing on a fresh session, because the
 * skip only happens when there is something to skip.
 */
export function nextIdAvoiding(prefix: string, taken: ReadonlySet<string>): string {
  for (;;) {
    const id = nextId(prefix);
    if (!taken.has(id)) return id;
  }
}

/** The desk ids a shell state already holds. */
export function deskIdsOf(state: ShellState): ReadonlySet<string> {
  return new Set(state.desks.map((d) => d.id));
}

function nextDeskId(state: ShellState): string {
  return nextIdAvoiding("desk", deskIdsOf(state));
}

export function initialShell(): ShellState {
  const deskId = nextId("desk");
  return {
    desks: [{ id: deskId, createdBy: "user", windows: [] }],
    activeDeskId: deskId,
    windows: {},
    focusedWindowId: null,
  };
}

export function deskById(state: ShellState, deskId: DeskId): Desk | undefined {
  return state.desks.find((d) => d.id === deskId);
}

export function activeDesk(state: ShellState): Desk {
  const desk = deskById(state, state.activeDeskId);
  if (!desk) throw new Error("shell invariant: active desk missing");
  return desk;
}

export function deskOfWindow(state: ShellState, windowId: WindowId): Desk | undefined {
  return state.desks.find((d) => d.windows.includes(windowId));
}

export function windowForApp(state: ShellState, appId: AppId): OsWindow | undefined {
  return Object.values(state.windows).find((w) => w.appId === appId);
}

function replaceDesk(state: ShellState, desk: Desk): ShellState {
  return { ...state, desks: state.desks.map((d) => (d.id === desk.id ? desk : d)) };
}

function deskHasVacancy(desk: Desk): boolean {
  return desk.windows.length < DESK_CAP;
}

/**
 * Open an app: focus its existing window (switching desks if needed),
 * place it on the active desk when there is room, or spill onto a fresh
 * auto desk created right after the active one.
 */
export function openApp(state: ShellState, appId: AppId, sectionId = ""): ShellResult {
  const existing = windowForApp(state, appId);
  if (existing) {
    const desk = deskOfWindow(state, existing.id);
    if (!desk) throw new Error("shell invariant: window without a desk");
    const restored: OsWindow =
      existing.mode === "minimized" ? { ...existing, mode: "normal" } : existing;
    const next: ShellState = {
      ...state,
      windows: { ...state.windows, [existing.id]: restored },
      activeDeskId: desk.id,
      focusedWindowId: existing.id,
    };
    return { state: gcAutoDesks(next), effect: { kind: "focused-existing", deskId: desk.id, windowId: existing.id } };
  }

  const win = newWindow(nextId("win"), appId, sectionId);
  const current = activeDesk(state);
  if (deskHasVacancy(current)) {
    const next = replaceDesk(
      { ...state, windows: { ...state.windows, [win.id]: win }, focusedWindowId: win.id },
      { ...current, windows: [...current.windows, win.id] },
    );
    return { state: next, effect: { kind: "placed", deskId: current.id, windowId: win.id } };
  }

  const fresh: Desk = { id: nextDeskId(state), createdBy: "auto", windows: [win.id] };
  const at = state.desks.findIndex((d) => d.id === current.id) + 1;
  const desks = [...state.desks.slice(0, at), fresh, ...state.desks.slice(at)];
  const next: ShellState = {
    ...state,
    desks,
    activeDeskId: fresh.id,
    windows: { ...state.windows, [win.id]: win },
    focusedWindowId: win.id,
  };
  return { state: next, effect: { kind: "spilled", deskId: fresh.id, windowId: win.id } };
}

export function closeWindow(state: ShellState, windowId: WindowId): ShellResult {
  const desk = deskOfWindow(state, windowId);
  if (!desk) return { state, effect: { kind: "none" } };
  const windows = { ...state.windows };
  delete windows[windowId];
  let next: ShellState = replaceDesk(
    {
      ...state,
      windows,
      focusedWindowId: state.focusedWindowId === windowId ? null : state.focusedWindowId,
    },
    { ...desk, windows: desk.windows.filter((id) => id !== windowId) },
  );
  if (next.focusedWindowId === null) {
    const remaining = deskById(next, desk.id)?.windows ?? [];
    next = { ...next, focusedWindowId: remaining[remaining.length - 1] ?? null };
  }
  return { state: gcAutoDesks(next), effect: { kind: "none" } };
}

export function setWindowMode(state: ShellState, windowId: WindowId, mode: OsWindow["mode"]): ShellState {
  const win = state.windows[windowId];
  if (!win || win.mode === mode) return state;
  let next: ShellState = { ...state, windows: { ...state.windows, [windowId]: { ...win, mode } } };
  if (mode === "minimized" && state.focusedWindowId === windowId) {
    const desk = deskOfWindow(state, windowId);
    const sibling = desk?.windows.find((id) => {
      const w = next.windows[id];
      return id !== windowId && !!w && isVisible(w);
    });
    next = { ...next, focusedWindowId: sibling ?? null };
  }
  if (mode !== "minimized") {
    next = { ...next, focusedWindowId: windowId };
  }
  return next;
}

export function focusWindow(state: ShellState, windowId: WindowId): ShellState {
  const desk = deskOfWindow(state, windowId);
  const win = state.windows[windowId];
  if (!desk || !win) return state;
  const restored: OsWindow = win.mode === "minimized" ? { ...win, mode: "normal" } : win;
  return {
    ...state,
    windows: { ...state.windows, [windowId]: restored },
    activeDeskId: desk.id,
    focusedWindowId: windowId,
  };
}

export function setWindowSection(state: ShellState, windowId: WindowId, sectionId: string): ShellState {
  const win = state.windows[windowId];
  if (!win || win.sectionId === sectionId) return state;
  return { ...state, windows: { ...state.windows, [windowId]: { ...win, sectionId } } };
}

/** Swap the two windows of a desk (left <-> right). No-op on a solo desk. */
export function swapSides(state: ShellState, deskId: DeskId): ShellState {
  const desk = deskById(state, deskId);
  if (!desk) return state;
  const [left, right] = desk.windows;
  if (!left || !right) return state;
  return replaceDesk(state, { ...desk, windows: [right, left] });
}

/**
 * Throw a window onto another desk. Refused (typed, so the pager can dim)
 * when the target is full; "new" creates a fresh user desk at the end.
 */
export function throwToDesk(state: ShellState, windowId: WindowId, target: DeskId | "new"): ShellResult {
  const from = deskOfWindow(state, windowId);
  if (!from) return { state, effect: { kind: "none" } };

  let next = state;
  let targetDesk: Desk;
  if (target === "new") {
    targetDesk = { id: nextDeskId(state), createdBy: "user", windows: [] };
    next = { ...next, desks: [...next.desks, targetDesk] };
  } else {
    const found = deskById(state, target);
    if (!found || found.id === from.id) return { state, effect: { kind: "none" } };
    if (!deskHasVacancy(found)) return { state, effect: { kind: "refused-full", deskId: found.id } };
    targetDesk = found;
  }

  next = replaceDesk(next, { ...from, windows: from.windows.filter((id) => id !== windowId) });
  const landed = deskById(next, targetDesk.id);
  if (!landed) throw new Error("shell invariant: throw target vanished");
  next = replaceDesk(next, { ...landed, windows: [...landed.windows, windowId] });
  next = { ...next, activeDeskId: landed.id, focusedWindowId: windowId };
  return { state: gcAutoDesks(next), effect: { kind: "placed", deskId: landed.id, windowId } };
}

export interface SwitchDeskOptions {
  /** True when the desk still shows items/widgets -- blocks GC (spec A). */
  deskHasSurfaceContent?: (deskId: DeskId) => boolean;
}

export function switchDesk(state: ShellState, deskId: DeskId, opts: SwitchDeskOptions = {}): ShellState {
  if (!deskById(state, deskId) || state.activeDeskId === deskId) return state;
  const next: ShellState = { ...state, activeDeskId: deskId, focusedWindowId: topWindowOf(state, deskId) };
  return gcAutoDesks(next, opts.deskHasSurfaceContent);
}

export function switchDeskBy(state: ShellState, delta: 1 | -1, opts: SwitchDeskOptions = {}): ShellState {
  const at = state.desks.findIndex((d) => d.id === state.activeDeskId);
  const to = state.desks[at + delta];
  return to ? switchDesk(state, to.id, opts) : state;
}

function topWindowOf(state: ShellState, deskId: DeskId): WindowId | null {
  const desk = deskById(state, deskId);
  if (!desk) return null;
  const visible = desk.windows.filter((id) => {
    const w = state.windows[id];
    return !!w && isVisible(w);
  });
  return visible[visible.length - 1] ?? null;
}

/**
 * Drop auto-created desks that are inactive and completely empty. The desk
 * you are on is never collected under you; user-created desks never are.
 */
export function gcAutoDesks(
  state: ShellState,
  deskHasSurfaceContent?: (deskId: DeskId) => boolean,
): ShellState {
  const keep = state.desks.filter(
    (d) =>
      d.id === state.activeDeskId ||
      d.createdBy === "user" ||
      d.windows.length > 0 ||
      (deskHasSurfaceContent?.(d.id) ?? false),
  );
  if (keep.length === state.desks.length) return state;
  return { ...state, desks: keep };
}

/** Add a user desk explicitly (context menu / pager "+"). */
export function addDesk(state: ShellState): ShellState {
  const desk: Desk = { id: nextDeskId(state), createdBy: "user", windows: [] };
  return { ...state, desks: [...state.desks, desk], activeDeskId: desk.id, focusedWindowId: null };
}
