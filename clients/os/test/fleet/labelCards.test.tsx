import { useState } from "react";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { MapEditor } from "../../src/apps/fleet/MapEditor";
import { LabelEditor } from "../../src/apps/fleet/machines/LabelEditor";
import type { LabelMap } from "../../src/apps/fleet/labels";

const initial = { gpu: "Metal", room: "Studio", tier: "production" };
function LocalEditor({ onChange = () => {}, seed = initial }: { onChange?: (next: LabelMap) => void; seed?: LabelMap }) {
  const [value, setValue] = useState<LabelMap>(seed);
  return <MapEditor value={value} onChange={next => { onChange(next); setValue(next); }} label="Routing labels" idPrefix="test-labels" />;
}
const search = () => screen.getByRole("textbox", { name: /^Search .*labels$/ });
function find(text: string) { fireEvent.change(search(), { target: { value: text } }); }
function mode(value: string) {
  fireEvent.click(screen.getByRole("combobox", { name: /^Search .* by$/ }));
  fireEvent.click(screen.getByRole("option", { name: value }));
}
function newLabel(name: string, value: string) {
  find(name);
  fireEvent.keyDown(search(), { key: "Enter" });
  fireEvent.change(screen.getByRole("textbox", { name: `Value for ${name.trim()}` }), { target: { value } });
}

