import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { ModelsGroup } = await import("../../src/apps/fleet/machines/ModelsGroup");
const { machineFromRow } = await import("../../src/apps/fleet/rows");
const { MODEL_PULL_CONCEPT } = await import("../../src/apps/fleet/machines/useModelPulls");
const { fakeConnection, machineRow, modelPullRow, withSession } = await import("./harness");

// The Models group on a machine's detail (epic memql#5103, design D5).
//
// WHAT IS ASSERTED HERE and not in models.test.ts: the group is where a
// projection meets an owner check, a live feed and an act. Each of those is a
// seam a correct projection can still fall through -- the classic being a Pull
// button rendered for somebody the engine will refuse.

afterEach(cleanup);

const OWNER = "v1:identity:user:me";

async function mount(
  labels: Record<string, string>,
  opts: { owner?: string; viewer?: string; pulls?: Row[]; setup?: Row } = {},
) {
  const connection = fakeConnection({ modelPullsForWorker: opts.pulls ?? [], fleetModelSetup: [opts.setup ?? { id:"laptop",revision:"",choices:{},usableBytes:48e9,diskFreeBytes:100e9,catalog:[{modelId:"hf.co/TheBloke/Llama-2-70B-GGUF:Q4_K_M",runtime:"ollama",purposes:["conversation"],memoryBytes:40e9,sizeBytes:39e9,installed:false,blocked:"",installBlocked:"",recommended:true}] }] });
  h.connection = connection;
  const machine = machineFromRow(
    machineRow({ id: "laptop", ownerUserId: opts.owner ?? OWNER, labels }),
  );
  render(withSession(<ModelsGroup machine={machine} />, { userId: opts.viewer ?? OWNER }));
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
  return connection;
}

describe("the Models group", () => {
  it.each(["qwen3.5:9b","qwen3.8:27b","qwen3.8:27b-q8_0"])("keeps installed model details informational: %s",async modelId=>{
    const connection = await mount({["model:"+modelId]:"tools=1,ctx=32768","model:embed":"embeddings=1"});
    expect(screen.queryByRole("button",{name:"Ask it something"})).toBeNull();
    fireEvent.click(screen.getByRole("button",{name:new RegExp(modelId.replaceAll(".","\\."))}));
    expect(screen.getByRole("region", {name:"Overview"})).toBeTruthy();
    expect(screen.queryByRole("region", {name:"Test"})).toBeNull();
    expect(screen.queryByRole("button",{name:"Ask it something"})).toBeNull();
    expect(screen.queryByRole("button",{name:"Measure performance"})).toBeNull();
    expect(connection.query.measurementsForMachine).not.toHaveBeenCalled();
    expect(connection.query.fleetModelProbe).not.toHaveBeenCalled();
  });
  it("never offers a conversation check to image models",async()=>{
    await mount({"model:flux":"imagegen=1"});
    fireEvent.click(screen.getByRole("button",{name:/flux/}));
    expect(screen.queryByRole("button",{name:"Ask it something"})).toBeNull();
  });
  it("keeps detailed model facts off the overview",async()=>{
    await mount({"model:chat":"params=8000000000,quant=Q4_K_M,ctx=131072,structured=1"});
    fireEvent.click(screen.getByRole("button",{name:/chat Conversation/}));
    expect(screen.getByText("8B")).toBeTruthy();
    expect(screen.getByText("Q4_K_M")).toBeTruthy();
    expect(screen.getByText("128k")).toBeTruthy();
  });
  it("distinguishes a missing runtime from a runtime waiting for models",async()=>{
    await mount({}); expect(screen.getByText("No model runtime reported yet.")).toBeTruthy(); cleanup();
    await mount({"runtime:ollama":"1"}); expect(screen.getByText("No models installed yet.")).toBeTruthy();
  });
  it("keeps catalog and setup changes with the machine's owner",async()=>{
    await mount({"model:chat":"tools=1"},{owner:"alice"});
    expect(screen.queryByRole("button",{name:"Change model setup"})).toBeNull();
    expect(screen.queryByRole("button",{name:"Browse catalog"})).toBeNull();
  });
  it("installs an eligible catalog model with its exact runtime id",async()=>{
    const connection=await mount({"runtime:ollama":"1"});
    fireEvent.click(screen.getByRole("button",{name:"Browse catalog"}));
    fireEvent.click(screen.getByRole("button",{name:/hf.co/}));
    await act(async()=>{screen.getByRole("button",{name:"Install model"}).click();});
    expect(connection.query.fleetModelPull).toHaveBeenCalledWith({registrationId:"laptop",model:"hf.co/TheBloke/Llama-2-70B-GGUF:Q4_K_M"});
  });
  it("shows a failed installation without changing the setup",async()=>{
    const connection=await mount({"runtime:ollama":"1"});
    connection.query.fleetModelPull.mockRejectedValueOnce(new Error("Waiting for refreshed storage details"));
    fireEvent.click(screen.getByRole("button",{name:"Browse catalog"}));
    fireEvent.click(screen.getByRole("button",{name:/hf.co/}));
    await act(async()=>{screen.getByRole("button",{name:"Install model"}).click();});
    expect(screen.getByText("Waiting for refreshed storage details")).toBeTruthy();
    expect(connection.query.fleetSetModelSetup).not.toHaveBeenCalled();
  });
});

