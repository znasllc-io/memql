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
});

describe("SortControl (rule 3: sort is not a button)", () => {
  it("reads as quiet text whose name says what a click does, and toggles", () => {
    const onToggle = vi.fn();
    const { rerender } = render(<SortControl ascending={false} onToggle={onToggle} />);
    const control = screen.getByRole("button", {
      name: "Sorted newest first -- switch to oldest first",
    });
    expect(control.className).toBe("os-sort");
    fireEvent.click(control);
    expect(onToggle).toHaveBeenCalledOnce();
    rerender(<SortControl ascending onToggle={onToggle} />);
    expect(
      screen.getByRole("button", { name: "Sorted oldest first -- switch to newest first" }),
    ).toBeTruthy();
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
