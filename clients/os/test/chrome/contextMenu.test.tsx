import { fireEvent, render } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ContextMenu, placeContextMenu, type MenuEntry } from "../../src/chrome/ContextMenu";

// The shell's one context menu. Two things are under test and they are the
// same thing twice: WHERE the node lives in the DOM, and where it therefore
// ends up on screen.
//
// The bug this suite exists for: `.os-context-menu` is `position: fixed`, and
// a `backdrop-filter` on `.os-window` (or a `transform` on the desk plate)
// makes that ancestor the containing block for fixed descendants -- so the
// menu opened against the WINDOW rather than the viewport and landed a whole
// window offset from the cursor. jsdom computes no containing blocks, so the
// assertion that stands in for it is structural: the menu must not be a
// descendant of the surface that opened it.

const ENTRIES: MenuEntry[] = [
  { id: "open", label: "Open", onSelect: () => {} },
  { id: "rename", label: "Rename", onSelect: () => {} },
  { id: "archive", label: "Move to Bin", onSelect: () => {} },
];

const restore: Array<() => void> = [];

afterEach(() => {
  while (restore.length > 0) restore.pop()?.();
  vi.restoreAllMocks();
});

/** Pin the viewport, the way a browser would report it. */
function viewport(width: number, height: number) {
  for (const [key, value] of [
    ["innerWidth", width],
    ["innerHeight", height],
  ] as const) {
    const original = Object.getOwnPropertyDescriptor(window, key);
    Object.defineProperty(window, key, { configurable: true, value });
    restore.push(() => {
      if (original) Object.defineProperty(window, key, original);
    });
  }
}

/**
 * Give the menu a real size. jsdom lays nothing out, so every rect is zero
 * and nothing would ever clamp -- the arithmetic under test would be
 * measured against a menu with no width.
 */
function menuMeasures(width: number, height: number) {
  const zero = new DOMRect(0, 0, 0, 0);
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
    return this.classList.contains("os-context-menu") ? new DOMRect(0, 0, width, height) : zero;
  });
}

/** Render the menu the way an app does: inside its own surface. */
function openIn(props: { x: number; y: number; onClose?: () => void }) {
  const view = render(
    <div className="os-window" data-testid="surface">
      <div className="os-files">
        <ContextMenu
          x={props.x}
          y={props.y}
          label="File"
          entries={ENTRIES}
          onClose={props.onClose ?? (() => {})}
        />
      </div>
    </div>,
  );
  const menu = document.body.querySelector<HTMLElement>('[role="menu"]');
  if (menu === null) throw new Error("no menu rendered");
  return { view, menu, surface: view.getByTestId("surface") };
}