describe("a pull in flight", () => {
  it("renders the runtime's own status line and a bar for the current step", async () => {
    await mount(
      { "runtime:ollama": "1" },
      {
        pulls: [
          modelPullRow({
            id: "pull-1",
            model: "llama3.1:70b",
            status: "running",
            statusLine: "pulling 8eeb52dfb3bb",
            completedBytes: 4_200_000_000,
            totalBytes: 39_000_000_000,
          }),
        ],
      },
    );
    expect(screen.getByText("pulling 8eeb52dfb3bb")).toBeTruthy();
    const bar = screen.getByRole("progressbar", { name: /Current step of llama3.1:70b/ });
    expect(bar.getAttribute("aria-valuenow")).toBe("11");
    expect(screen.getByText(/in this step/)).toBeTruthy();
  });

  // ===========================================================================
  // NO TOTAL MEANS NO BAR, NOT AN EMPTY ONE
  // ===========================================================================
  // An empty track says "this download has moved nothing". What happened is
  // that the runtime did not say how big the step is, and painting a 0% bar
  // asserts a denominator nobody supplied.
  it("draws no bar at all when the runtime stated no size", async () => {
    await mount(
      { "runtime:ollama": "1" },
      {
        pulls: [
          modelPullRow({
            id: "pull-1",
            status: "running",
            statusLine: "verifying sha256 digest",
            completedBytes: 0,
            totalBytes: 0,
          }),
        ],
      },
    );
    expect(screen.getByText("verifying sha256 digest")).toBeTruthy();
    expect(screen.queryByRole("progressbar")).toBeNull();
  });

  // The act is ABSENT while a pull runs, for the reason it is absent for a
  // non-owner: a greyed button beside a live bar invites the click it exists
  // to prevent.
  it("withdraws the Pull act while one is already running", async () => {
    await mount(
      { "runtime:ollama": "1" },
      { pulls: [modelPullRow({ id: "pull-1", status: "running" })] },
    );
    expect(screen.queryByRole("button", { name: "Pull" })).toBeNull();
  });

  // ===========================================================================
  // THE FEED IS LIVE, AND THIS IS THE HALF A SEED-ONLY TEST CANNOT SEE
  // ===========================================================================
  // A pull's whole content is that it moves. If the fold never reached this
  // surface the first render would be right and every render after it frozen,
  // which is the failure that looks most like success.
  it("follows the pull as the cluster updates it", async () => {
    const connection = await mount(
      { "runtime:ollama": "1" },
      {
        pulls: [
          modelPullRow({ id: "pull-1", status: "running", statusLine: "pulling manifest" }),
        ],
      },
    );
    expect(screen.getByText("pulling manifest")).toBeTruthy();

    await act(async () => {
      connection.subscriptions.emit(
        MODEL_PULL_CONCEPT,
        modelPullRow({
          id: "pull-1",
          status: "running",
          statusLine: "pulling 8eeb52dfb3bb",
          completedBytes: 1_000,
          totalBytes: 4_000,
        }),
      );
      await Promise.resolve();
    });
    expect(screen.getByText("pulling 8eeb52dfb3bb")).toBeTruthy();
    expect(screen.getByRole("progressbar").getAttribute("aria-valuenow")).toBe("25");
  });

  // ===========================================================================
  // THE ROW'S workerId IS CANONICAL AND THE MACHINE'S id IS BARE
  // ===========================================================================
  it("matches the bare worker id returned on the wire", async () => {
    await mount(
      { "runtime:ollama": "1" },
      {
        pulls: [
          modelPullRow({
            id: "pull-1",
            workerId: "laptop",
            status: "running",
            statusLine: "pulling manifest",
          }),
        ],
      },
    );
    expect(screen.getByText("pulling manifest")).toBeTruthy();
  });

  // A pull for a DIFFERENT machine arrives on this feed too -- the
  // subscription is by concept, not by machine -- and must not appear under
  // the machine being looked at.
  it("ignores a pull that belongs to another machine", async () => {
    const connection = await mount({ "runtime:ollama": "1" });
    await act(async () => {
      connection.subscriptions.emit(
        MODEL_PULL_CONCEPT,
        modelPullRow({
          id: "pull-elsewhere",
          workerId: "desktop",
          status: "running",
          statusLine: "pulling on somebody else's machine",
        }),
      );
      await Promise.resolve();
    });
    expect(screen.queryByText("pulling on somebody else's machine")).toBeNull();
    // And the act stays available, because nothing is running HERE.
    expect(screen.getByRole("button", { name: "Browse catalog" })).toBeTruthy();
  });
});

