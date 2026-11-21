import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { Shell } from "../src/chrome/Shell";
import { StubAskTransport } from "./ask/stubTransport";
import { resetIdsForTest } from "../src/system/desks";
import { LocalDesktopStore } from "../src/system/store";
import type { OsRuntimeConfig } from "../src/cluster/config";
import { saveEditorPreference } from "../src/items/editorPreference";
import { artifactHandoffUrl, openInVsCode } from "../src/items/vscode";
import { appTileName } from "./appTile";
import { installSeededAccess } from "./seededAccess";
import { chooseOption } from "./selectControl";

// The shell behavior suite (spec K): rendered against the REAL registry,
// an in-memory store per test, and the stub transports PR A ships.

const CONFIG: OsRuntimeConfig = {
  identityUrl: "https://identity.example.test",
  identityApiBaseUrl: "",
  oauthClientId: "client",
  authEnabled: true,
  domain: "example.test",
};

const OWNER = { userId: "u-1", primaryEmail: "owner@example.test", role: "owner", roleName: "", rank: 0 };
const READER = { userId: "u-2", primaryEmail: "reader@example.test", role: "reader", roleName: "", rank: 0 };

function memStorage(): Pick<Storage, "getItem" | "setItem"> {
  const data = new Map<string, string>();
  return { getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

function renderShell({
  config = CONFIG,
  access = OWNER,
  layout = "desktop" as const,
  storage = memStorage(),
}: {
  config?: OsRuntimeConfig;
  access?: typeof OWNER;
  layout?: "desktop" | "ipad" | "phone";
  storage?: Pick<Storage, "getItem" | "setItem">;
} = {}) {
  // The effective set follows the role the harness signs in as (epic
  // memql#5289): the cluster's answer for that role on a cluster with no
  // grants, installed before the first render.
  installSeededAccess(access.role);
  const view = render(
    <Shell
      layout={layout}
      onSignOut={vi.fn()}
      access={access}
      config={config}
      ports={{ store: new LocalDesktopStore(storage), disableConnection: true, askTransport: new StubAskTransport(), askVoice: null }}
    />,
  );
  return { view, storage };
}

function openFromLauncher(name: string) {
  fireEvent.click(screen.getByRole("button", { name: "Launcher" }));
  fireEvent.click(within(screen.getByRole("dialog", { name: "Launcher" })).getByRole("button", { name: appTileName(name) }));
}

beforeEach(() => {
  resetIdsForTest();
  document.documentElement.removeAttribute("data-theme");
});

describe("the desktop lands (spec K bullet 1)", () => {
  it("desktop stays at scroll zero", () => {
    renderShell();
    const desktop = document.querySelector("[data-os-desktop]") as HTMLElement;
    desktop.scrollLeft = 120;
    desktop.scrollTop = 40;
    fireEvent.scroll(desktop);
    expect(desktop.scrollLeft).toBe(0);
    expect(desktop.scrollTop).toBe(0);
  });

  it("shows the desk world and none of the slot chrome", () => {
    renderShell();
    expect(document.querySelector("[data-os-desktop]")).not.toBeNull();
    expect(document.querySelector("[data-os-dock]")).not.toBeNull();
    expect(document.querySelector("[data-os-wallpaper]")).not.toBeNull();
    expect(document.querySelector("[data-os-desk-numeral]")).not.toBeNull();
    expect(document.querySelector("[data-os-slot]")).toBeNull();
  });

  it("seeds the Ask widget on a fresh desktop", () => {
    renderShell();
    expect(document.querySelector("[data-os-widget='ask']")).not.toBeNull();
  });
});

describe("windows and desks (spec K bullets 2-3)", () => {
  it("opens two apps on one desk, spills the third onto a new desk", () => {
    renderShell();
    openFromLauncher("Files");
    openFromLauncher("Fleet");
    expect(document.querySelectorAll(".os-window")).toHaveLength(2);
    expect(screen.getByText("Desk 1 of 1")).toBeTruthy();

    openFromLauncher("Deployables");
    expect(screen.getByText("Desk 2 of 2")).toBeTruthy();
    const pager = screen.getByRole("navigation", { name: "Desks" });
    expect(within(pager).getAllByRole("button", { name: /^Desk \d/ })).toHaveLength(2);
  });

  it("draws the desk dots inside the dock, before the clock", () => {
    renderShell();
    const pager = screen.getByRole("navigation", { name: "Desks" });
    const dock = pager.closest("footer.os-dock");
    expect(dock).not.toBeNull();
    expect(pager.parentElement?.classList.contains("os-dock-desks")).toBe(true);
    const clock = dock!.querySelector(".os-clock")!;
    expect(pager.compareDocumentPosition(clock) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("grows a desk dot as the mouse comes near it, and only for a mouse", async () => {
    renderShell();
    const dot = screen.getByRole("button", { name: /^Desk 1/ });
    const move = (x: number, pointerType: string) => {
      const event = new MouseEvent("pointermove", { clientX: x, clientY: 0, bubbles: true });
      Object.defineProperty(event, "pointerType", { value: pointerType });
      window.dispatchEvent(event);
    };
    const frame = () => act(() => new Promise<void>((done) => requestAnimationFrame(() => done())));
    // jsdom lays nothing out, so every dot's centre is (0, 0).
    move(0, "mouse"); await frame();
    expect(dot.style.getPropertyValue("--os-near")).toBe("1.000");
    move(22, "mouse"); await frame();
    expect(dot.style.getPropertyValue("--os-near")).toBe("0.500");
    move(200, "mouse"); await frame();
    expect(dot.style.getPropertyValue("--os-near")).toBe("0.000");
    move(0, "touch"); await frame();
    expect(dot.style.getPropertyValue("--os-near")).toBe("0.000");
  });

  it("relaunching an open app focuses it instead of duplicating it", () => {
    renderShell();
    openFromLauncher("Files");
    openFromLauncher("Files");
    expect(document.querySelectorAll("[data-os-window='files']")).toHaveLength(1);
  });

  it("minimizes to the dock and restores from it", () => {
    renderShell();
    openFromLauncher("Files");
    const original = document.querySelector("[data-os-window='files']");
    fireEvent.click(screen.getByRole("button", { name: "Minimize Files" }));
    expect(original?.hasAttribute("hidden")).toBe(true);

    const dock = document.querySelector("[data-os-dock]") as HTMLElement;
    fireEvent.click(within(dock).getByRole("button", { name: "Files (running)" }));
    expect(document.querySelector("[data-os-window='files']")).not.toBeNull();
  });

  it("cycles a lone app through borderless and full screen on its own desk, then back to a window", () => {
    renderShell();
    openFromLauncher("Files");
    const original = screen.getByRole("dialog", { name: "Files" });
    const originalStyle = original.getAttribute("style");
    const desk = original.getAttribute("data-os-window-desk");
    fireEvent.click(screen.getByRole("button", { name: "Make Files borderless" }));
    expect(original.hasAttribute("data-maximized")).toBe(true);
    expect(original.getAttribute("data-os-window-desk")).toBe(desk);
    expect(screen.getByRole("button", { name: "Launcher" })).toBeTruthy();
    expect(screen.getByRole("navigation", { name: "Desks" })).toBeTruthy();
    const maximizedHeight = parseFloat(original.style.height);
    fireEvent.click(screen.getByRole("button", { name: "Make Files full screen" }));
    expect(original.hasAttribute("data-fullscreen")).toBe(true);
    expect(parseFloat(original.style.height)).toBeGreaterThan(maximizedHeight);
    expect(original.getAttribute("data-os-window-desk")).toBe(desk);
    expect(screen.queryByRole("button", { name: "Launcher" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Restore Files to a window" }));
    expect(original.getAttribute("style")).toBe(originalStyle);
    expect(original.getAttribute("data-os-window-desk")).toBe(desk);
    expect(screen.getByText("Desk 1 of 1")).toBeTruthy();
  });

  it("a bar double-click toggles borderless, never full screen, and ignores the bar's buttons", () => {
    renderShell();
    openFromLauncher("Files");
    const files = screen.getByRole("dialog", { name: "Files" });
    const bar = files.querySelector(".os-window-bar") as HTMLElement;
    fireEvent.doubleClick(screen.getByRole("button", { name: "Minimize Files" }));
    fireEvent.doubleClick(within(files).getByRole("button", { name: /^Files settings/ }));
    expect(files.hasAttribute("data-maximized")).toBe(false);
    fireEvent.doubleClick(bar.querySelector(".os-window-drag-space")!);
    expect(files.hasAttribute("data-maximized")).toBe(true);
    fireEvent.doubleClick(bar.querySelector(".os-window-grip")!);
    expect(files.hasAttribute("data-maximized") || files.hasAttribute("data-fullscreen")).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "Make Files borderless" }));
    fireEvent.click(screen.getByRole("button", { name: "Make Files full screen" }));
    fireEvent.doubleClick(bar);
    expect(files.hasAttribute("data-fullscreen") || files.hasAttribute("data-maximized")).toBe(false);
  });

  it("a bar double-click on half a pair makes it borderless and moves the other app to its own desk", () => {
    renderShell();
    openFromLauncher("Files");
    openFromLauncher("Fleet");
    const files = screen.getByRole("dialog", { name: "Files" });
    fireEvent.doubleClick(files.querySelector(".os-window-drag-space")!);
    expect(files.hasAttribute("data-maximized")).toBe(true);
    expect(files.hasAttribute("data-split")).toBe(false);
    expect(screen.getByText("Desk 1 of 2")).toBeTruthy();
    const fleet = document.querySelector("[data-os-window='fleet']")!;
    expect(fleet.getAttribute("data-os-window-desk")).not.toBe(files.getAttribute("data-os-window-desk"));
    expect(fleet.hasAttribute("hidden")).toBe(true);
  });

  it("splits two apps edge to edge, and a second app joins a lone borderless one", () => {
    renderShell();
    openFromLauncher("Files");
    fireEvent.click(screen.getByRole("button", { name: "Make Files borderless" }));
    openFromLauncher("Fleet");
    expect(screen.getByText("Desk 1 of 1")).toBeTruthy();
    const files = screen.getByRole("dialog", { name: "Files" });
    const fleet = screen.getByRole("dialog", { name: "Fleet" });
    const half = `${window.innerWidth / 2}px`;
    expect([files.style.left, files.style.width]).toEqual(["0px", half]);
    expect([fleet.style.left, fleet.style.width]).toEqual([half, half]);
    expect(fleet.style.height).toBe(files.style.height);
    expect(files.hasAttribute("data-split") && fleet.hasAttribute("data-split")).toBe(true);
  });

  it("sends a shared app full screen on its own desk, then restores the original window and split", () => {
    renderShell();
    openFromLauncher("Files");
    openFromLauncher("Fleet");
    const original = screen.getByRole("dialog", { name: "Files" });
    const originalStyle = original.getAttribute("style");
    const origin = original.getAttribute("data-os-window-desk");
    fireEvent.click(screen.getByRole("button", { name: "Make Files full screen on its own desktop" }));
    expect(original.hasAttribute("data-fullscreen")).toBe(true);
    expect(original.getAttribute("data-os-window-desk")).not.toBe(origin);
    expect(screen.queryByRole("button", { name: "Launcher" })).toBeNull();
    expect(screen.queryByRole("navigation", { name: "Desks" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Restore Files to previous desktop" }));
    expect(screen.getByRole("dialog", { name: "Files" })).toBe(original);
    expect(original.getAttribute("style")).toBe(originalStyle);
    expect(original.getAttribute("data-os-window-desk")).toBe(origin);
    expect(screen.getByRole("dialog", { name: "Fleet" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Launcher" })).toBeTruthy();
    expect(screen.getByRole("navigation", { name: "Desks" })).toBeTruthy();
  });

  it("shows the dock on other desks and after minimizing or closing a fullscreen window", () => {
    renderShell();
    openFromLauncher("Files");
    openFromLauncher("Fleet");
    fireEvent.click(screen.getByRole("button", { name: "Make Files full screen on its own desktop" }));
    const original = screen.getByRole("dialog", { name: "Files" });
    fireEvent.keyDown(window, { key: "ArrowLeft", ctrlKey: true, shiftKey: true });
    expect(screen.getByRole("button", { name: "Launcher" })).toBeTruthy();
    expect(screen.getByRole("navigation", { name: "Desks" })).toBeTruthy();
    fireEvent.keyDown(window, { key: "ArrowRight", ctrlKey: true, shiftKey: true });
    expect(screen.queryByRole("button", { name: "Launcher" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Minimize Files" }));
    expect(screen.getByRole("navigation", { name: "Desks" })).toBeTruthy();
    const dock = document.querySelector("[data-os-dock]") as HTMLElement;
    fireEvent.click(within(dock).getByRole("button", { name: "Files (running)" }));
    expect(screen.getByRole("dialog", { name: "Files" })).toBe(original);
    expect(original.hasAttribute("data-fullscreen")).toBe(true);
    expect(screen.queryByRole("button", { name: "Launcher" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Close Files" }));
    expect(document.querySelector("[data-os-window='files']")).toBeNull();
    expect(screen.getByRole("button", { name: "Launcher" })).toBeTruthy();
    expect(screen.getByRole("navigation", { name: "Desks" })).toBeTruthy();
  });

  it("switches desks from the keyboard", () => {
    renderShell();
    openFromLauncher("Files");
    openFromLauncher("Fleet");
    openFromLauncher("Deployables"); // desk 2
    fireEvent.keyDown(window, { key: "ArrowLeft", ctrlKey: true, shiftKey: true });
    expect(screen.getByText("Desk 1 of 2")).toBeTruthy();
    fireEvent.keyDown(window, { key: "2", ctrlKey: true, shiftKey: true });
    expect(screen.getByText("Desk 2 of 2")).toBeTruthy();
  });
});

describe("desk switch motion (owner, Nov 2026)", () => {
  // Themes write the token in seconds or milliseconds (Graphite: ".26s").
  it.each(["260ms", ".26s"])("slides the windows with the desks for the whole slide (token %s): the desk left stays drawn and inert", (token) => {
    document.documentElement.style.setProperty("--os-duration-med", token);
    try {
      renderShell();
      openFromLauncher("Files");
      openFromLauncher("Fleet");
      openFromLauncher("Deployables"); // desk 2
      vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
      fireEvent.keyDown(window, { key: "ArrowLeft", ctrlKey: true, shiftKey: true });
      const leaving = document.querySelector("[data-os-window='deployables']") as HTMLElement;
      const arriving = document.querySelector("[data-os-window='files']") as HTMLElement;
      expect(leaving.hasAttribute("hidden")).toBe(false);
      expect(leaving.getAttribute("data-slide")).toBe("out");
      expect(leaving.hasAttribute("inert")).toBe(true);
      expect(arriving.getAttribute("data-slide")).toBe("in");
      act(() => { vi.advanceTimersByTime(250); });
      expect(leaving.hasAttribute("hidden")).toBe(false);
      expect(arriving.getAttribute("data-slide")).toBe("in");
      act(() => { vi.advanceTimersByTime(10); });
      expect(leaving.hasAttribute("hidden")).toBe(true);
      expect(leaving.hasAttribute("data-slide")).toBe(false);
      expect(arriving.hasAttribute("data-slide")).toBe(false);
    } finally {
      vi.useRealTimers();
      document.documentElement.style.removeProperty("--os-duration-med");
    }
  });

  it("keeps one speed: a switch across two desks slides for twice as long", () => {
    document.documentElement.style.setProperty("--os-duration-med", "260ms");
    try {
      renderShell();
      openFromLauncher("Files");
      openFromLauncher("Fleet");
      openFromLauncher("Deployables"); // desk 2: an empty one would delete itself
      fireEvent.click(screen.getByRole("button", { name: "New desk" }));
      expect(screen.getByRole("button", { name: /^Desk 3, current/ })).toBeTruthy();
      vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
      fireEvent.click(screen.getByRole("button", { name: /^Desk 1/ }));
      const arriving = document.querySelector("[data-os-window='files']") as HTMLElement;
      const desktop = document.querySelector("[data-os-desktop]") as HTMLElement;
      expect(arriving.getAttribute("data-slide")).toBe("in");
      expect(desktop.style.getPropertyValue("--os-slide-travel")).toBe("2");
      act(() => { vi.advanceTimersByTime(510); });
      expect(arriving.getAttribute("data-slide")).toBe("in");
      act(() => { vi.advanceTimersByTime(10); });
      expect(arriving.hasAttribute("data-slide")).toBe(false);
      expect(desktop.style.getPropertyValue("--os-slide-travel")).toBe("1");
    } finally {
      vi.useRealTimers();
      document.documentElement.style.removeProperty("--os-duration-med");
    }
  });

  it("still slides when the empty desk you leave deletes itself and the desks after it move up", () => {
    document.documentElement.style.setProperty("--os-duration-med", "260ms");
    try {
      renderShell();
      openFromLauncher("Files");
      openFromLauncher("Fleet");
      openFromLauncher("Deployables"); // desk 2
      fireEvent.click(screen.getByRole("button", { name: "New desk" }));
      openFromLauncher("Settings"); // desk 3
      fireEvent.click(screen.getByRole("button", { name: /^Desk 2/ }));
      fireEvent.click(screen.getByRole("button", { name: "Close Deployables" }));
      vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
      fireEvent.click(screen.getByRole("button", { name: /^Desk 3/ }));
      expect(screen.getByText("Desk 2 of 2")).toBeTruthy();
      const arriving = document.querySelector("[data-os-window='settings']") as HTMLElement;
      expect(arriving.getAttribute("data-slide")).toBe("in");
      expect(document.querySelector("[data-os-desktop]")?.getAttribute("style")).toContain("--os-slide-travel: 1");
    } finally {
      vi.useRealTimers();
      document.documentElement.style.removeProperty("--os-duration-med");
    }
  });

  it("does not slide under reduced motion, where the duration token is 0ms", () => {
    document.documentElement.style.setProperty("--os-duration-med", "0ms");
    try {
      renderShell();
      openFromLauncher("Files");
      openFromLauncher("Fleet");
      openFromLauncher("Deployables");
      fireEvent.keyDown(window, { key: "ArrowLeft", ctrlKey: true, shiftKey: true });
      const leaving = document.querySelector("[data-os-window='deployables']") as HTMLElement;
      expect(leaving.hasAttribute("hidden")).toBe(true);
      expect(document.querySelector("[data-slide]")).toBeNull();
    } finally {
      document.documentElement.style.removeProperty("--os-duration-med");
    }
  });
});

describe("empty desks (José and the owner, Nov 2026)", () => {
  it("an unused new desk deletes itself once left, and New desk waits while you are on an empty one", () => {
    // The first desk holds the seeded Ask widget, so it is in use.
    renderShell();
    const pager = screen.getByRole("navigation", { name: "Desks" });
    const plus = () => within(pager).getByRole("button", { name: "New desk" });
    fireEvent.click(plus());
    expect(screen.getByText("Desk 2 of 2")).toBeTruthy();
    expect(plus().getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(plus());
    expect(screen.getByText("Desk 2 of 2")).toBeTruthy();
    fireEvent.click(within(pager).getByRole("button", { name: "Desk 1" }));
    expect(screen.getByText("Desk 1 of 1")).toBeTruthy();
    fireEvent.click(plus());
    openFromLauncher("Files");
    expect(plus().hasAttribute("aria-disabled")).toBe(false);
    fireEvent.click(plus());
    expect(screen.getByText("Desk 3 of 3")).toBeTruthy();
    fireEvent.click(within(pager).getByRole("button", { name: "Desk 2" }));
    expect(screen.getByText("Desk 2 of 2")).toBeTruthy();
  });

  it("closing a desk's last app keeps you there; leaving then deletes it", () => {
    renderShell();
    const pager = screen.getByRole("navigation", { name: "Desks" });
    fireEvent.click(within(pager).getByRole("button", { name: "New desk" }));
    openFromLauncher("Files");
    fireEvent.click(screen.getByRole("button", { name: "Close Files" }));
    expect(screen.getByText("Desk 2 of 2")).toBeTruthy();
    fireEvent.click(within(pager).getByRole("button", { name: "Desk 1" }));
    expect(screen.getByText("Desk 1 of 1")).toBeTruthy();
  });
});

// Drag a window by its grip with a real pointer gesture (dnd-kit's pointer
// sensor listens on the document); `during` runs with the pointer at `to`.
// dnd-kit swallows every click for 50ms after a drop, so this waits it out.
async function dragWindow(name: string, to: { x: number; y: number }, during?: () => void) {
  const grip = screen.getByRole("button", { name: `Move ${name} -- drag or press Shift+F10 for desktop choices` });
  fireEvent.pointerDown(grip, { clientX: 400, clientY: 300, button: 0, isPrimary: true });
  fireEvent.pointerMove(document, { clientX: 420, clientY: 300 });
  fireEvent.pointerMove(document, { clientX: to.x, clientY: to.y });
  during?.();
  fireEvent.pointerUp(document, { clientX: to.x, clientY: to.y });
  await act(() => new Promise((resolve) => setTimeout(resolve, 60)));
}

describe("dragging a window to an edge (Jesús, Nov 2026)", () => {
  const PICKER = "Choose an app for the other half";

  it("lights the half, then offers the apps on other desks and brings the pick into the empty half", async () => {
    renderShell();
    openFromLauncher("Files");
    openFromLauncher("Fleet");
    openFromLauncher("Deployables"); // alone on desk 2
    const lone = screen.getByRole("dialog", { name: "Deployables" });
    await dragWindow("Deployables", { x: 5, y: 300 }, () => {
      expect(document.querySelector("[data-os-snap-zone='left']")).not.toBeNull();
    });
    expect(document.querySelector("[data-os-snap-zone]")).toBeNull();
    const picker = screen.getByRole("group", { name: PICKER });
    expect(within(picker).getAllByRole("button").map(b => b.getAttribute("aria-label"))).toEqual(["Files", "Fleet"]);
    const half = `${window.innerWidth / 2}px`;
    expect([lone.style.left, lone.style.width]).toEqual(["0px", half]);
    fireEvent.click(within(picker).getByRole("button", { name: "Fleet" }));
    expect(screen.queryByRole("group", { name: PICKER })).toBeNull();
    const fleet = screen.getByRole("dialog", { name: "Fleet" });
    expect(fleet.getAttribute("data-os-window-desk")).toBe(lone.getAttribute("data-os-window-desk"));
    expect([fleet.style.left, fleet.style.width]).toEqual([half, half]);
    expect([lone.style.left, lone.style.width]).toEqual(["0px", half]);
    expect(screen.getByText("Desk 2 of 2")).toBeTruthy();
  });

  // Jesús, Nov 20 2026: the options fade away and the app stays on its half,
  // the other half left empty with the desk showing.
  it.each([
    ["Escape", () => fireEvent.keyDown(window, { key: "Escape" })],
    ["a click elsewhere", () => fireEvent.pointerDown(document.body)],
    ["a click on the darkened half", () => fireEvent.pointerDown(screen.getByRole("group", { name: PICKER }))],
  ] as const)("%s fades the options away and the lone app stays on its half", async (_, dismiss) => {
    renderShell();
    openFromLauncher("Files");
    openFromLauncher("Fleet");
    openFromLauncher("Deployables");
    await dragWindow("Deployables", { x: window.innerWidth - 5, y: 300 });
    expect(screen.getByRole("group", { name: PICKER })).toBeTruthy();
    dismiss();
    expect(screen.queryByRole("group", { name: PICKER })).toBeNull();
    expect(document.querySelector(".os-snap-picker[data-leaving]")).not.toBeNull();
    await act(() => new Promise((resolve) => setTimeout(resolve, 200)));
    expect(document.querySelector(".os-snap-picker")).toBeNull();
    const lone = screen.getByRole("dialog", { name: "Deployables" });
    const half = `${window.innerWidth / 2}px`;
    expect([lone.style.left, lone.style.width]).toEqual([half, half]);
    expect(lone.hasAttribute("data-maximized")).toBe(false);
    expect(lone.hasAttribute("data-split")).toBe(true);
  });

  it("under reduced motion the options go at once", async () => {
    const original = window.matchMedia;
    vi.stubGlobal("matchMedia", (query: string) => query === "(prefers-reduced-motion: reduce)"
      ? { ...original(query), matches: true }
      : original(query));
    try {
      renderShell();
      openFromLauncher("Files");
      openFromLauncher("Fleet");
      openFromLauncher("Deployables");
      await dragWindow("Deployables", { x: 5, y: 300 });
      fireEvent.keyDown(window, { key: "Escape" });
      expect(document.querySelector(".os-snap-picker")).toBeNull();
      const lone = screen.getByRole("dialog", { name: "Deployables" });
      expect([lone.style.left, lone.style.width]).toEqual(["0px", `${window.innerWidth / 2}px`]);
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("with no other app open, a side drop rests the app on that half with no picker", async () => {
    renderShell();
    openFromLauncher("Files");
    await dragWindow("Files", { x: 5, y: 300 });
    expect(screen.queryByRole("group", { name: PICKER })).toBeNull();
    const files = screen.getByRole("dialog", { name: "Files" });
    expect([files.style.left, files.style.width]).toEqual(["0px", `${window.innerWidth / 2}px`]);
    expect(files.hasAttribute("data-maximized")).toBe(false);
    // Its size button says the next step, borderless, and takes it.
    fireEvent.click(screen.getByRole("button", { name: "Make Files borderless" }));
    expect(files.hasAttribute("data-maximized")).toBe(true);
  });

  it("a second app opened on that desk fills the empty half", async () => {
    renderShell();
    openFromLauncher("Files");
    await dragWindow("Files", { x: window.innerWidth - 5, y: 300 });
    openFromLauncher("Fleet");
    const half = `${window.innerWidth / 2}px`;
    const files = screen.getByRole("dialog", { name: "Files" });
    const fleet = screen.getByRole("dialog", { name: "Fleet" });
    expect(fleet.getAttribute("data-os-window-desk")).toBe(files.getAttribute("data-os-window-desk"));
    expect([fleet.style.left, files.style.left]).toEqual(["0px", half]);
  });

  it("a top drop lights the whole area and makes a lone app borderless on its desk", async () => {
    renderShell();
    openFromLauncher("Files");
    await dragWindow("Files", { x: 500, y: 5 }, () => {
      expect(document.querySelector("[data-os-snap-zone='top']")).not.toBeNull();
    });
    expect(screen.getByRole("dialog", { name: "Files" }).hasAttribute("data-maximized")).toBe(true);
    expect(screen.getByText("Desk 1 of 1")).toBeTruthy();
  });
});

describe("dock pins (spec K bullet 3)", () => {
  it("pins from the context menu and persists across a remount", () => {
    const storage = memStorage();
    const { view } = renderShell({ storage });
    openFromLauncher("Files");
    const dock = document.querySelector("[data-os-dock]") as HTMLElement;
    fireEvent.contextMenu(within(dock).getByRole("button", { name: "Files (running)" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Pin to dock" }));

    view.unmount();
    resetIdsForTest();
    renderShell({ storage });
    const dock2 = document.querySelector("[data-os-dock]") as HTMLElement;
    expect(within(dock2).getByRole("button", { name: "Files" })).toBeTruthy();
  });
});

describe("roles (spec K bullet 7)", () => {
  it("hides admin- and writer-gated apps from a reader everywhere", () => {
    renderShell({ access: READER });
    fireEvent.click(screen.getByRole("button", { name: "Launcher" }));
    const launcher = screen.getByRole("dialog", { name: "Launcher" });
    expect(within(launcher).queryByRole("button", { name: "Users" })).toBeNull();
    expect(within(launcher).queryByRole("button", { name: "Training" })).toBeNull();
    expect(within(launcher).getByRole("button", { name: appTileName("Files") })).toBeTruthy();
  });

  it("keeps the reader's personal settings in Profile", () => {
    renderShell({ access: READER });
    fireEvent.click(screen.getByRole("button", { name: "Open Profile" }));
    const nav = screen.getByRole("navigation", { name: "Profile sections" });
    expect(within(nav).queryByRole("button", { name: "Sign-in policy" })).toBeNull();
    expect(within(nav).getByRole("button", { name: "About" })).toBeTruthy();
  });

  it("owner sees installation services and the gear opens Integrations", async () => {
    renderShell();
    openFromLauncher("Settings");
    const nav = screen.getByRole("navigation", { name: "Settings sections" });
    expect(within(nav).getByRole("button", { name: "Integrations" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Settings settings" }));
    expect(screen.getByRole("button", { name: "Settings settings" }).getAttribute("aria-current")).toBe("page");
    // The real Settings module loads on demand. Await that work and its React
    // update explicitly; a cold transform need not finish within findBy's 1s.
    await act(async () => { await import("../src/apps/settings/SettingsApp"); });
    expect(screen.getByRole("heading", { name: "Integrations" })).toBeTruthy();
  });
});

describe("the one trail row", () => {
  // The position has been specified twice and was wrong the first time, so it
  // is pinned against the REAL window frame rather than left to a comment. The
  // order is the hierarchy: tabs choose the section, the trail is depth in it.
  it("sits under the section tabs and above the app body, once per window", () => {
    renderShell();
    openFromLauncher("Settings");
    const tabs = screen.getByRole("navigation", { name: "Settings sections" });
    const trails = screen.getAllByRole("navigation", { name: "Breadcrumbs" });
    expect(trails).toHaveLength(1);
    const row = trails[0]!.closest("[data-os-trail-row]") as HTMLElement;
    const body = document.querySelector("[data-os-window-content]") as HTMLElement;
    expect(tabs.compareDocumentPosition(row) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(row.compareDocumentPosition(body) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // Drawn by the frame, not by the page: it is a sibling of the app body.
    expect(body.contains(row)).toBe(false);
  });

  it("is there at an app's root, with Back in place and inert", () => {
    renderShell();
    openFromLauncher("Settings");
    const back = screen.getByRole("button", { name: "Back" }) as HTMLButtonElement;
    expect(back.disabled).toBe(true);
    expect(back.closest("[data-os-trail-row]")).not.toBeNull();
  });
});

describe("Ask (spec K bullet 5)", () => {
  // Ask lives in the dock, not in each window's title bar (José, Nov 19 2026).
  it("the orb and the widget open the same surface; no title bar carries Ask or search", () => {
    renderShell();
    fireEvent.click(screen.getByRole("button", { name: "Ask" }));
    const sheet = screen.getByRole("dialog", { name: "Ask" });
    expect(within(sheet).getByRole("textbox", { name: "Ask" })).toBeTruthy();
    fireEvent.keyDown(sheet, { key: "Escape" });
    expect(screen.queryByRole("dialog", { name: "Ask" })).toBeNull();

    openFromLauncher("Files");
    const bar = screen.getByRole("dialog", { name: "Files" }).querySelector(".os-window-bar") as HTMLElement;
    expect(within(bar).queryByRole("button", { name: "Ask about Files" })).toBeNull();
    expect(within(bar).queryByRole("button", { name: "Search Files" })).toBeNull();
    expect(within(bar).getByRole("button", { name: "Close Files" })).toBeTruthy();

    const widget = document.querySelector("[data-os-widget='ask']") as HTMLElement;
    expect(within(widget).getByRole("textbox", { name: "Ask" })).toBeTruthy();
  });

  it("keeps a text question while Send is blocked without a cluster connection", () => {
    renderShell();
    fireEvent.click(screen.getByRole("button", { name: "Ask" }));
    const sheet = screen.getByRole("dialog", { name: "Ask" });
    const input = within(sheet).getByRole("textbox", { name: "Ask" });
    fireEvent.change(input, { target: { value: "what is this cluster" } });
    fireEvent.click(within(sheet).getByRole("button", { name: "Send" }));
    expect((within(sheet).getByRole("button", { name: "Send" }) as HTMLButtonElement).disabled).toBe(true);
    expect((input as HTMLInputElement).value).toBe("what is this cluster");
    expect(within(sheet).queryByText(/Not connected to the cluster/)).toBeNull();
    expect(within(sheet).queryByRole("button", { name: "Open Fleet" })).toBeNull();
  });

  // The harness passes askVoice={null} (jsdom has no audio stack), which is
  // the "this window has no voice wiring" case -- and the rule it pins is
  // that the control stays PRESENT and accounts for itself, rather than
  // disappearing or sitting there dead. Voice's own behaviour is tested
  // against the pure session in test/ask/, with no shell and no DOM.
  it("an unwired mic control says so instead of going quiet", () => {
    renderShell();
    fireEvent.click(screen.getByRole("button", { name: "Ask" }));
    const sheet = screen.getByRole("dialog", { name: "Ask" });
    const mic = within(sheet).getByRole("button", { name: "Dictate a message" }) as HTMLButtonElement;
    expect(mic.disabled).toBe(true);
    expect(within(sheet).getByText(/Voice is not wired up in this window/)).toBeTruthy();
  });
});

describe("desktop items (spec K bullet 4)", () => {
  it("uploads a dropped host file and lands it as a desk file", async () => {
    renderShell();
    const plate = document.querySelector(".os-plate") as HTMLElement;
    const file = new File(["hello"], "notes.txt", { type: "text/plain" });
    fireEvent.drop(plate, { dataTransfer: { files: [file], types: ["Files"] } });
    expect(await screen.findByText("notes.txt")).toBeTruthy();
    expect(
      await screen.findByRole("button", { name: "notes.txt -- open in VS Code" }),
    ).toBeTruthy();
  });

  it("refuses desk folder creation while no cluster connection exists", () => {
    // A desk folder IS a Library folder now (design D4), so creating one is
    // a write the cluster must confirm. This shell renders with the
    // connection disabled, and the menu says so by refusing -- a shortcut to
    // a folder that was never created would be a control pointing at
    // nothing. The connected path is covered in test/files/desk.test.tsx.
    renderShell();
    const plate = document.querySelector(".os-plate") as HTMLElement;
    fireEvent.contextMenu(plate);
    const entry = screen.getByRole("menuitem", { name: "New folder" });
    expect((entry as HTMLButtonElement).disabled).toBe(true);
  });

  it("removes the seeded widget through its menu, leaving the empty-desk hint", () => {
    // THE SEED IS ASK ALONE, FOR EVERY ROLE (epic memql#5118, D4), so removing
    // it leaves a genuinely empty desk and the hint comes back.
    //
    // This case used to assert the opposite for an owner, on the grounds that
    // the seed placed the Set up wizard beneath Ask. It never did: the seed
    // runs in a state initializer, before the role ladder lands, and
    // `roleAdmits("")` refused the widget on every production boot. The
    // wizard's presence is derived now, from the readiness feed and the
    // ladder, so a shell dialling nothing places nothing.
    for (const access of [OWNER, READER]) {
      cleanup();
      renderShell({ access });
      fireEvent.click(screen.getByRole("button", { name: "Ask widget menu" }));
      fireEvent.click(screen.getByRole("menuitem", { name: "Remove from desk" }));
      expect(document.querySelector("[data-os-widget='ask']")).toBeNull();
      expect(screen.getByText("Drop a file, or open the Launcher.")).toBeTruthy();
    }
  });
});

describe("the VS Code handoff (spec D3)", () => {
  it("opens a single file in browser VS Code by default", () => {
    saveEditorPreference("browser");
    const url = new URL(artifactHandoffUrl("acme.example.com", "abc", "report.pdf"));
    expect(url.origin).toBe("https://vscode.acme.example.com");
    expect(url.searchParams.get("resource")).toBe("memql-file://acme.example.com/artifacts/abc/report.pdf");
  });

  it("fires the URL and reports no-answer only while the page stays visible", () => {
    saveEditorPreference("vscode");
    const fired: string[] = [];
    let armed: (() => void) | null = null;
    const noAnswer = vi.fn();
    openInVsCode("acme.example.com", "a-1", noAnswer, {
      navigate: (url) => fired.push(url),
      schedule: (fn) => {
        armed = fn;
        return () => {};
      },
    });
    expect(fired).toHaveLength(1);
    expect(noAnswer).not.toHaveBeenCalled();
    armed!();
    expect(noAnswer).toHaveBeenCalledTimes(1);
    saveEditorPreference("browser");
  });
});

describe("phone chrome (spec D13)", () => {
  it("has no desks, windows or pins -- a tab bar and one app at a time", async () => {
    renderShell({ layout: "phone" });
    expect(document.querySelector("[data-os-desktop]")).toBeNull();
    expect(document.querySelector("[data-os-dock]")).toBeNull();
    expect(document.querySelector("[data-os-phone]")).not.toBeNull();

    expect(screen.queryByRole("button", { name: appTileName("Profile") })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Open Profile" }));
    fireEvent.click(screen.getByRole("button", { name: "About" }));
    expect(await screen.findByRole("heading", { name: "About" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Back to home" }));
    expect(screen.queryByRole("heading", { name: "About" })).toBeNull();
  });
});

describe("right-click (the shell owns it)", () => {
  it("suppresses the browser menu on a control inside a window", () => {
    renderShell();
    // Anything the shell renders and does not offer a menu for: the browser's
    // own Back / Reload / View Page Source over a desktop window is the
    // loudest tell that this is a tab.
    const dock = document.querySelector(".os-dock") as HTMLElement;
    expect(dock).toBeTruthy();
    const event = new MouseEvent("contextmenu", { bubbles: true, cancelable: true });
    dock.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
  });

  it("covers the PHONE shell too, which is a separate root", () => {
    // Two roots render the shell, and a rule attached to one of them is a
    // rule that holds on half the product.
    renderShell({ layout: "phone" });
    const root = document.querySelector("[data-os-root]") as HTMLElement;
    expect(root).toBeTruthy();
    const event = new MouseEvent("contextmenu", { bubbles: true, cancelable: true });
    root.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
  });

  it("still lets a text field have cut/copy/paste", () => {
    renderShell();
    const input = document.createElement("input");
    (document.querySelector("[data-os-root]") as HTMLElement).appendChild(input);
    const event = new MouseEvent("contextmenu", { bubbles: true, cancelable: true });
    input.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(false);
  });
});


it("switches through registry apps without remounting the prior window or losing its section", () => {
  renderShell();
  openFromLauncher("Fleet");
  fireEvent.click(screen.getByRole("button", { name: "Model library" }));
  const original = document.querySelector("[data-os-window='fleet']");
  // Full screen keeps the desk to itself, so Files opens on a desk of its own.
  fireEvent.click(screen.getByRole("button", { name: "Make Fleet borderless" }));
  fireEvent.click(screen.getByRole("button", { name: "Make Fleet full screen" }));
  expect(document.querySelector("[data-os-window='fleet']")).toBe(original);
  chooseOption(screen.getByRole("combobox", { name: "Switch app from Fleet" }), "Files");
  expect(original?.hasAttribute("hidden")).toBe(true);
  chooseOption(screen.getByRole("combobox", { name: "Switch app from Files" }), "Fleet");
  expect(document.querySelector("[data-os-window='fleet']")).toBe(original);
  expect(original?.hasAttribute("hidden")).toBe(false);
  expect(original?.hasAttribute("data-fullscreen")).toBe(true);
  expect(within(original as HTMLElement).getByRole("button", { name: "Model library" }).getAttribute("aria-current")).toBe("page");
});

// Search lives in the page, not the title bar (José, Nov 19 2026).
it('Fleet searches machines from its own page button, not the title bar',async()=>{
 renderShell();openFromLauncher('Fleet');
 const fleet=within(screen.getByRole('dialog',{name:'Fleet'}));
 expect(fleet.queryByRole('button',{name:'Search Fleet'})).toBeNull();
 fireEvent.click(fleet.getByRole('button',{name:'Machines'}));
 fireEvent.click(await fleet.findByRole('button',{name:'Find machines'}));
 const search = await fleet.findByPlaceholderText('Search machines');
 await waitFor(() => expect(search).toBe(document.activeElement));
});


describe("one Email app", () => {
  it("lists Email in the launcher for every installation, and no Campaigns app", () => {
    renderShell({ config: CONFIG });
    fireEvent.click(screen.getByRole("button", { name: "Launcher" }));
    expect(screen.getByRole("button", { name: appTileName("Email") })).toBeTruthy();
    expect(screen.queryByRole("button", { name: appTileName("Campaigns") })).toBeNull();
  });
});
