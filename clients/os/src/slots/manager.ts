// Slot occupancy is a named region that mounts one module. Cap is 2 on
// desktop/iPad and 0 on phone (memql#4706). Research is chrome and must
// never take a slot.

import type { ChromeLayout } from "../app/layout";

export type SlotId = "a" | "b";

export type SlotState = {
  a: string | null;
  b: string | null;
};

export function emptySlots(): SlotState {
  return { a: null, b: null };
}

export function slotCap(layout: ChromeLayout): number {
  return layout === "phone" ? 0 : 2;
}

export function findSlot(state: SlotState, moduleId: string): SlotId | null {
  if (state.a === moduleId) return "a";
  if (state.b === moduleId) return "b";
  return null;
}

export function occupiedCount(state: SlotState): number {
  return (state.a ? 1 : 0) + (state.b ? 1 : 0);
}

export function occupy(
  state: SlotState,
  layout: ChromeLayout,
  moduleId: string,
): { state: SlotState; slot: SlotId | null; ok: boolean } {
  // Research is chrome. It is not a module and cannot take a slot.
  if (!moduleId || moduleId === "research") {
    return { state, slot: null, ok: false };
  }
  const existing = findSlot(state, moduleId);
  if (existing) return { state, slot: existing, ok: true };
  const cap = slotCap(layout);
  if (cap === 0) return { state, slot: null, ok: false };
  if (!state.a && cap >= 1) {
    return { state: { ...state, a: moduleId }, slot: "a", ok: true };
  }
  if (!state.b && cap >= 2) {
    return { state: { ...state, b: moduleId }, slot: "b", ok: true };
  }
  return { state, slot: null, ok: false };
}

export function vacate(state: SlotState, slot: SlotId): SlotState {
  return { ...state, [slot]: null };
}
