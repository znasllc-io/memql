import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { Submenu } from "../../src/kit/Submenu";

afterEach(() => vi.useRealTimers());

function renderMenu(make = vi.fn(), onOuterKey = vi.fn()) {
  render(
    <div role="menu" aria-label="Outer" onKeyDown={(event) => onOuterKey(event.key)}>
      <Submenu label="New file">
        <button type="button" role="menuitem" onClick={make}>Markdown file</button>
      </Submenu>
    </div>,
  );
  return { row: screen.getByRole("menuitem", { name: "New file" }), make };
}

it("opens on hover and closes a moment after the pointer leaves", () => {
  vi.useFakeTimers();
  const { row } = renderMenu();
  expect(row.getAttribute("aria-expanded")).toBe("false");
  fireEvent.mouseEnter(row.parentElement!);
  expect(screen.getByRole("menu", { name: "New file" })).toBeTruthy();
  expect(row.getAttribute("aria-expanded")).toBe("true");
  fireEvent.mouseLeave(row.parentElement!);
  fireEvent.mouseEnter(row.parentElement!); // back in time: stays open
  act(() => vi.advanceTimersByTime(300));
  expect(screen.getByRole("menu", { name: "New file" })).toBeTruthy();
  fireEvent.mouseLeave(row.parentElement!);
  act(() => vi.advanceTimersByTime(300));
  expect(screen.queryByRole("menu", { name: "New file" })).toBeNull();
});

it("clicking the row itself does nothing", () => {
  const { row, make } = renderMenu();
  fireEvent.click(row);
  expect(screen.queryByRole("menu", { name: "New file" })).toBeNull();
  expect(make).not.toHaveBeenCalled();
});

it.each(["ArrowRight", "Enter", " "])("%s opens it and focuses the first entry", (key) => {
  const { row } = renderMenu();
  fireEvent.keyDown(row, { key });
  expect(document.activeElement).toBe(screen.getByRole("menuitem", { name: "Markdown file" }));
});

it.each(["ArrowLeft", "Escape"])("%s returns to the row without closing the outer menu", (key) => {
  const outer = vi.fn();
  const { row } = renderMenu(vi.fn(), outer);
  fireEvent.keyDown(row, { key: "ArrowRight" });
  fireEvent.keyDown(screen.getByRole("menuitem", { name: "Markdown file" }), { key });
  expect(screen.queryByRole("menu", { name: "New file" })).toBeNull();
  expect(document.activeElement).toBe(row);
  expect(outer).not.toHaveBeenCalledWith(key);
});

it("flips to the left when the right side has no room", () => {
  const { row } = renderMenu();
  vi.spyOn(row, "getBoundingClientRect").mockReturnValue({ right: window.innerWidth - 10 } as DOMRect);
  vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockReturnValue(190);
  fireEvent.mouseEnter(row.parentElement!);
  expect(screen.getByRole("menu", { name: "New file" }).dataset.side).toBe("left");
  vi.restoreAllMocks();
});
