import { beforeEach, describe, expect, it } from "vitest";

import {
  activeDesk,
  cycleWindowMode,
  focusWindow,
  addDesk,
  canAddDesk,
  MAX_EMPTY_DESKS,
  closeWindow,
  consumeIntent,
  DESK_CAP,
  deskById,
  initialShell,
  makeBorderless,
  openApp,
  pairWindows,
  resetIdsForTest,
  setWindowMode,
  setWindowSection,
  swapSides,
  switchDesk,
  toggleBorderless,
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

describe("the empty-desk limit (owner, Nov 2026)", () => {
  it("allows the desks in use plus two empty ones, and refuses a third empty desk", () => {
    expect(MAX_EMPTY_DESKS).toBe(2);
    let s = addDesk(initialShell());
    expect(s.desks).toHaveLength(2);
    expect(canAddDesk(s)).toBe(false);
    expect(addDesk(s)).toBe(s);
  });

  it("allows another desk once an empty one gets a window or surface content", () => {
    let s = addDesk(initialShell());
    s = openApp(s, "a").state;
    expect(canAddDesk(s)).toBe(true);
    s = addDesk(s);
    expect(s.desks).toHaveLength(3);
    expect(canAddDesk(s)).toBe(false);
    const third = s.desks[2]!.id;
    expect(canAddDesk(s, (id) => id === third)).toBe(true);
  });
});

describe("open intents (epic memql#4842, #4845)", () => {
  const intent = (id: string) => ({ id, payload: { scope: "desktop", folderId: "f-1" } });

  it("a fresh window carries the intent it was opened with", () => {
    const { state } = openApp(initialShell(), "files", "browse", intent("i-1"));
    const win = windowForApp(state, "files");
    expect(win?.intent?.id).toBe("i-1");
    expect(win?.sectionId).toBe("browse");
  });

  it("opening an already-open app with an intent replaces the standing intent and adopts the section", () => {
    let s = openApp(initialShell(), "files", "settings").state;
    s = openApp(s, "files", "browse", intent("i-1")).state;
    const first = windowForApp(s, "files");
    expect(first?.intent?.id).toBe("i-1");
    expect(first?.sectionId).toBe("browse");
    const { state, effect } = openApp(s, "files", "browse", intent("i-2"));
    expect(effect.kind).toBe("focused-existing");
    expect(windowForApp(state, "files")?.intent?.id).toBe("i-2");
  });

  it("re-opening without an intent neither clears the standing intent nor yanks the section", () => {
    let s = openApp(initialShell(), "files", "browse", intent("i-1")).state;
    s = setWindowSection(s, windowForApp(s, "files")!.id, "settings");
    s = openApp(s, "files").state;
    const win = windowForApp(s, "files");
    expect(win?.intent?.id).toBe("i-1");
    expect(win?.sectionId).toBe("settings");
  });

  it("consumeIntent clears by id, and a stale consume leaves a newer intent standing", () => {
    let s = openApp(initialShell(), "files", "browse", intent("i-1")).state;
    const winId = windowForApp(s, "files")!.id;
    s = openApp(s, "files", "browse", intent("i-2")).state;
    s = consumeIntent(s, winId, "i-1");
    expect(windowForApp(s, "files")?.intent?.id).toBe("i-2");
    s = consumeIntent(s, winId, "i-2");
    expect(windowForApp(s, "files")?.intent).toBeUndefined();
  });

  it("an intent survives a throw to another desk", () => {
    let s = openApp(initialShell(), "files", "browse", intent("i-1")).state;
    const winId = windowForApp(s, "files")!.id;
    s = throwToDesk(s, winId, "new").state;
    expect(windowForApp(s, "files")?.intent?.id).toBe("i-1");
  });

  it("a spilled window carries its intent onto the fresh desk", () => {
    const base = openMany(initialShell(), "a", "b");
    const { state, effect } = openApp(base, "files", "browse", intent("i-1"));
    expect(effect.kind).toBe("spilled");
    expect(windowForApp(state, "files")?.intent?.id).toBe("i-1");
  });
});


describe("dedicated full-screen desktop and contextual navigation", () => {
  it("moves a shared window to an exclusive full-screen desk and restores its original slot and intent", () => {
    const intent = { id: "size-cycle", payload: { machineId: "m-1" } };
    let state = openApp(openMany(initialShell(), "files"), "fleet", "overview", intent).state;
    const fleet = windowForApp(state, "fleet")!;
    const origin = state.activeDeskId;
    state = setWindowSection(state, fleet.id, "routing");
    state = cycleWindowMode(state, fleet.id);
    const dedicated = state.activeDeskId;
    expect(state.windows[fleet.id]?.mode).toBe("fullscreen");
    expect(activeDesk(state).windows).toEqual([fleet.id]);
    expect(state.activeDeskId).not.toBe(origin);
    expect(state.windows[fleet.id]?.sectionId).toBe("routing");
    expect(state.desks).toHaveLength(2);
    expect(openApp(state, "logs").state.activeDeskId).not.toBe(dedicated);
    state = cycleWindowMode(state, fleet.id);
    expect(state.activeDeskId).toBe(origin);
    expect(activeDesk(state).windows[1]).toBe(fleet.id);
    expect(state.windows[fleet.id]?.mode).toBe("normal");
    expect(state.windows[fleet.id]?.expandedReturn).toBeUndefined();
    expect(state.windows[fleet.id]?.intent).toEqual(intent);
    expect(state.windows[fleet.id]?.sectionId).toBe("routing");
    expect(state.desks).toHaveLength(1);
  });
  it("restores a dedicated full-screen window from minimize and never displaces a filled origin", () => {
    let state = openMany(initialShell(), "files", "fleet");
    const fleet = windowForApp(state, "fleet")!;
    const origin = state.activeDeskId;
    state = cycleWindowMode(state, fleet.id);
    const dedicated = state.activeDeskId;
    state = setWindowMode(state, fleet.id, "minimized");
    expect(openApp(state, "logs").state.activeDeskId).not.toBe(dedicated);
    state = focusWindow(state, fleet.id);
    expect(state.windows[fleet.id]?.mode).toBe("fullscreen");
    state = setWindowMode(state, fleet.id, "minimized");
    state = openApp(state, "fleet").state;
    expect(state.windows[fleet.id]?.mode).toBe("fullscreen");
    state = switchDesk(state, origin);
    state = openApp(state, "logs").state;
    state = cycleWindowMode(state, fleet.id);
    expect(state.activeDeskId).toBe(dedicated);
    expect(state.windows[fleet.id]?.mode).toBe("normal");
    expect(deskById(state, origin)?.windows).toHaveLength(2);
  });
  it("preserves a dedicated desk with surface content when restoring", () => {
    let state = openMany(initialShell(), "files", "fleet");
    const id = windowForApp(state, "fleet")!.id;
    state = cycleWindowMode(state, id);
    const dedicated = state.activeDeskId;
    state = cycleWindowMode(state, id, deskId => deskId === dedicated);
    expect(state.windows[id]?.mode).toBe("normal");
    expect(deskById(state, dedicated)).toBeTruthy();
  });
  it("moving a dedicated full-screen window clears its old return slot", () => {
    let state = openMany(initialShell(), "files", "fleet");
    const fleet = windowForApp(state, "fleet")!;
    const origin = state.activeDeskId;
    state = cycleWindowMode(state, fleet.id);
    state = throwToDesk(state, fleet.id, origin).state;
    expect(state.windows[fleet.id]?.mode).toBe("normal");
    expect(state.windows[fleet.id]?.expandedReturn).toBeUndefined();
    expect(activeDesk(state).windows[1]).toBe(fleet.id);
    expect(state.desks).toHaveLength(1);
  });
  it("a breadcrumb can return across multiple sections without leaving a stale trail", () => {
    let state = openApp(initialShell(), "fleet", "overview").state;
    const id = windowForApp(state, "fleet")!.id;
    state = setWindowSection(state, id, "machines", "content");
    state = setWindowSection(state, id, "apps", "content");
    state = setWindowSection(state, id, "overview", "back");
    expect(state.windows[id]?.sectionTrail).toEqual([]);
    expect(state.windows[id]?.sectionId).toBe("overview");
  });
  it("content links retain origins; a direct tab click clears them even on the same tab", () => {
    let state = openApp(initialShell(), "fleet", "machines").state;
    const id = windowForApp(state, "fleet")!.id;
    state = setWindowSection(state, id, "apps", "content");
    expect(state.windows[id]?.sectionTrail).toEqual(["machines"]);
    state = setWindowSection(state, id, "machines", "back");
    expect(state.windows[id]?.sectionTrail).toEqual([]);
    state = setWindowSection(state, id, "apps", "content");
    const revision = state.windows[id]!.sectionNavigation!.revision;
    state = setWindowSection(state, id, "apps", "peer");
    expect(state.windows[id]?.sectionTrail).toEqual([]);
    expect(state.windows[id]?.sectionNavigation).toEqual({ origin: "peer", revision: revision + 1 });
  });
});

// Jesús, Nov 18-19 2026: window mode only alone, borderless = maximized, full
// screen solitary, two apps split the borderless area, drag to an edge snaps.
describe("window rules", () => {
  it("cycles a lone window through borderless and full screen on its own desk", () => {
    let s = openMany(initialShell(), "files");
    const id = windowForApp(s, "files")!.id;
    const desk = s.activeDeskId;
    const modes: string[] = [];
    for (let i = 0; i < 3; i++) {
      s = cycleWindowMode(s, id);
      modes.push(s.windows[id]!.mode);
      expect(s.activeDeskId).toBe(desk);
      expect(s.desks).toHaveLength(1);
      expect(s.windows[id]?.expandedReturn).toBeUndefined();
    }
    expect(modes).toEqual(["maximized", "fullscreen", "normal"]);
  });

  it("a second app joins a lone borderless window's desk", () => {
    let s = openMany(initialShell(), "files");
    s = cycleWindowMode(s, windowForApp(s, "files")!.id);
    const { state, effect } = openApp(s, "fleet");
    expect(effect.kind).toBe("placed");
    expect(state.desks).toHaveLength(1);
    expect(activeDesk(state).windows).toHaveLength(2);
  });

  it("a desk holding a full-screen window, minimized or not, takes no second app", () => {
    let s = openMany(initialShell(), "files");
    const id = windowForApp(s, "files")!.id;
    s = cycleWindowMode(cycleWindowMode(s, id), id);
    expect(openApp(s, "fleet").effect.kind).toBe("spilled");
    s = setWindowMode(s, id, "minimized");
    expect(openApp(s, "fleet").effect.kind).toBe("spilled");
  });

  it("a lone full-screen window thrown onto another desk leaves full screen there", () => {
    let s = openMany(initialShell(), "a", "b", "c"); // desk1(a,b) desk2(c)
    const c = activeDesk(s).windows[0]!;
    s = throwToDesk(s, s.desks[0]!.windows[0]!, "new").state; // desk1(b)
    s = switchDesk(s, s.desks[1]!.id);
    s = cycleWindowMode(cycleWindowMode(s, c), c);
    expect(s.windows[c]?.mode).toBe("fullscreen");
    s = throwToDesk(s, c, s.desks[0]!.id).state;
    expect(s.windows[c]?.mode).toBe("normal");
    expect(activeDesk(s).windows).toHaveLength(2);
  });

  it("a shared window goes full screen on a desk of its own and back to its half", () => {
    let s = openMany(initialShell(), "files", "fleet");
    const origin = s.activeDeskId;
    const files = windowForApp(s, "files")!.id;
    s = cycleWindowMode(s, files);
    expect(s.windows[files]?.mode).toBe("fullscreen");
    expect(activeDesk(s).windows).toEqual([files]);
    expect(s.activeDeskId).not.toBe(origin);
    s = cycleWindowMode(s, files);
    expect(s.activeDeskId).toBe(origin);
    expect(activeDesk(s).windows[0]).toBe(files);
    expect(s.windows[files]?.mode).toBe("normal");
  });

  it("a top drop makes a lone window borderless on its desk and leaves a pair alone", () => {
    const pair = openMany(initialShell(), "files", "fleet");
    expect(makeBorderless(pair, windowForApp(pair, "files")!.id)).toBe(pair);
    let s = openMany(initialShell(), "logs");
    const id = windowForApp(s, "logs")!.id;
    s = makeBorderless(s, id);
    expect(s.windows[id]?.mode).toBe("maximized");
    expect(s.desks).toHaveLength(1);
  });

  it("a top drop on a dedicated full-screen window makes it borderless and drops its return slot", () => {
    let s = openMany(initialShell(), "files", "fleet");
    const files = windowForApp(s, "files")!.id;
    s = makeBorderless(cycleWindowMode(s, files), files);
    expect(s.windows[files]?.mode).toBe("maximized");
    expect(s.windows[files]?.expandedReturn).toBeUndefined();
    expect(activeDesk(s).windows).toEqual([files]);
  });

  it.each([0, 1] as const)("a side drop brings a window from another desk into the other half (side %s)", side => {
    let s = openMany(initialShell(), "a", "b", "c"); // desk1(a,b) desk2(c)
    const [a, b] = s.desks[0]!.windows as [string, string];
    const c = activeDesk(s).windows[0]!;
    s = pairWindows(s, c, side, a);
    expect(activeDesk(s).windows).toEqual(side === 0 ? [c, a] : [a, c]);
    expect(s.desks[0]!.windows).toEqual([b]);
    expect(s.windows[a]?.mode).toBe("normal");
  });

  it("a side drop takes both windows out of full screen and minimize, and off a dedicated desk", () => {
    let s = openMany(initialShell(), "a", "b", "c"); // desk1(a,b) desk2(c)
    const [a, b] = s.desks[0]!.windows as [string, string];
    const c = activeDesk(s).windows[0]!;
    s = cycleWindowMode(switchDesk(s, s.desks[0]!.id), a); // a: full screen on desk3
    s = setWindowMode(s, b, "minimized");
    s = switchDesk(s, s.desks.find(d => d.windows.includes(c))!.id);
    s = cycleWindowMode(cycleWindowMode(s, c), c); // c: lone, full screen
    s = pairWindows(s, c, 0, a);
    expect(activeDesk(s).windows).toEqual([c, a]);
    expect(s.windows[c]?.mode).toBe("normal");
    expect(s.windows[a]?.mode).toBe("normal");
    expect(s.windows[a]?.expandedReturn).toBeUndefined();
    s = pairWindows(throwToDesk(s, a, "new").state, a, 1, b);
    expect(activeDesk(s).windows).toEqual([b, a]);
    expect(s.windows[b]?.mode).toBe("normal");
  });

  it("a bar double-click toggles a lone window and borderless, and takes full screen back to a window", () => {
    let s = openMany(initialShell(), "files");
    const id = windowForApp(s, "files")!.id;
    s = toggleBorderless(s, id);
    expect(s.windows[id]?.mode).toBe("maximized");
    s = toggleBorderless(s, id);
    expect(s.windows[id]?.mode).toBe("normal");
    s = toggleBorderless(cycleWindowMode(cycleWindowMode(s, id), id), id);
    expect(s.windows[id]?.mode).toBe("normal");
    expect(s.desks).toHaveLength(1);
  });

  it("a bar double-click on a dedicated full-screen window makes it a window where it is", () => {
    let s = openMany(initialShell(), "files", "fleet");
    const files = windowForApp(s, "files")!.id;
    s = cycleWindowMode(s, files);
    const dedicated = s.activeDeskId;
    s = toggleBorderless(s, files);
    expect(s.windows[files]?.mode).toBe("normal");
    expect(s.windows[files]?.expandedReturn).toBeUndefined();
    expect(s.activeDeskId).toBe(dedicated);
    expect(activeDesk(s).windows).toEqual([files]);
  });

  it("a bar double-click on half a pair makes it borderless and moves the other to a new desk next to it", () => {
    let s = openMany(initialShell(), "logs", "files", "fleet"); // desk1(logs,files) desk2(fleet)
    s = switchDesk(s, s.desks[0]!.id);
    const [logs, files] = s.desks[0]!.windows as [string, string];
    s = toggleBorderless(s, files);
    expect(s.desks.map(d => d.windows)).toEqual([[files], [logs], [windowForApp(s, "fleet")!.id]]);
    expect(s.activeDeskId).toBe(s.desks[0]!.id);
    expect(s.focusedWindowId).toBe(files);
    expect(s.windows[files]?.mode).toBe("maximized");
    expect(s.windows[logs]?.mode).toBe("normal");
  });

  it("a bar double-click next to a minimized partner acts as if alone", () => {
    let s = openMany(initialShell(), "files", "fleet");
    const [files, fleet] = activeDesk(s).windows as [string, string];
    s = toggleBorderless(setWindowMode(s, fleet, "minimized"), files);
    expect(s.windows[files]?.mode).toBe("maximized");
    expect(s.windows[fleet]?.mode).toBe("minimized");
    expect(activeDesk(s).windows).toEqual([files, fleet]);
  });
});
