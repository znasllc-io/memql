// LabelChips -- the shared chip editor for free-text labels (task 3 of the
// artifacts-labels epic). A pure presentational component: it takes its
// state as props and reports intent through onAdd/onRemove, so unlike most
// of this test/ directory there is no Connection, no provider tree and no
// route to stand up -- rendering it needs nothing but React.

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";

import { LabelChips } from "../src/ui/LabelChips";

describe("LabelChips", () => {
  it("renders one chip per label", () => {
    render(<LabelChips labels={["alpha", "beta"]} onAdd={() => {}} onRemove={() => {}} />);
    expect(screen.getByText("alpha")).toBeTruthy();
    expect(screen.getByText("beta")).toBeTruthy();
  });

  it("calls onRemove with the label its own control belongs to", () => {
    const onRemove = vi.fn();
    render(<LabelChips labels={["alpha", "beta"]} onAdd={() => {}} onRemove={onRemove} />);
    // The accessible name has to NAME the label it removes -- two chips
    // means two controls, and a generic "Remove" would leave a screen-reader
    // user unable to tell them apart.
    fireEvent.click(screen.getByRole("button", { name: "Remove label beta" }));
    expect(onRemove).toHaveBeenCalledTimes(1);
    expect(onRemove).toHaveBeenCalledWith("beta");
  });

  it("adds the trimmed value on Enter and clears the input", () => {
    const onAdd = vi.fn();
    render(<LabelChips labels={["existing"]} onAdd={onAdd} onRemove={() => {}} />);
    const input = screen.getByLabelText("Add a label") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "  fresh  " } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onAdd).toHaveBeenCalledTimes(1);
    expect(onAdd).toHaveBeenCalledWith("fresh");
    expect(input.value).toBe("");
  });

  it("calls nothing for a blank or whitespace-only entry", () => {
    const onAdd = vi.fn();
    render(<LabelChips labels={[]} onAdd={onAdd} onRemove={() => {}} />);
    const input = screen.getByLabelText("Add a label") as HTMLInputElement;

    fireEvent.change(input, { target: { value: "" } });
    fireEvent.keyDown(input, { key: "Enter" });

    fireEvent.change(input, { target: { value: "   " } });
    fireEvent.keyDown(input, { key: "Enter" });

    expect(onAdd).not.toHaveBeenCalled();
  });

  it("calls nothing for a label that is already on the list", () => {
    const onAdd = vi.fn();
    render(<LabelChips labels={["alpha"]} onAdd={onAdd} onRemove={() => {}} />);
    const input = screen.getByLabelText("Add a label") as HTMLInputElement;
    // Padded with whitespace on purpose: the duplicate check has to compare
    // against the TRIMMED value, the same value onAdd would receive.
    fireEvent.change(input, { target: { value: "  alpha  " } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onAdd).not.toHaveBeenCalled();
  });

  it("renders chips with no controls when readOnly", () => {
    render(<LabelChips labels={["alpha"]} onAdd={() => {}} onRemove={() => {}} readOnly />);
    expect(screen.getByText("alpha")).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.queryByRole("textbox")).toBeNull();
  });

  it("disables every control while busy, without hiding them", () => {
    render(<LabelChips labels={["alpha"]} onAdd={() => {}} onRemove={() => {}} busy />);
    const removeButton = screen.getByRole("button", { name: "Remove label alpha" }) as HTMLButtonElement;
    expect(removeButton.disabled).toBe(true);
    const input = screen.getByLabelText("Add a label") as HTMLInputElement;
    expect(input.disabled).toBe(true);
  });
});
