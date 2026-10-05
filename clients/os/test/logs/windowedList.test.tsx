import { useState } from "react";
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { WindowedList } from "../../src/logs/WindowedList";

// The windowed list (epic memql#4895, spec H): a ten-thousand-row fixture
// puts a bounded number of rows in the DOM, following pins to the bottom,
// scrolling away reports itself, and the keyboard moves a cursor.

interface Item {
  id: string;
}

function items(n: number): Item[] {
  return Array.from({ length: n }, (_, i) => ({ id: `r-${i}` }));
}

function Harness({
  rows,
  rowHeight = 30,
  follow: initial = false,
  onFollow,
  onSelect,
}: {
  rows: Item[];
  rowHeight?: number;
  follow?: boolean;
  onFollow?: (follow: boolean) => void;
  onSelect?: (id: string) => void;
}) {
  const [follow, setFollow] = useState(initial);
  const [selected, setSelected] = useState("");
  return (
    <WindowedList
      rows={rows}
      rowHeight={rowHeight}
      renderRow={(row) => <span role="gridcell">{row.id}</span>}
      rowId={(row) => row.id}
      selectedId={selected}
      onSelect={(id) => {
        setSelected(id);
        onSelect?.(id);
      }}
      follow={follow}
      onFollowChange={(next) => {
        setFollow(next);
        onFollow?.(next);
      }}
      label="Test rows"
    />
  );
}

function grid(): HTMLElement {
  return screen.getByRole("grid", { name: "Test rows" });
}

/** jsdom lays nothing out; give the scroll box a geometry of its own. */
function geometry(el: HTMLElement, scrollTop: number, clientHeight: number): void {
  Object.defineProperty(el, "scrollTop", { value: scrollTop, writable: true, configurable: true });
  Object.defineProperty(el, "clientHeight", { value: clientHeight, configurable: true });
}

describe("the windowed list", () => {
  it("holds a bounded number of ten thousand rows in the DOM, and says how many there are", () => {
    render(<Harness rows={items(10_000)} />);
    const rendered = screen.getAllByRole("row");
    expect(rendered.length).toBeGreaterThan(0);
    expect(rendered.length).toBeLessThan(60);
    expect(grid().getAttribute("aria-rowcount")).toBe("10000");
    // The spacer is honest about the whole height, so the scrollbar is too.
    const spacer = grid().querySelector(".os-vlist-space") as HTMLElement;
    expect(spacer.style.height).toBe("300000px");
    // Unfollowed, the list starts at the top.
    expect(screen.getByText("r-0")).toBeTruthy();
    expect(screen.queryByText("r-9999")).toBeNull();
  });

  it("pins to the bottom while following, and keeps pinning as rows arrive", () => {
    const view = render(<Harness rows={items(1_000)} follow />);
    expect(screen.getByText("r-999")).toBeTruthy();
    expect(screen.queryByText("r-0")).toBeNull();
    view.rerender(<Harness rows={items(1_200)} follow />);
    expect(screen.getByText("r-1199")).toBeTruthy();
  });

  it("reports scrolling away from the bottom, and coming back", () => {
    const onFollow = vi.fn();
    render(<Harness rows={items(1_000)} follow onFollow={onFollow} />);
    const el = grid();
    // More than a row from the bottom: the reader has scrolled up.
    geometry(el, 0, 480);
    fireEvent.scroll(el);
    expect(onFollow).toHaveBeenLastCalledWith(false);
    // Back within a row of it: following again.
    geometry(el, 30_000 - 480, 480);
    fireEvent.scroll(el);
    expect(onFollow).toHaveBeenLastCalledWith(true);
  });

  it("moves a cursor with the arrows, selects on Enter and clears on Escape", () => {
    const onSelect = vi.fn();
    render(<Harness rows={items(50)} onSelect={onSelect} />);
    const el = grid();
    el.focus();
    fireEvent.keyDown(el, { key: "ArrowDown" });
    expect(el.getAttribute("aria-activedescendant")).toBe("os-vlist-row-0");
    fireEvent.keyDown(el, { key: "ArrowDown" });
    expect(el.getAttribute("aria-activedescendant")).toBe("os-vlist-row-1");
    fireEvent.keyDown(el, { key: "Enter" });
    expect(onSelect).toHaveBeenLastCalledWith("r-1");
    expect(screen.getByText("r-1").closest("[role=row]")?.getAttribute("aria-selected")).toBe("true");
    fireEvent.keyDown(el, { key: "End" });
    expect(el.getAttribute("aria-activedescendant")).toBe("os-vlist-row-49");
    fireEvent.keyDown(el, { key: "Home" });
    expect(el.getAttribute("aria-activedescendant")).toBe("os-vlist-row-0");
    fireEvent.keyDown(el, { key: "Escape" });
    expect(onSelect).toHaveBeenLastCalledWith("");
    expect(el.getAttribute("aria-activedescendant")).toBeNull();
  });

  it("arrowing away from the last row while following stops following; End resumes it", () => {
    const onFollow = vi.fn();
    render(<Harness rows={items(50)} follow onFollow={onFollow} />);
    const el = grid();
    fireEvent.keyDown(el, { key: "ArrowUp" });
    // ArrowUp with no cursor lands on the last row, which is still the bottom.
    expect(onFollow).not.toHaveBeenCalled();
    fireEvent.keyDown(el, { key: "ArrowUp" });
    expect(onFollow).toHaveBeenLastCalledWith(false);
    fireEvent.keyDown(el, { key: "End" });
    expect(onFollow).toHaveBeenLastCalledWith(true);
  });

  it("selects a row on click", () => {
    const onSelect = vi.fn();
    render(<Harness rows={items(5)} onSelect={onSelect} />);
    fireEvent.click(screen.getByText("r-3").closest("[role=row]") as HTMLElement);
    expect(onSelect).toHaveBeenCalledWith("r-3");
  });
});

