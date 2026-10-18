import { describe, expect, it } from "vitest";

const { machineModelsFrom, machineRuntimesFrom, pullFromRow, pullProgressFraction, pullStepLabel } =
  await import("../../src/apps/fleet/machines/models");

// ===========================================================================
// WHAT A MACHINE OFFERS COMES FROM ITS LABELS, AND NOTHING ELSE
// ===========================================================================
// The cockpit advertises `model:<id>` for every model it will serve, with a
// flat `k=v` value carrying the attributes. That set IS the machine's offer:
// the cockpit only advertises what its own `models.allow` permits, so there
// is no "blocked" state hiding anywhere for this surface to render.

describe("a machine's models, from its labels", () => {
  it("reads the attributes off each model label", () => {
    const models = machineModelsFrom({
      "model:llama3.1:8b": "ctx=131072,structured=1,tools=1,vision=1,params=8000000000,quant=Q4_K_M,max=2",
      "runtime:ollama": "1",
      "os": "darwin",
    });
    expect(models).toHaveLength(1);
    expect(models[0]).toMatchObject({
      modelId: "llama3.1:8b",
      params: 8_000_000_000,
      quant: "Q4_K_M",
      contextWindow: 131072,
      structuredOutput: true,
      tools: true,
      embeddings: false,
      vision: true,
      imageGen: false,
      audioIn: false,
      audioOut: false,
      maxConcurrent: 2,
    });
  });

  // A MODEL ID CONTAINS COLONS, which is the whole reason this is not a split.
  // `model:hf.co/owner/repo:Q4_K_M` has three, and taking the segment after
  // the first would leave a model nothing can be pulled or routed under.
  it("keeps every colon in the model id", () => {
    const models = machineModelsFrom({
      "model:hf.co/TheBloke/Llama-2-70B-GGUF:Q4_K_M": "ctx=4096",
    });
    expect(models[0]?.modelId).toBe("hf.co/TheBloke/Llama-2-70B-GGUF:Q4_K_M");
  });

  // ABSENT IS "THE MACHINE DID NOT SAY", not false and not zero. Every
  // capability defaults off because a model that never claimed structured
  // output must not be routed a structured prompt; every NUMBER defaults to
  // zero and is rendered as "not reported" rather than as a small value.
  it("defaults every unstated attribute to the fail-closed value", () => {
    const models = machineModelsFrom({ "model:mystery": "" });
    expect(models[0]).toMatchObject({
      params: 0,
      quant: "",
      contextWindow: 0,
      structuredOutput: false,
      tools: false,
      embeddings: false,
      vision: false,
      imageGen: false,
      audioIn: false,
      audioOut: false,
    });
  });

  it("sorts by model id, so the same machine reads the same way twice", () => {
    const models = machineModelsFrom({
      "model:zephyr:7b": "",
      "model:llama3.1:8b": "",
      "model:mistral:7b": "",
    });
    expect(models.map((m) => m.modelId)).toEqual(["llama3.1:8b", "mistral:7b", "zephyr:7b"]);
  });

  it("ignores a label with the prefix and no id", () => {
    expect(machineModelsFrom({ "model:": "ctx=4096" })).toHaveLength(0);
  });

  // The runtime is its own question, and worth answering separately: "a
  // runtime is installed and serving nothing" and "no runtime at all" are
  // different states with different fixes, and a models list alone collapses
  // them into one empty list.
  it("reads the runtimes separately from the models", () => {
    const labels = { "runtime:ollama": "1", "model:llama3.1:8b": "ctx=8192" };
    expect(machineRuntimesFrom(labels)).toEqual(["ollama"]);
    expect(machineModelsFrom(labels)).toHaveLength(1);
  });
});

// ===========================================================================
// A PULL'S PROGRESS IS PER LAYER, AND THE SURFACE MUST NOT PRETEND OTHERWISE
// ===========================================================================

describe("a pull in flight", () => {
  const row = {
    id: "pull-1",
    workerId: "reg-1",
    model: "llama3.1:70b",
    status: "running",
    statusLine: "pulling 8eeb52dfb3bb",
    layer: "sha256:8eeb52dfb3bb",
    completedBytes: 4_200_000_000,
    totalBytes: 39_000_000_000,
    readvertised: false,
    errorMessage: "",
    requestedAt: "2026-09-07T12:00:00Z",
    updatedAt: "2026-09-07T12:04:00Z",
    endedAt: "",
  };

  it("projects the row it will render", () => {
    const pull = pullFromRow(row);
    expect(pull).toMatchObject({
      pullId: "pull-1",
      model: "llama3.1:70b",
      status: "running",
      statusLine: "pulling 8eeb52dfb3bb",
      completedBytes: 4_200_000_000,
      totalBytes: 39_000_000_000,
    });
  });

  it("gives a fraction of the CURRENT STEP when the runtime stated its size", () => {
    expect(pullProgressFraction(pullFromRow(row))).toBeCloseTo(4.2 / 39, 3);
  });

  // ===========================================================================
  // AN UNSTATED TOTAL IS NULL, NEVER ZERO
  // ===========================================================================
  // Returning 0 would paint an empty bar, which claims the download has moved
  // nothing -- when what actually happened is that the runtime did not say how
  // big this step is. A bar is a claim about a denominator; with no
  // denominator there is no bar to draw, and the caller renders the status
  // line alone.
  it("has no fraction at all when the runtime did not state a total", () => {
    expect(pullProgressFraction(pullFromRow({ ...row, totalBytes: 0 }))).toBeNull();
  });

  it("has no fraction for a pull that is not running", () => {
    expect(pullProgressFraction(pullFromRow({ ...row, status: "succeeded" }))).toBeNull();
  });

  // Clamped, because the two counters come from different observations of a
  // moving target and a runtime that overshoots its own stated total would
  // otherwise draw a bar past the end of its track.
  it("clamps a fraction that overshoots", () => {
    const over = pullFromRow({ ...row, completedBytes: 50_000_000_000 });
    expect(pullProgressFraction(over)).toBe(1);
  });

  // ===========================================================================
  // THE STEP LABEL SAYS "IN THIS STEP", ALWAYS
  // ===========================================================================
  // Without those words the numbers read as the whole download, and the first
  // layer boundary makes them jump backwards -- which reads as a bug in the
  // page rather than as the next blob starting.
  it("names the numbers as belonging to the step", () => {
    const label = pullStepLabel(pullFromRow(row));
    expect(label).toContain("in this step");
    expect(label).toContain("3.9 GiB"); // completed
    expect(label).toContain("36.3 GiB"); // total -- 39e9 bytes over 1024^3
  });

  it("reports only what was fetched when the size is unknown", () => {
    const label = pullStepLabel(pullFromRow({ ...row, totalBytes: 0 }));
    expect(label).toContain("3.9 GiB");
    expect(label).toContain("in this step");
    // No "of <total>", because there is no total to be of.
    expect(label).not.toContain(" of ");
  });

  it("says nothing at all before the first byte of a step", () => {
    expect(pullStepLabel(pullFromRow({ ...row, completedBytes: 0, totalBytes: 0 }))).toBe("");
  });
});
