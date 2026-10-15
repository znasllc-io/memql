import { useState } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ModelAssignmentBoard } from "../../src/apps/fleet/machines/ModelAssignmentBoard";
import { modelSetupFromRow, type ModelChoices, type ModelSetupView } from "../../src/apps/fleet/machines/modelSetup";

afterEach(cleanup);
const GiB = 2 ** 30;
const view = modelSetupFromRow({ id: "machine", revision: "one", usableBytes: 32 * GiB,
  choices: { conversation: "chat", tools: "chat", embeddings: "embed" },
  suggestedChoices: { conversation: "small", tools: "small", embeddings: "embed" },
  catalog: [
    { modelId: "chat", installed: true, memoryBytes: 20 * GiB, purposes: ["conversation", "tools"] },
    { modelId: "small", installed: true, memoryBytes: 6 * GiB, purposes: ["conversation", "tools"] },
    { modelId: "embed", installed: true, memoryBytes: 2 * GiB, purposes: ["embeddings"] },
    { modelId: "new-embed", installed: true, memoryBytes: 3 * GiB, purposes: ["embeddings"] },
    { modelId: "large", installed: true, memoryBytes: 28 * GiB, purposes: ["conversation"] },
    { modelId: "unknown", installed: true, memoryBytes: 0, purposes: ["conversation"] },
    { modelId: "blocked", installed: true, memoryBytes: 2 * GiB, purposes: ["conversation"], blocked: "Runtime unavailable" },
    { modelId: "missing", installed: false, memoryBytes: 2 * GiB, purposes: ["conversation"] },
  ],
});
function mount(overrides: Partial<ModelSetupView> = {}, saving = false) {
  const data = { ...view, ...overrides };
  const changes = vi.fn(), save = vi.fn(), discard = vi.fn();
  function Editor() {
    const [choices, setChoices] = useState<ModelChoices>(data.choices);
    return <ModelAssignmentBoard view={data} choices={choices} saving={saving} canSave onChange={next => { changes(next); setChoices(next); }} onSave={save} onDiscard={discard} />;
  }
  render(<Editor />);
  return { changes, save, discard };
}
const select = (name: string) => fireEvent.click(screen.getByRole("button", { name: `Select ${name}` }));
const task = (name: string) => screen.getByRole("button", { name: new RegExp(`^${name}:`) });
const readOnlyTask = (name: string) => screen.getByRole("group", { name: new RegExp(`^${name}:`) });
const memory = () => Number(screen.getByRole("meter").getAttribute("aria-valuenow")) / GiB;
function transfer() {
  const values = new Map<string, string>();
  return { types: ["text/plain"], setData: (type: string, value: string) => values.set(type, value), getData: (type: string) => values.get(type) ?? "", effectAllowed: "", dropEffect: "" };
}
describe("model assignment board", () => {
  it("assigns by selecting a model and task, counting each shared model only once", () => {
    const { changes } = mount();
    select("small"); fireEvent.click(task("Conversation"));
    expect(memory()).toBe(28);
    expect(changes).toHaveBeenLastCalledWith({ conversation: "small", tools: "chat", embeddings: "embed" });
    fireEvent.click(screen.getByRole("button", { name: "Use for 2 matching tasks" }));
    expect(memory()).toBe(8);
    expect(task("Tools & agents").textContent).toContain("small");
  });
  it("previews memory without changing the draft and restores it after leaving", () => {
    const { changes } = mount(); select("small");
    fireEvent.mouseEnter(task("Conversation")); expect(memory()).toBe(28);
    expect(changes).not.toHaveBeenCalled();
    fireEvent.mouseLeave(task("Conversation")); expect(memory()).toBe(22);
  });
  it("uses the same proposal for drag and drop, rejecting unrelated external drops", () => {
    const { changes } = mount(); const dataTransfer = transfer();
    dataTransfer.setData(dataTransfer.types[0]!, "small");
    fireEvent.drop(task("Conversation"), { dataTransfer }); expect(changes).not.toHaveBeenCalled();
    fireEvent.dragStart(screen.getByRole("button", { name: "Select small" }), { dataTransfer });
    fireEvent.dragOver(task("Conversation"), { dataTransfer }); expect(memory()).toBe(28);
    fireEvent.drop(task("Conversation"), { dataTransfer });
    expect(changes).toHaveBeenLastCalledWith({ conversation: "small", tools: "chat", embeddings: "embed" });
    expect(document.activeElement).toBe(task("Conversation"));
  });
  it("keeps incompatible tasks readable without offering a click or keyboard action", () => {
    const { changes } = mount();
    select("small");
    expect(screen.queryByRole("button", { name: /^Search & memory:/ })).toBeNull();
    const target = readOnlyTask("Search & memory");
    expect(target.textContent).toContain("embed");
    expect(target.tabIndex).toBe(-1);
    fireEvent.click(target); fireEvent.keyDown(target, { key: "Enter" });
    expect(changes).not.toHaveBeenCalled();
    expect(screen.queryByText("This model does not support that task.")).toBeNull();
    select("embed"); expect(task("Search & memory")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Conversation:/ })).toBeNull();
    select("small"); expect(task("Conversation")).toBeTruthy();
    // Removing an existing assignment remains a separate, legal action.
    fireEvent.click(screen.getByRole("button", { name: "Clear Search & memory" }));
    expect(changes).toHaveBeenLastCalledWith({ conversation: "chat", tools: "chat", embeddings: "" });
    expect(document.activeElement).toBe(readOnlyTask("Search & memory"));
  });
  it("ignores incompatible drops without a warning or memory preview", () => {
    const { changes } = mount(); const dataTransfer = transfer();
    fireEvent.dragStart(screen.getByRole("button", { name: "Select small" }), { dataTransfer });
    fireEvent.dragOver(readOnlyTask("Search & memory"), { dataTransfer });
    expect(dataTransfer.dropEffect).toBe("none"); expect(memory()).toBe(22);
    fireEvent.drop(readOnlyTask("Search & memory"), { dataTransfer });
    expect(changes).not.toHaveBeenCalled();
    expect(screen.queryByText("This model does not support that task.")).toBeNull();
  });
  it("offers no assignment action for oversized, blocked and unverified choices", () => {
    const { changes } = mount();
    for (const model of ["large", "unknown", "blocked"]) {
      select(model);
      expect(screen.queryByRole("button", { name: /^Conversation:/ })).toBeNull();
      fireEvent.click(readOnlyTask("Conversation"));
    }
    expect(screen.queryByRole("button", { name: "Select missing" })).toBeNull();
    expect(changes).not.toHaveBeenCalled(); expect(memory()).toBe(22);
  });
  it("offers a fitting batch even when individual replacements exceed capacity", () => {
    const { changes } = mount({ usableBytes: 24 * GiB });
    select("small");
    expect(screen.queryByRole("button", { name: /^Conversation:/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Use for 2 matching tasks" }));
    expect(changes).toHaveBeenLastCalledWith({ conversation: "small", tools: "small", embeddings: "embed" });
    expect(memory()).toBe(8); expect(task("Conversation")).toBeTruthy();
  });
  it("omits a batch assignment that would exceed capacity", () => {
    mount({ usableBytes: 4 * GiB, choices: { embeddings: "embed" } });
    select("small");
    expect(screen.queryByRole("button", { name: /Use for .* matching tasks/ })).toBeNull();
  });
  it("keeps a bound embedding model through manual, drag and suggested changes", () => {
    const { changes } = mount({ boundEmbeddingModel: "embed", suggestedChoices: { conversation: "small", embeddings: "new-embed" } });
    expect(screen.queryByRole("button", { name: "Clear Search & memory" })).toBeNull();
    select("new-embed"); fireEvent.click(readOnlyTask("Search & memory")); expect(changes).not.toHaveBeenCalled();
    const dataTransfer = transfer(); fireEvent.dragStart(screen.getByRole("button", { name: "Select new-embed" }), { dataTransfer });
    fireEvent.drop(readOnlyTask("Search & memory"), { dataTransfer }); expect(changes).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Use suggested" }));
    expect(changes).toHaveBeenLastCalledWith({ conversation: "small", embeddings: "embed" });
  });
  it("permits repairing unavailable saved assignments and keeps clear keyboard focus", () => {
    mount({ choices: { conversation: "removed", tools: "chat" } });
    expect(screen.queryByRole("button", { name: "Save setup" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Clear Conversation" }));
    expect(document.activeElement).toBe(task("Conversation"));
    expect(task("Conversation").textContent).toContain("Not assigned");
    expect(memory()).toBe(20);
    expect(screen.getByRole("button", { name: "Save setup" })).toBeTruthy();
  });
  it("shows an honest empty board with an exit", () => {
    const { discard } = mount({ choices: {}, catalog: [], suggestedChoices: {}, revision: "" });
    expect(screen.getByText(/No models installed. Browse/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Conversation:/ })).toBeNull();
    expect(readOnlyTask("Conversation").textContent).toContain("No model installed");
    fireEvent.click(screen.getByRole("button", { name: "Discard changes" })); expect(discard).toHaveBeenCalledOnce();
  });
  it("freezes every editing action while saving", () => {
    const { changes, save, discard } = mount({}, true);
    select("small"); fireEvent.click(task("Conversation")); fireEvent.click(screen.getByRole("button", { name: "Use suggested" }));
    fireEvent.click(screen.getByRole("button", { name: "Done" }));
    fireEvent.click(screen.getByRole("button", { name: "Saving setup…" }));
    expect(changes).not.toHaveBeenCalled(); expect(save).not.toHaveBeenCalled(); expect(discard).not.toHaveBeenCalled();
  });
});
