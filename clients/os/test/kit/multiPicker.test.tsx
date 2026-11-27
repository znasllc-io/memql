import { useState } from "react";
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { MultiPicker } from "../../src/kit/MultiPicker";

describe("shared multi-picker", () => {
  const choices = Array.from({length: 30}, (_, index) => ({id: String(index), label: `Person ${String(index).padStart(2, "0")}`}));
  function Picker({save = () => {}, disabled = false}: {save?: (ids: string[]) => void; disabled?: boolean}) {
    const [selected, setSelected] = useState<string[]>([]);
    return <MultiPicker label="People" choices={choices} selected={selected} onChange={setSelected} disabled={disabled} add={{label: "Add people", onClick: () => save(selected)}} />;
  }
  it("searches the whole set and retains multiple choices across filtering and closing", () => {
    const save = vi.fn(); render(<Picker save={save} />);
    const input = screen.getByRole("combobox", {name: "People"});
    fireEvent.focus(input);
    expect(screen.getAllByRole("option")).toHaveLength(20);
    fireEvent.change(input, {target: {value: "Person 29"}});
    fireEvent.click(screen.getByRole("option", {name: "Person 29"}));
    fireEvent.change(input, {target: {value: "Person 00"}});
    fireEvent.click(screen.getByRole("option", {name: "Person 00"}));
    fireEvent.keyDown(input, {key: "Escape"});
    expect(screen.queryByRole("listbox")).toBeNull();
    fireEvent.click(screen.getByRole("button", {name: "Add people"}));
    expect(save).toHaveBeenCalledWith(["29", "0"]);
  });
  it("reveals more choices without losing selection", () => {
    render(<Picker />); fireEvent.focus(screen.getByRole("combobox", {name: "People"}));
    fireEvent.click(screen.getByRole("option", {name: "Person 00"}));
    fireEvent.click(screen.getByRole("button", {name: "Show more"}));
    expect(screen.getAllByRole("option")).toHaveLength(30);
    expect(screen.getByRole("option", {name: "Person 00"}).getAttribute("aria-selected")).toBe("true");
  });
  it("opens on the first item from the keyboard and commits once", () => {
    const save = vi.fn(); render(<Picker save={save} />);
    const input = screen.getByRole("combobox", {name: "People"});
    fireEvent.keyDown(input, {key: "ArrowDown"}); fireEvent.keyDown(input, {key: "Enter"});
    fireEvent.keyDown(input, {key: "ArrowDown"}); fireEvent.keyDown(input, {key: "Enter"});
    fireEvent.keyDown(input, {key: "Escape"});
    fireEvent.click(screen.getByRole("button", {name: "Add people"}));
    expect(save).toHaveBeenCalledOnce(); expect(save).toHaveBeenCalledWith(["0", "1"]);
  });
  it("does not offer choices or submit while disabled", () => {
    const save = vi.fn(); render(<Picker disabled save={save} />);
    fireEvent.focus(screen.getByRole("combobox", {name: "People"}));
    expect(screen.queryByRole("listbox")).toBeNull();
    fireEvent.click(screen.getByRole("button", {name: "Add people"}));
    expect(save).not.toHaveBeenCalled();
  });
});
