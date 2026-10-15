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
  it("refuses incompatible, oversized and unverified choices without changing assignments", () => {
    const { changes } = mount();
    select("small"); fireEvent.click(task("Search & memory")); expect(screen.getByText("This model does not support that task.")).toBeTruthy();
    select("large"); fireEvent.click(task("Conversation")); expect(screen.getByText(/would exceed capacity/)).toBeTruthy();
    select("unknown"); fireEvent.click(task("Conversation")); expect(screen.getByText("This model's memory use has not been verified.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Select missing" })).toBeNull();
    expect(changes).not.toHaveBeenCalled(); expect(memory()).toBe(22);
  });
  it("keeps a bound embedding model through manual, drag and suggested changes", () => {
    const { changes } = mount({ boundEmbeddingModel: "embed", suggestedChoices: { conversation: "small", embeddings: "new-embed" } });
    expect(screen.queryByRole("button", { name: "Clear Search & memory" })).toBeNull();
    select("new-embed"); fireEvent.click(task("Search & memory")); expect(changes).not.toHaveBeenCalled();
    const dataTransfer = transfer(); fireEvent.dragStart(screen.getByRole("button", { name: "Select new-embed" }), { dataTransfer });
    fireEvent.drop(task("Search & memory"), { dataTransfer }); expect(changes).not.toHaveBeenCalled();
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
    fireEvent.click(task("Conversation")); expect(screen.getByRole("status").textContent).toContain("Select an installed model first");
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