describe("what has been pulled here before", () => {
  // A FAILURE IS THE POINT OF THE HISTORY. A succeeded pull is already visible
  // as a model; a failed one leaves nothing behind at all, so without this the
  // machine's models simply do not change and nothing says why.
  it("carries a failure's own words", async () => {
    await mount(
      { "runtime:ollama": "1" },
      {
        pulls: [
          modelPullRow({
            id: "pull-1",
            status: "failed",
            errorMessage: "write /root/.ollama: no space left on device",
            endedAt: "2026-09-07T12:30:00Z",
          }),
        ],
      },
    );
    expect(screen.getByText(/no space left on device/)).toBeTruthy();
  });

  // `readvertised` earns its own sentence: a model on disk the cluster cannot
  // see yet is neither a usable success nor a failure, and reporting it as
  // plain success sends somebody looking for a model that is not in the
  // catalog.
  it("says when a pulled model is not visible to the cluster yet", async () => {
    await mount(
      { "runtime:ollama": "1" },
      {
        pulls: [
          modelPullRow({
            id: "pull-1",
            status: "succeeded",
            readvertised: false,
            endedAt: "2026-09-07T12:30:00Z",
          }),
        ],
      },
    );
    expect(screen.getByText(/sees it when this machine next reconnects/)).toBeTruthy();
  });

  it("says plainly when it is routable now", async () => {
    await mount(
      { "runtime:ollama": "1" },
      {
        pulls: [
          modelPullRow({
            id: "pull-1",
            status: "succeeded",
            readvertised: true,
            endedAt: "2026-09-07T12:30:00Z",
          }),
        ],
      },
    );
    expect(screen.getByText(/can route to it now/)).toBeTruthy();
  });
});

