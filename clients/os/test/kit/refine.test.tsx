import { fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import { Refine, Select, SortControl } from "../../src/kit";

// The Refine affordance and the quiet sort (epic memql#4848, #4849):
// DESIGN.md rules 2 and 3 as behaviour. Geometry is the visual QA pass's to
// judge; what a test can pin is the disclosure contract and the accessible
// names.

function Harness({ chips = [] as Array<{ id: string; label: string; onRemove: () => void }> }) {
  const [search, setSearch] = useState("");
  return (
    <Refine search={search} onSearch={setSearch} label="Refine files" chips={chips}>
      <span data-testid="facet">facet controls</span>
    </Refine>
  );
}

describe("Refine (rule 2: filters are questions, not furniture)", () => {
  it("is collapsed by default and opens into the panel with the search focused", () => {
    render(<Harness />);
    expect(screen.queryByTestId("facet")).toBeNull();
    const open = screen.getByRole("button", { name: "Refine files" });
    expect(open.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(open);
    expect(screen.getByTestId("facet")).toBeTruthy();
    expect(open.getAttribute("aria-expanded")).toBe("true");
    expect(document.activeElement).toBe(screen.getByPlaceholderText("Search"));
  });

  it("typing lands in the search, shows on the collapsed affordance, and marks it active", () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole("button", { name: "Refine files" }));
    fireEvent.change(screen.getByPlaceholderText("Search"), { target: { value: "report" } });
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByTestId("facet")).toBeNull();
    const open = screen.getByRole("button", { name: "Refine files" });
    expect(open.textContent).toContain("report");
    expect(open.getAttribute("data-active")).not.toBeNull();
  });

  it("Escape and clicking elsewhere both collapse the panel", () => {
    render(
      <div>
        <button type="button">elsewhere</button>
        <Harness />
      </div>,
    );
    const open = screen.getByRole("button", { name: "Refine files" });
    fireEvent.click(open);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByTestId("facet")).toBeNull();
    fireEvent.click(open);
    fireEvent.pointerDown(screen.getByRole("button", { name: "elsewhere" }));
    expect(screen.queryByTestId("facet")).toBeNull();
  });

  it("active constraints render as chips that remove in place, visible while collapsed", () => {
    const onRemove = vi.fn();
    render(<Harness chips={[{ id: "kind", label: "Documents", onRemove }]} />);
    expect(screen.getByText("Documents")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove Documents" }));
    expect(onRemove).toHaveBeenCalledOnce();
  });

  it("a facet's portalled listbox is inside the panel: the mouse can choose an option (SVC: mouse and keyboard reach every action)", () => {
    // The kit's Select draws its list through a portal on <body> (the
    // containing-block trap select.test.tsx pins), so its options are OUTSIDE
    // this Refine's DOM subtree even though they are inside its React
    // subtree. A pointerdown on one used to read as "clicked elsewhere",
    // collapse the panel, and unmount the list before the click that would
    // have committed the choice -- nine sections' facets were keyboard-only.
    function Facets() {
      const [search, setSearch] = useState("");
      const [category, setCategory] = useState("all");
      return (
        <Refine search={search} onSearch={setSearch} label="Refine events">
          <Select id="facet-category" label="Category" value={category} onChange={setCategory}>
            <option value="all">Every category</option>
            <option value="configuration">configuration</option>
          </Select>
        </Refine>
      );
    }
    render(<Facets />);
    fireEvent.click(screen.getByRole("button", { name: "Refine events" }));
    fireEvent.click(screen.getByRole("combobox", { name: "Category" }));
    const option = screen.getByRole("option", { name: "configuration" });
    expect(option.closest(".os-refine")).toBeNull(); // the trap: portalled out of the panel's subtree
    fireEvent.pointerDown(option);
    fireEvent.mouseDown(option);
    fireEvent.click(option);
    expect(screen.getByRole("combobox", { name: "Category" }).textContent).toContain("configuration");
    expect(screen.getByPlaceholderText("Search")).toBeTruthy(); // the panel stayed open
  });
});

describe("SortControl", () => {
  it("switches orders directly and keeps focus on the same control", () => {
    function SortHarness() {
      const [ascending, setAscending] = useState(false);
      return <SortControl ascending={ascending} onToggle={() => setAscending(value => !value)} />;
    }
    render(<SortHarness />);
    const control = screen.getByRole("button", { name: "Sort order: Newest first" });
    control.focus();
    fireEvent.click(control);
    expect(screen.getByRole("button", { name: "Sort order: Oldest first" })).toBe(control);
    expect(document.activeElement).toBe(control);
    expect(screen.queryByRole("listbox")).toBeNull();
    fireEvent.click(control);
    expect(control.textContent).toBe("Newest first");
  });
});

describe("Select (rule 5: no UA chrome)", () => {
  it("wraps the select with the drawn chevron", () => {
    render(
      <Select id="s" label="Source" value="a" onChange={() => {}}>
        <option value="a">a</option>
      </Select>,
    );
    const select = screen.getByLabelText("Source");
    expect(select.parentElement?.className).toBe("os-select-wrap");
    expect(select.parentElement?.querySelector(".os-select-chevron")).toBeTruthy();
  });
});

// The window title bar has no search button (José, Nov 19 2026): every
// Refine draws its own, in the page, and Escape hands focus back to it.
it('draws its own search button, focuses the field and returns focus on Escape',()=>{
 render(<Refine search="" onSearch={()=>{}} label="Refine Fleet" placeholder="Find Fleet"><span>Fleet filters</span></Refine>);
 const trigger=screen.getByRole('button',{name:'Refine Fleet'});fireEvent.click(trigger);
 expect(document.activeElement).toBe(screen.getByPlaceholderText('Find Fleet'));
 fireEvent.keyDown(document,{key:'Escape'});
 expect(screen.queryByPlaceholderText('Find Fleet')).toBeNull();
 expect(document.activeElement).toBe(trigger);
});

describe("Refine's panel near the window's left edge (Jesús, Nov 23 2026: Downloads cut it off)", () => {
  // jsdom lays nothing out, so the two boxes the check compares are given.
  const box = (left: number) => ({ left, right: left + 264, top: 0, bottom: 40, width: 264, height: 40, x: left, y: 0, toJSON: () => ({}) }) as DOMRect;
  function inWindow() {
    return render(
      <div className="os-window">
        <Harness />
      </div>,
    );
  }

  it("opens rightward when leftward would leave the window", () => {
    const spy = vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
      return this.classList.contains("os-window") ? box(100) : box(this.classList.contains("os-refine-panel") ? 40 : 200);
    });
    inWindow();
    fireEvent.click(screen.getByRole("button", { name: "Refine files" }));
    expect(document.querySelector(".os-refine-panel")?.getAttribute("data-align")).toBe("start");
    spy.mockRestore();
  });

  it("keeps opening leftward where there is room", () => {
    const spy = vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
      return this.classList.contains("os-window") ? box(0) : box(600);
    });
    inWindow();
    fireEvent.click(screen.getByRole("button", { name: "Refine files" }));
    expect(document.querySelector(".os-refine-panel")?.hasAttribute("data-align")).toBe(false);
    spy.mockRestore();
  });
});