describe("the shell's context menu", () => {
  it("renders into document.body, outside the surface that opened it", () => {
    // THE REGRESSION TEST. Inside `.os-window` the menu's `position: fixed`
    // resolves against the window's backdrop-filtered box, not the viewport,
    // and the menu opens a window offset away from the cursor.
    const { view, menu, surface } = openIn({ x: 120, y: 80 });

    expect(menu.parentElement).toBe(document.body);
    expect(surface.contains(menu)).toBe(false);
    expect(view.container.contains(menu)).toBe(false);
    // The caller's subtree renders the component and holds none of its DOM.
    expect(view.container.querySelector('[role="menu"]')).toBeNull();
  });

  it("puts its corner exactly on the raw viewport coordinates it was given", () => {
    // The props are `event.clientX` / `event.clientY` and nothing else. A
    // caller subtracting a container rect is the bug, not the fix.
    viewport(1024, 768);
    menuMeasures(190, 120);
    const { menu } = openIn({ x: 300, y: 210 });

    expect(menu.style.left).toBe("300px");
    expect(menu.style.top).toBe("210px");
  });

  it("opens leftwards when the click is too near the right edge", () => {
    viewport(1024, 768);
    menuMeasures(200, 120);
    const { menu } = openIn({ x: 980, y: 100 });

    // 980 + 200 runs 156px past the edge, so the menu's RIGHT edge takes the
    // click instead: 980 - 200.
    expect(menu.style.left).toBe("780px");
    expect(menu.style.top).toBe("100px");
  });

  it("opens upwards when the click is too near the bottom edge", () => {
    viewport(1024, 768);
    menuMeasures(200, 300);
    const { menu } = openIn({ x: 100, y: 700 });

    expect(menu.style.left).toBe("100px");
    expect(menu.style.top).toBe("400px");
  });

  it("pins inside the margin when the flip would run off the near edge too", () => {
    // A menu wider than the room on either side of the click has nowhere to
    // flip to. Pinning it at the margin is the last resort; running off the
    // near edge as well is not.
    viewport(360, 320);
    menuMeasures(300, 280);
    const { menu } = openIn({ x: 200, y: 150 });

    expect(menu.style.left).toBe("8px");
    expect(menu.style.top).toBe("8px");
  });

  it("flips only on the axis that overflows", () => {
    expect(
      placeContextMenu({
        x: 500,
        y: 700,
        width: 190,
        height: 200,
        viewportWidth: 1024,
        viewportHeight: 768,
      }),
    ).toEqual({ left: 500, top: 500 });
    expect(
      placeContextMenu({
        x: 1000,
        y: 100,
        width: 190,
        height: 200,
        viewportWidth: 1024,
        viewportHeight: 768,
      }),
    ).toEqual({ left: 810, top: 100 });
  });

  it("focuses the first ENABLED entry on open", () => {
    const view = render(
      <ContextMenu
        x={10}
        y={10}
        label="File"
        entries={[
          { id: "a", label: "Restore", disabled: true, onSelect: () => {} },
          { id: "b", label: "Move to Bin", onSelect: () => {} },
        ]}
        onClose={() => {}}
      />,
    );
    expect(document.activeElement?.textContent).toBe("Move to Bin");
    view.unmount();
  });

  it("wraps with ArrowDown and ArrowUp", () => {
    const { menu } = openIn({ x: 10, y: 10 });
    expect(document.activeElement?.textContent).toBe("Open");

    fireEvent.keyDown(menu, { key: "ArrowDown" });
    expect(document.activeElement?.textContent).toBe("Rename");
    fireEvent.keyDown(menu, { key: "ArrowDown" });
    fireEvent.keyDown(menu, { key: "ArrowDown" });
    // Past the last entry is the first one again.
    expect(document.activeElement?.textContent).toBe("Open");
    fireEvent.keyDown(menu, { key: "ArrowUp" });
    expect(document.activeElement?.textContent).toBe("Move to Bin");
  });

  it("closes on Escape and does not let it reach the surface behind", () => {
    // A portal bubbles through the REACT tree, so an Escape the menu handled
    // would otherwise also close the window it was opened over.
    const onClose = vi.fn();
    const outer = vi.fn();
    const view = render(
      <div onKeyDown={outer}>
        <ContextMenu x={10} y={10} label="File" entries={ENTRIES} onClose={onClose} />
      </div>,
    );
    const menu = document.body.querySelector<HTMLElement>('[role="menu"]');
    fireEvent.keyDown(menu as HTMLElement, { key: "Escape" });

    expect(onClose).toHaveBeenCalledTimes(1);
    expect(outer).not.toHaveBeenCalled();
    view.unmount();
  });

  it("closes on an outside pointerdown and stays open on one inside it", () => {
    // The containment test runs against the PORTALLED node, which is what
    // keeps this working now that the menu is not under the shell root.
    const onClose = vi.fn();
    const { menu, surface } = openIn({ x: 10, y: 10, onClose });

    fireEvent.pointerDown(menu.querySelector("button") as HTMLElement);
    expect(onClose).not.toHaveBeenCalled();

    fireEvent.pointerDown(surface);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("keeps the browser's menu off itself", () => {
    // The shell's right-click rule: a surface with its own menu says so. The
    // root handler that would re-enable the browser's is a React ancestor,
    // and a portal still bubbles to it.
    const { menu } = openIn({ x: 10, y: 10 });
    const event = new MouseEvent("contextmenu", { bubbles: true, cancelable: true });
    menu.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
  });

  it("runs an entry and closes, in that order", () => {
    const onClose = vi.fn();
    const onSelect = vi.fn(() => expect(onClose).toHaveBeenCalled());
    const view = render(
      <ContextMenu
        x={10}
        y={10}
        label="File"
        entries={[{ id: "open", label: "Open", onSelect }]}
        onClose={onClose}
      />,
    );
    fireEvent.click(document.body.querySelector('[role="menuitem"]') as HTMLElement);
    expect(onSelect).toHaveBeenCalledTimes(1);
    view.unmount();
  });
});
