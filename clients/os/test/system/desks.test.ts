import { beforeEach, describe, expect, it } from "vitest";

import {
  activeDesk,
  addDesk,
  closeWindow,
  DESK_CAP,
  deskById,
  initialShell,
  openApp,
  resetIdsForTest,
  setWindowMode,
  swapSides,
  switchDesk,
  switchDeskBy,
  throwToDesk,
  windowForApp,
  type ShellState,
} from "../../src/system/desks";

function openMany(state: ShellState, ...appIds: string[]): ShellState {
  let next = state;
  for (const id of appIds) next = openApp(next, id).state;
  return next;
}

beforeEach(resetIdsForTest);

describe("desk cap and auto-spill", () => {
  it("places the first two apps on the same desk", () => {
    const s = openMany(initialShell(), "artifacts", "fleet");
    expect(s.desks).toHaveLength(1);
    expect(activeDesk(s).windows).toHaveLength(2);
  });

  it("spills the third app onto a new desk and switches to it", () => {
    const base = openMany(initialShell(), "artifacts", "fleet");
    const { state, effect } = openApp(base, "settings");
    expect(effect.kind).toBe("spilled");
    expect(state.desks).toHaveLength(2);
    expect(state.activeDeskId).toBe(state.desks[1]!.id);
    expect(activeDesk(state).windows).toHaveLength(1);
    expect(state.desks[1]!.createdBy).toBe("auto");
  });

  it("creates the spill desk immediately after the active desk", () => {
    let s = openMany(initialShell(), "a", "b", "c"); // desk1(a,b) desk2(c)
    s = switchDesk(s, s.desks[0]!.id);
    s = openMany(s, "d"); // desk1 full -> new desk between 1 and 2
    expect(s.desks).toHaveLength(3);
    expect(activeDesk(s).windows.map((id) => s.windows[id]!.appId)).toEqual(["d"]);
    expect(s.desks[1]!.id).toBe(s.activeDeskId);
  });

  it("never exceeds the cap on any desk", () => {
    const s = openMany(initialShell(), "a", "b", "c", "d", "e");
    for (const desk of s.desks) expect(desk.windows.length).toBeLessThanOrEqual(DESK_CAP);
  });
});

describe("single instance", () => {
  it("relaunching an open app focuses it instead of opening a second window", () => {
    const base = openMany(initialShell(), "artifacts", "fleet", "settings");
    const { state, effect } = openApp(base, "artifacts");
    expect(effect.kind).toBe("focused-existing");
    expect(Object.values(state.windows).filter((w) => w.appId === "artifacts")).toHaveLength(1);
    expect(state.activeDeskId).toBe(state.desks[0]!.id);
    expect(state.focusedWindowId).toBe(windowForApp(state, "artifacts")?.id);
  });

  it("relaunching a minimized app restores it", () => {
    let s = openMany(initialShell(), "artifacts");
    const winId = windowForApp(s, "artifacts")!.id;
    s = setWindowMode(s, winId, "minimized");
    const { state } = openApp(s, "artifacts");
    expect(state.windows[winId]!.mode).toBe("normal");
  });
});

describe("window modes and focus", () => {
  it("minimizing the focused window passes focus to its visible sibling", () => {
    let s = openMany(initialShell(), "a", "b");
    const [aId, bId] = activeDesk(s).windows as [string, string];
    s = setWindowMode(s, bId, "minimized");
    expect(s.focusedWindowId).toBe(aId);
  });

  it("closing a window focuses the remaining one and it survives alone", () => {
    let s = openMany(initialShell(), "a", "b");
    const [aId, bId] = activeDesk(s).windows as [string, string];
    s = closeWindow(s, aId).state;
    expect(activeDesk(s).windows).toEqual([bId]);
    expect(s.focusedWindowId).toBe(bId);
  });
});

describe("swap and throw", () => {
  it("swaps the two windows of a desk", () => {
    const s = openMany(initialShell(), "a", "b");
    const before = activeDesk(s).windows;
    const after = activeDesk(swapSides(s, s.activeDeskId)).windows;
    expect(after).toEqual([before[1], before[0]]);
  });

  it("throws a window to another desk with room", () => {
    let s = openMany(initialShell(), "a", "b", "c"); // desk2 holds c
    const desk1 = s.desks[0]!;
    const desk2 = s.desks[1]!;
    const aId = desk1.windows[0]!;
    const { state, effect } = throwToDesk(s, aId, desk2.id);
    expect(effect).toEqual({ kind: "placed", deskId: desk2.id, windowId: aId });
    expect(deskById(state, desk2.id)?.windows).toContain(aId);
    expect(state.activeDeskId).toBe(desk2.id);
  });

  it("refuses a throw onto a full desk with a typed effect", () => {
    let s = openMany(initialShell(), "a", "b", "c"); // desk1 full
    s = switchDesk(s, s.desks[1]!.id);
    const cId = activeDesk(s).windows[0]!;
    const { state, effect } = throwToDesk(s, cId, s.desks[0]!.id);
    expect(effect).toEqual({ kind: "refused-full", deskId: s.desks[0]!.id });
    expect(state).toBe(s);
  });

  it("throws to a brand-new desk on demand", () => {
    const s = openMany(initialShell(), "a", "b");
    const aId = activeDesk(s).windows[0]!;
    const { state } = throwToDesk(s, aId, "new");
    expect(state.desks).toHaveLength(2);
    expect(state.desks[1]!.windows).toEqual([aId]);
    expect(state.desks[1]!.createdBy).toBe("user");
  });
});

describe("desk lifecycle", () => {
  it("garbage-collects an auto desk left empty", () => {
    let s = openMany(initialShell(), "a", "b", "c"); // desk2 auto with c
    const cId = activeDesk(s).windows[0]!;
    s = closeWindow(s, cId).state; // still ON desk2 -> not collected under us
    expect(s.desks).toHaveLength(2);
    s = switchDesk(s, s.desks[0]!.id);
    expect(s.desks).toHaveLength(1);
  });

  it("keeps an empty auto desk that still shows surface content", () => {
    let s = openMany(initialShell(), "a", "b", "c");
    const autoDeskId = s.activeDeskId;
    const cId = activeDesk(s).windows[0]!;
    s = closeWindow(s, cId).state;
    s = switchDesk(s, s.desks[0]!.id, { deskHasSurfaceContent: (id) => id === autoDeskId });
    expect(s.desks.map((d) => d.id)).toContain(autoDeskId);
  });

  it("never collects user-created desks, and always keeps at least one", () => {
    let s = addDesk(initialShell());
    s = switchDesk(s, s.desks[0]!.id);
    expect(s.desks).toHaveLength(2);
  });

  it("switchDeskBy walks the desk strip and stops at the ends", () => {
    let s = openMany(initialShell(), "a", "b", "c");
    s = switchDeskBy(s, -1);
    expect(s.activeDeskId).toBe(s.desks[0]!.id);
    s = switchDeskBy(s, -1);
    expect(s.activeDeskId).toBe(s.desks[0]!.id);
  });
});
