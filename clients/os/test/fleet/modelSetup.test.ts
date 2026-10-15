import { describe, expect, it } from "vitest";
import { safeModelSource, setupCapacity, type SetupModel } from "../../src/apps/fleet/machines/modelSetup";
import { canConverse, machineModelsFrom, modelPurposeLabel } from "../../src/apps/fleet/machines/models";

const model = (modelId: string, memoryBytes: number, purposes: string[]): SetupModel => ({ modelId, memoryBytes, purposes, installed: true, blocked: "", installBlocked: "", category: "text", runtime: "ollama", sizeBytes: 10, contextWindow: 32768, recommended: false, source: "", notes: "" });
describe("machine model setup", () => {
  it("counts a shared model once, while keeping independent model costs", () => {
    const catalog = [model("chat",30,["conversation","tools"]),model("embed",4,["embeddings"])];
    expect(setupCapacity({ conversation:"chat",tools:"chat",embeddings:"embed" },catalog,48)).toMatchObject({used:34,fits:true});
    expect(setupCapacity({ conversation:"chat",embeddings:"embed" },catalog,32).fits).toBe(false);
  });
  it("does not allow an unknown, missing, incompatible or uninstalled model", () => {
    expect(setupCapacity({conversation:"gone"},[],48).fits).toBe(false);
    expect(setupCapacity({conversation:"unknown"},[model("unknown",0,["conversation"])],48).fits).toBe(false);
    expect(setupCapacity({vision:"chat"},[model("chat",4,["conversation"])],48).fits).toBe(false);
    expect(setupCapacity({conversation:"chat"},[{...model("chat",4,["conversation"]),installed:false}],48).fits).toBe(false);
  });
  it("does not offer the conversation test to image, audio or embedding models", () => {
    const models = machineModelsFrom({"model:image":"imagegen=1","model:voice":"audioout=1","model:embed":"embeddings=1","model:chat":"vision=1,tools=1"});
    expect(models.filter(canConverse).map(m=>m.modelId)).toEqual(["chat"]);
    expect(modelPurposeLabel(models.find(m=>m.modelId==="image")!)).toBe("Image generation");
  });
  it("keeps untrusted catalog links out of executable URL schemes", () => {
    expect(safeModelSource("javascript:alert(1)")).toBeUndefined();
    expect(safeModelSource("https://ollama.com/library/model")).toBe("https://ollama.com/library/model");
  });
});
