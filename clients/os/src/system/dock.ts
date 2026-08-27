// Dock state is the pin list only. Running is derived from the shell's
// windows at render time -- storing it would let the two disagree.

import type { AppId } from "./windows";

export interface DockState {
  pinned: AppId[];
}

export function emptyDock(): DockState {
  return { pinned: [] };
}

export function isPinned(dock: DockState, appId: AppId): boolean {
  return dock.pinned.includes(appId);
}

export function pin(dock: DockState, appId: AppId): DockState {
  if (isPinned(dock, appId)) return dock;
  return { pinned: [...dock.pinned, appId] };
}

export function unpin(dock: DockState, appId: AppId): DockState {
  if (!isPinned(dock, appId)) return dock;
  return { pinned: dock.pinned.filter((id) => id !== appId) };
}

/** Reorder a pin to a new index (clamped). No-op for unpinned apps. */
export function movePin(dock: DockState, appId: AppId, toIndex: number): DockState {
  const from = dock.pinned.indexOf(appId);
  if (from < 0) return dock;
  const to = Math.max(0, Math.min(dock.pinned.length - 1, toIndex));
  if (to === from) return dock;
  const pinned = [...dock.pinned];
  pinned.splice(from, 1);
  pinned.splice(to, 0, appId);
  return { pinned };
}

/**
 * The dock's center strip: pins in pinned order, then running-but-unpinned
 * apps in the order given (open order). Pins render whether running or not.
 */
export function dockOrder(dock: DockState, runningAppIds: AppId[]): AppId[] {
  const extras = runningAppIds.filter((id) => !isPinned(dock, id));
  return [...dock.pinned, ...extras];
}