const setupFixture = {
 id: "laptop", revision: "before", choices: { conversation: "chat", tools: "chat" }, usableBytes: 24e9, diskFreeBytes: 100e9,
 catalog: [
  { modelId: "chat", runtime: "ollama", installed: true, purposes: ["conversation", "tools"], memoryBytes: 16e9 },
  { modelId: "small", runtime: "ollama", installed: true, purposes: ["conversation", "tools"], memoryBytes: 6e9 },
  { modelId: "large", runtime: "ollama", installed: true, purposes: ["conversation"], memoryBytes: 20e9 },
  { modelId: "unknown", runtime: "ollama", installed: true, purposes: ["conversation"], memoryBytes: 0, blocked: "Memory estimate unavailable" },
 ],
};
async function chooseTask(label: string, model: string) {
 fireEvent.click(screen.getByRole("button", { name: `Select ${model}` }));
 fireEvent.click(screen.getByRole("button", { name: new RegExp(`^${label}:`) }));
}
describe("model setup choices", () => {
 it("saves the expected revision and deduplicates a model serving several tasks", async () => {
  const connection = await mount({"model:chat":"tools=1", "model:small":"tools=1"}, {setup: setupFixture});
  expect(screen.getByRole("meter").getAttribute("aria-valuenow")).toBe("16000000000");
  fireEvent.click(screen.getByRole("button", {name:"Change model setup"}));
  await chooseTask("Conversation", "small");
  expect(screen.getByRole("meter").getAttribute("aria-valuenow")).toBe("22000000000");
  await waitFor(() => expect((screen.getByRole("button", {name:"Save setup"}) as HTMLButtonElement).disabled).toBe(false));
  await act(async () => { await new Promise(resolve => setTimeout(resolve, 450)); });
  await act(async () => { fireEvent.click(screen.getByRole("button", {name:"Save setup"})); });
  expect(connection.query.fleetSetModelSetup).toHaveBeenCalledWith({registrationId:"laptop", revision:"before", choices:{conversation:"small", tools:"chat"}});
 });
 it("prevents an over-capacity save and excludes unknown memory profiles", async () => {
  const connection = await mount({"model:chat":"tools=1", "model:large":"ctx=32768"}, {setup:setupFixture});
  fireEvent.click(screen.getByRole("button", {name:"Change model setup"}));
  fireEvent.click(screen.getByRole("button", { name: "Select unknown" }));
  expect(screen.queryByRole("button", { name: "Conversation: chat" })).toBeNull();
  expect(screen.getAllByText("Memory estimate unavailable").length).toBeGreaterThan(0);
  fireEvent.click(screen.getByRole("button", { name: "Select large" }));
  expect(screen.queryByRole("button", { name: "Conversation: chat" })).toBeNull();
  fireEvent.click(screen.getByRole("group", { name: "Conversation: chat" }));
  expect(screen.queryByText(/This would exceed capacity/)).toBeNull();
  expect(screen.queryByRole("button", {name:"Save setup"})).toBeNull();
  expect(connection.query.fleetSetModelSetup).not.toHaveBeenCalled();
 });
 it("preserves choices after a concurrent-save refusal", async () => {
  const connection = await mount({"model:chat":"tools=1", "model:small":"tools=1"}, {setup:setupFixture});
  connection.query.fleetSetModelSetup.mockRejectedValueOnce(new Error("The setup changed. Reload it before saving."));
  fireEvent.click(screen.getByRole("button", {name:"Change model setup"}));
  await chooseTask("Conversation", "small");
  await waitFor(() => expect((screen.getByRole("button", {name:"Save setup"}) as HTMLButtonElement).disabled).toBe(false));
  await act(async () => { await new Promise(resolve => setTimeout(resolve, 450)); });
  await act(async () => { fireEvent.click(screen.getByRole("button", {name:"Save setup"})); });
  expect(screen.getByRole("button", {name:"Conversation: small"})).toBeTruthy();
  expect(screen.getByText("The setup changed. Reload it before saving.")).toBeTruthy();
 });
 it("retains a refused draft when the recovery read also fails", async () => {
  const connection = await mount({"model:chat":"tools=1", "model:small":"tools=1"}, {setup:setupFixture});
  fireEvent.click(screen.getByRole("button", {name:"Change model setup"}));
  await chooseTask("Conversation", "small");
  connection.query.fleetSetModelSetup.mockRejectedValueOnce(new Error("Connection interrupted"));
  connection.query.fleetModelSetup.mockRejectedValueOnce(new Error("Still disconnected"));
  await act(async () => { fireEvent.click(screen.getByRole("button", {name:"Save setup"})); });
  expect(screen.getByRole("button", {name:"Conversation: small"})).toBeTruthy();
  expect(screen.queryByRole("button", {name:"Save setup"})).toBeNull();
  expect(screen.getByRole("button", {name:"Discard changes"})).toBeTruthy();
 });
 it("holds choices and prevents a second write while saving", async () => {
  const connection = await mount({"model:chat":"tools=1", "model:small":"tools=1"}, {setup:setupFixture});
  let finish!: () => void;
  connection.query.fleetSetModelSetup.mockImplementationOnce(() => new Promise<void>(resolve => { finish = resolve; }));
  fireEvent.click(screen.getByRole("button", {name:"Change model setup"}));
  await chooseTask("Conversation", "small");
  fireEvent.click(screen.getByRole("button", {name:"Save setup"}));
  fireEvent.click(screen.getByRole("button", {name:"Saving setup…"}));
  expect(connection.query.fleetSetModelSetup).toHaveBeenCalledTimes(1);
  expect((screen.getByRole("button", {name:"Discard changes"}) as HTMLButtonElement).disabled).toBe(true);
  expect((screen.getByRole("button", {name:"Select chat"}) as HTMLButtonElement).disabled).toBe(true);
  await act(async () => finish());
  expect(screen.getByText("Model setup saved.")).toBeTruthy();
 });
});
