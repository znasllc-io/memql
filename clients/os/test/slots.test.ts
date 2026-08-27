import { describe, expect, it } from "vitest";

import { layoutFromMedia, type MatchMedia } from "../src/app/layout";
import { isModule, PHONE_ALLOWLIST } from "../src/modules/catalog";
import {
  emptySlots,
  findSlot,
  occupy,
  slotCap,
  vacate,
  type SlotState,
} from "../src/slots/manager";

function stubMedia(flags: { hover?: boolean; fine?: boolean; coarse?: boolean }): MatchMedia {
  return (query: string) => {
    if (query === "(hover: hover)") return { matches: Boolean(flags.hover) };
    if (query === "(pointer: fine)") return { matches: Boolean(flags.fine) };
    if (query === "(pointer: coarse)") return { matches: Boolean(flags.coarse) };
    return { matches: false };
  };
}

describe("slot cap", () => {
  it("caps desktop at two visible modules", () => {
    expect(slotCap("desktop")).toBe(2);
    let state: SlotState = emptySlots();
    expect(occupy(state, "desktop", "profile").ok).toBe(true);
    state = occupy(state, "desktop", "profile").state;
    const second = occupy(state, "desktop", "other");
    expect(second.ok).toBe(true);
    state = second.state;
    const third = occupy(state, "desktop", "third");
    expect(third.ok).toBe(false);
    expect(third.state).toEqual(state);
  });

  it("caps iPad at two and paints slots (touch-first, not width)", () => {
    expect(slotCap("ipad")).toBe(2);
    expect(slotCap("ipad")).toBe(slotCap("desktop"));
  });

  it("gives phone no slots", () => {
    expect(slotCap("phone")).toBe(0);
    const attempt = occupy(emptySlots(), "phone", "profile");
    expect(attempt.ok).toBe(false);
    expect(attempt.state).toEqual(emptySlots());
  });

  it("keys the cap off pointer/hover layout, never innerWidth", () => {
    expect(layoutFromMedia(stubMedia({ hover: true, fine: true }))).toBe("desktop");
    expect(slotCap(layoutFromMedia(stubMedia({ hover: true, fine: true })))).toBe(2);
    expect(layoutFromMedia(stubMedia({ hover: true, coarse: true }))).toBe("ipad");
    expect(slotCap(layoutFromMedia(stubMedia({ hover: true, coarse: true })))).toBe(2);
    expect(layoutFromMedia(stubMedia({ hover: false, coarse: true }))).toBe("phone");
    expect(slotCap(layoutFromMedia(stubMedia({ hover: false, coarse: true })))).toBe(0);
  });

  it("re-opening an occupant focuses the existing slot instead of taking a second", () => {
    const first = occupy(emptySlots(), "desktop", "profile");
    const again = occupy(first.state, "desktop", "profile");
    expect(again.ok).toBe(true);
    expect(again.slot).toBe(first.slot);
    expect(again.state).toEqual(first.state);
  });

  it("vacates a slot so another module can occupy it", () => {
    let state = occupy(emptySlots(), "desktop", "profile").state;
    const slot = findSlot(state, "profile");
    expect(slot).toBe("a");
    state = vacate(state, slot!);
    expect(findSlot(state, "profile")).toBeNull();
    expect(occupy(state, "desktop", "profile").ok).toBe(true);
  });
});

describe("strip is not a module", () => {
  it("does not list research in the module catalog", () => {
    expect(isModule("research")).toBe(false);
    expect(isModule("profile")).toBe(true);
  });

  it("refuses to occupy a slot with research", () => {
    const attempt = occupy(emptySlots(), "desktop", "research");
    expect(attempt.ok).toBe(false);
    expect(attempt.state).toEqual(emptySlots());
  });
});

describe("phone allowlist", () => {
  it("allows Profile only — research is chrome, not a module", () => {
    expect(PHONE_ALLOWLIST).toEqual(["profile"]);
    expect(PHONE_ALLOWLIST).not.toContain("research");
  });
});
