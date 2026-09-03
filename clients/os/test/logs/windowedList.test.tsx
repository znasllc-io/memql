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
  follow: initial = false,
  onFollow,
  onSelect,
}: {
  rows: Item[];
  follow?: boolean;
  onFollow?: (follow: boolean) => void;
  onSelect?: (id: string) => void;
}) {
  const [follow, setFollow] = useState(initial);
  const [selected, setSelected] = useState("");
  return (
    <WindowedList
      rows={rows}
      rowHeight={30}
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