describe("routing label cards", () => {
  it("searches labels or values without offering creation from a value search", () => {
    const write = vi.fn(); render(<LocalEditor onChange={write} />);
    find("ROOM");
    expect(screen.getByRole("button", { name: "Edit room" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Edit gpu" })).toBeNull();
    mode("Value"); find("STUD");
    expect(screen.getByRole("button", { name: "Edit room" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Add label/ })).toBeNull();
    fireEvent.keyDown(search(), { key: "Enter" });
    expect(screen.queryByRole("textbox", { name: /^Value for/ })).toBeNull();
    find("nothing matches");
    expect(screen.getByRole("status").textContent).toBe("No matching labels.");
    expect(write).not.toHaveBeenCalled();
  });

  it("creates a unique named label, keeps other entries and reveals its card", async () => {
    const write = vi.fn(); render(<LocalEditor onChange={write} />);
    newLabel(" zone ", "  path=/opt/a=b  ");
    // The search is trimmed into the label name, independently of the value.
    const input = screen.getByRole("textbox", { name: "Value for zone" });
    expect(document.activeElement).toBe(input);
    fireEvent.keyDown(input, { key: "Enter" });
    await waitFor(() => expect(screen.getByRole("button", { name: "Edit zone" })).toBeTruthy());
    expect(write).toHaveBeenCalledExactlyOnceWith({ ...initial, zone: "path=/opt/a=b" });
    expect((search() as HTMLInputElement).value).toBe("");
    await waitFor(() => expect(document.activeElement).toBe(search()));
    expect(screen.getByRole("status").textContent).toBe("3–4 of 4 labels");
    find("zone");
    expect((screen.getByRole("button", { name: "Add label" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("edits the existing value instead of duplicating a label", async () => {
    const write = vi.fn(); render(<LocalEditor onChange={write} />);
    find("room"); fireEvent.keyDown(search(), { key: "Enter" });
    fireEvent.change(screen.getByRole("textbox", { name: "Value for room" }), { target: { value: "Office" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(write).toHaveBeenCalledExactlyOnceWith({ ...initial, room: "Office" }));
    expect(screen.getByRole("list", { name: "Routing labels" }).children).toHaveLength(3);
  });

  it("clamps the last page after deletion and returns focus to search", async () => {
    const write = vi.fn(); render(<LocalEditor onChange={write} />);
    fireEvent.click(screen.getByRole("button", { name: "Next labels" }));
    expect(screen.getByRole("status").textContent).toBe("3 of 3 labels");
    const remove = screen.getByRole("button", { name: "Remove tier=production" });
    remove.focus(); fireEvent.click(remove);
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("1–2 of 2 labels"));
    expect(write).toHaveBeenCalledExactlyOnceWith({ gpu: "Metal", room: "Studio" });
    await waitFor(() => expect(document.activeElement).toBe(search()));
    expect((screen.getByRole("button", { name: "Next labels" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("supports an empty value and can recreate the last deleted label", async () => {
    render(<LocalEditor seed={{}} />);
    expect(screen.queryByRole("button", { name: "Next labels" })).toBeNull();
    newLabel("available", ""); fireEvent.click(screen.getByRole("button", { name: "Add" }));
    await screen.findByRole("button", { name: "Edit available" });
    expect(screen.getByText("No value")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove available=" }));
    await screen.findByText("No routing labels.");
    newLabel("available", "yes"); fireEvent.click(screen.getByRole("button", { name: "Add" }));
    await screen.findByRole("button", { name: "Remove available=yes" });
  });

  it("keeps a refused creation for retry and rolls back the optimistic card", async () => {
    const onSave = vi.fn().mockResolvedValueOnce(false).mockResolvedValue(true);
    render(<LabelEditor operatorLabels={initial} busy={false} onSave={onSave} />);
    newLabel("zone", "West"); fireEvent.click(screen.getByRole("button", { name: "Add" }));
    await waitFor(() => expect(onSave).toHaveBeenCalledTimes(1));
    await waitFor(() => expect((screen.getByRole("button", { name: "Add" }) as HTMLButtonElement).disabled).toBe(false));
    expect((screen.getByRole("textbox", { name: "Value for zone" }) as HTMLInputElement).value).toBe("West");
    expect(screen.queryByRole("button", { name: "Edit zone" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    await screen.findByRole("button", { name: "Edit zone" });
    expect(onSave).toHaveBeenCalledTimes(2);
    expect(onSave).toHaveBeenLastCalledWith({ ...initial, zone: "West" });
  });

  it("locks writes while saving and preserves a newer broadcast on refusal", async () => {
    let finish!: (ok: boolean) => void;
    const onSave = vi.fn(() => new Promise<boolean>(resolve => { finish = resolve; }));
    const view = render(<LabelEditor operatorLabels={initial} busy={false} onSave={onSave} />);
    fireEvent.click(screen.getByRole("button", { name: "Remove room=Studio" }));
    expect((screen.getByRole("button", { name: "Remove gpu=Metal" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Remove gpu=Metal" }));
    view.rerender(<LabelEditor operatorLabels={{ gpu: "New GPU", room: "Office" }} busy={false} onSave={onSave} />);
    await act(async () => { finish(false); });
    expect(onSave).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("button", { name: "Remove room=Office" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Remove gpu=New GPU" })).toBeTruthy();
  });

  it("preserves a draft across heartbeats and refuses to overwrite a changed value", () => {
    const write = vi.fn();
    const view = render(<MapEditor value={initial} onChange={write} label="Routing labels" idPrefix="test" />);
    fireEvent.click(screen.getByRole("button", { name: "Edit room" }));
    const input = screen.getByRole("textbox", { name: "Value for room" });
    fireEvent.change(input, { target: { value: "Draft room" } });
    view.rerender(<MapEditor value={{ ...initial }} onChange={write} label="Routing labels" idPrefix="test" />);
    expect((input as HTMLInputElement).value).toBe("Draft room");
    view.rerender(<MapEditor value={{ ...initial, room: "Remote room" }} onChange={write} label="Routing labels" idPrefix="test" />);
    expect((input as HTMLInputElement).value).toBe("Draft room");
    expect(screen.getByText("This label changed elsewhere.")).toBeTruthy();
    fireEvent.keyDown(input, { key: "Enter" });
    expect(write).not.toHaveBeenCalled();
    fireEvent.keyDown(input, { key: "Escape" });
    expect(document.activeElement).toBe(search());
  });

  it("does not submit a surrounding route form when adding a predicate", async () => {
    const submit = vi.fn(event => event.preventDefault());
    render(<form onSubmit={submit}><LocalEditor /></form>);
    newLabel("zone", "East"); fireEvent.keyDown(screen.getByRole("textbox", { name: "Value for zone" }), { key: "Enter" });
    await screen.findByRole("button", { name: "Edit zone" });
    expect(submit).not.toHaveBeenCalled();
  });

  it("cannot replace a label created elsewhere while its new value is being typed", () => {
    const write = vi.fn();
    const view = render(<MapEditor value={{}} onChange={write} label="Routing labels" idPrefix="test" />);
    newLabel("zone", "West");
    view.rerender(<MapEditor value={{ zone: "East" }} onChange={write} label="Routing labels" idPrefix="test" />);
    expect((screen.getByRole("button", { name: "Add" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.keyDown(screen.getByRole("textbox", { name: "Value for zone" }), { key: "Enter" });
    expect(write).not.toHaveBeenCalled();
    expect((screen.getByRole("textbox", { name: "Value for zone" }) as HTMLInputElement).value).toBe("West");
  });
});