// A log list's rows change height when it narrows past its measure and stack
// each line on two rows (R40), and back again. The scroll position is pixels,
// so the list has to keep the reader's place itself.
describe("a new row height", () => {
  /** The cursor's row, as assistive technology resolves it: the element the
   *  grid's aria-activedescendant names, or null when it names none. */
  function cursorRow(): HTMLElement | null {
    const named = grid().getAttribute("aria-activedescendant");
    return named === null ? null : document.getElementById(named);
  }

  it("keeps the first visible row at the top, to the fraction", () => {
    const rows = items(1_000);
    const view = render(<Harness rows={rows} />);
    const el = grid();
    // Halfway through row 100.
    geometry(el, 100 * 30 + 15, 480);
    fireEvent.scroll(el);

    view.rerender(<Harness rows={rows} rowHeight={48} />);
    expect(el.scrollTop).toBe(100 * 48 + 24);
    expect(screen.getByText("r-100")).toBeTruthy();

    view.rerender(<Harness rows={rows} rowHeight={30} />);
    expect(el.scrollTop).toBe(100 * 30 + 15);
  });

  it("keeps the cursor's row where it was in the view, and named", () => {
    const rows = items(1_000);
    const view = render(<Harness rows={rows} />);
    const el = grid();
    geometry(el, 3_000, 480);
    fireEvent.scroll(el);
    // The cursor on row 105, 150px down the view.
    fireEvent.click(screen.getByText("r-105").closest("[role=row]") as HTMLElement);

    view.rerender(<Harness rows={rows} rowHeight={48} />);
    expect(el.scrollTop).toBe(105 * 48 - 150);
    expect(cursorRow()?.textContent).toBe("r-105");

    // Compact density's stacked height, 40, is a third arrangement.
    view.rerender(<Harness rows={rows} rowHeight={40} />);
    expect(el.scrollTop).toBe(105 * 40 - 150);
    expect(cursorRow()?.textContent).toBe("r-105");
  });

  it("keeps the whole cursor row in view when it grows at the bottom edge", () => {
    const rows = items(1_000);
    const view = render(<Harness rows={rows} />);
    const el = grid();
    geometry(el, 3_000, 480);
    fireEvent.scroll(el);
    // Row 115 is the last whole row: 450px down a 480px view.
    fireEvent.click(screen.getByText("r-115").closest("[role=row]") as HTMLElement);

    view.rerender(<Harness rows={rows} rowHeight={48} />);
    // Kept at 450px down it would end at 498, past the view; it ends at 480.
    expect(el.scrollTop).toBe(116 * 48 - 480);
    expect(cursorRow()?.textContent).toBe("r-115");
  });

  it("names no cursor row that is not rendered, and anchors on what is in view", () => {
    const rows = items(2_000);
    const view = render(<Harness rows={rows} />);
    const el = grid();
    geometry(el, 0, 480);
    el.focus();
    fireEvent.keyDown(el, { key: "ArrowDown" });
    expect(cursorRow()?.textContent).toBe("r-0");

    // The reader scrolls a thousand rows away from the cursor.
    geometry(el, 30_000, 480);
    fireEvent.scroll(el);
    expect(el.getAttribute("aria-activedescendant")).toBeNull();

    // The cursor is off screen, so the first visible row is the anchor.
    view.rerender(<Harness rows={rows} rowHeight={48} />);
    expect(el.scrollTop).toBe(1_000 * 48);
    expect(el.getAttribute("aria-activedescendant")).toBeNull();

    // The cursor was remembered: the next arrow moves it and shows its row.
    fireEvent.keyDown(el, { key: "ArrowDown" });
    expect(cursorRow()?.textContent).toBe("r-1");
  });

  it("leaves a following list pinned to the bottom", () => {
    const rows = items(1_000);
    const view = render(<Harness rows={rows} follow />);
    const el = grid();
    geometry(el, 30_000 - 480, 480);

    view.rerender(<Harness rows={rows} rowHeight={48} follow />);
    expect(el.scrollTop).toBe(1_000 * 48 - 480);
    expect(screen.getByText("r-999")).toBeTruthy();
  });
});
