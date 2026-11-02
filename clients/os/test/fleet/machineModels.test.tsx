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
const { builtinReply, fakeConnection, machineRow, modelPullRow, withSession } = await import("./harness");

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
  opts: { owner?: string; viewer?: string; pulls?: Row[]; setup?: Row; revokedAt?: string } = {},
) {
  const connection = fakeConnection({ modelPullsForWorker: opts.pulls ?? [], fleetModelSetup: [opts.setup ?? { id:"laptop",revision:"",choices:{},usableBytes:48e9,diskFreeBytes:100e9,catalog:[{modelId:"hf.co/TheBloke/Llama-2-70B-GGUF:Q4_K_M",runtime:"ollama",purposes:["conversation"],memoryBytes:40e9,sizeBytes:39e9,installed:false,blocked:"",installBlocked:"",recommended:true}] }] });
  h.connection = connection;
  const machine = machineFromRow(
    machineRow({ id: "laptop", ownerUserId: opts.owner ?? OWNER, labels, ...(opts.revokedAt ? { revokedAt: opts.revokedAt } : {}) }),
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
 it("estimates an Automatic setup from the models MemQL picks for each task", async () => {
  const automatic = { ...setupFixture, revision: "", choices: {}, suggestedChoices: { conversation: "chat", tools: "chat", embeddings: "embed" },
   catalog: [...setupFixture.catalog, { modelId: "embed", runtime: "ollama", installed: true, purposes: ["embeddings"], memoryBytes: 1e9 }] };
  await mount({"model:chat":"tools=1", "model:embed":"embeddings=1"}, {setup: automatic});
  const meter = screen.getByRole("meter", { name: "Planned model memory" });
  expect(meter.getAttribute("aria-valuenow")).toBe("17000000000");
  expect(meter.getAttribute("aria-valuetext")).toMatch(/automatic/i);
  expect(meter.querySelectorAll("span")).toHaveLength(2);
  expect(screen.getByText("MemQL chooses automatically")).toBeTruthy();
 });
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
  expect(screen.getByText("Model setup saved.")).toBeTruthy();
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
  connection.query.fleetSetModelSetup.mockImplementationOnce(() => new Promise(resolve => { finish = () => resolve(builtinReply("fleetSetModelSetup", [{ id: "laptop", revision: "after" }])); }));
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
 // A REPLY IS NOT A RECEIPT. The engine answers a save with one row carrying the
 // revision it wrote, so a reply without one says nothing about the machine's row.
 it.each<[string, Row[]]>([
  ["no row", []],
  ["a row with no revision", [{ id: "laptop" }]],
  ["a row with an empty revision", [{ id: "laptop", revision: "" }]],
  ["a revision that is not a string", [{ id: "laptop", revision: 7 }]],
 ])("does not report a save nobody confirmed: %s", async (_reply, rows) => {
  const connection = await mount({"model:chat":"tools=1", "model:small":"tools=1"}, {setup:setupFixture});
  connection.query.fleetSetModelSetup.mockResolvedValueOnce(builtinReply("fleetSetModelSetup", rows));
  fireEvent.click(screen.getByRole("button", {name:"Change model setup"}));
  await chooseTask("Conversation", "small");
  await act(async () => { fireEvent.click(screen.getByRole("button", {name:"Save setup"})); });
  expect(screen.getByText(/did not confirm/)).toBeTruthy();
  expect(screen.queryByText("Model setup saved.")).toBeNull();
  expect(screen.getByRole("button", {name:"Conversation: small"})).toBeTruthy();
 });
 it("saves the same draft again after a reply nobody confirmed", async () => {
  const connection = await mount({"model:chat":"tools=1", "model:small":"tools=1"}, {setup:setupFixture});
  connection.query.fleetSetModelSetup.mockResolvedValueOnce(builtinReply("fleetSetModelSetup", []));
  fireEvent.click(screen.getByRole("button", {name:"Change model setup"}));
  await chooseTask("Conversation", "small");
  await act(async () => { fireEvent.click(screen.getByRole("button", {name:"Save setup"})); });
  await act(async () => { fireEvent.click(screen.getByRole("button", {name:"Save setup"})); });
  expect(connection.query.fleetSetModelSetup).toHaveBeenCalledTimes(2);
  expect(connection.query.fleetSetModelSetup).toHaveBeenLastCalledWith({registrationId:"laptop", revision:"before", choices:{conversation:"small", tools:"chat"}});
  expect(screen.getByText("Model setup saved.")).toBeTruthy();
  expect(screen.queryByText(/did not confirm/)).toBeNull();
 });
});

// THE WAY BACK FROM A SAVED SETUP. Saving always mints a revision, so "Use
// suggested" with the same picks still leaves a saved setup and its 32K ceiling.
describe("returning a saved setup to automatic", () => {
 const settle = () => act(async () => { await new Promise(resolve => setTimeout(resolve, 450)); });
 const automatic = { ...setupFixture, revision: "", choices: {}, suggestedChoices: { conversation: "chat", tools: "chat" } };
 const labels = {"model:chat":"tools=1", "model:small":"tools=1"};
 // The next read answers with this setup, and a graph event is what asks for it.
 async function rereadWith(connection: Awaited<ReturnType<typeof mount>>, setup: Row) {
  connection.query.fleetModelSetup.mockResolvedValue(builtinReply("fleetModelSetup", [setup]));
  await act(async () => { connection.subscriptions.emit("v1:worker:registration", machineRow({ id: "laptop", ownerUserId: OWNER, labels }), "NODE_UPDATED"); await Promise.resolve(); });
 }
 async function ask() {
  const connection = await mount(labels, {setup: setupFixture});
  fireEvent.click(screen.getByRole("button", {name:"Change model setup"}));
  fireEvent.click(screen.getByRole("button", {name:"Return to automatic"}));
  return connection;
 }
 it("says what it does before doing it, then sends the revision it read", async () => {
  const connection = await ask();
  const question = screen.getByRole("group", {name:/^Returning .* to automatic$/});
  expect(question.textContent).toMatch(/every model this machine offers/);
  expect(question.textContent).toMatch(/32K/);
  expect(question.textContent).toMatch(/one at a time/);
  expect(question.textContent).toMatch(/saved choices/);
  expect(connection.query.fleetClearModelSetup).not.toHaveBeenCalled();
  connection.query.fleetModelSetup.mockResolvedValue(builtinReply("fleetModelSetup", [automatic]));
  await settle();
  await act(async () => { fireEvent.click(screen.getByRole("button", {name:"Return to automatic"})); });
  expect(connection.query.fleetClearModelSetup).toHaveBeenCalledWith({registrationId:"laptop", revision:"before"});
  expect(connection.query.fleetSetModelSetup).not.toHaveBeenCalled();
  await waitFor(() => expect(screen.getByText("MemQL chooses automatically")).toBeTruthy());
  expect(screen.getByText("Returned to automatic.").getAttribute("role")).toBe("status");
  expect(screen.queryByText("Your choices")).toBeNull();
 });
 it("is absent from a machine already on automatic", async () => {
  await mount(labels, {setup: automatic});
  fireEvent.click(screen.getByRole("button", {name:"Change model setup"}));
  expect(screen.getByRole("button", {name:"Discard changes"})).toBeTruthy();
  expect(screen.queryByRole("button", {name:"Return to automatic"})).toBeNull();
 });
 it("is not a save: Use suggested only fills the draft", async () => {
  const connection = await mount(labels, {setup: {...setupFixture, suggestedChoices: {conversation:"small"}}});
  fireEvent.click(screen.getByRole("button", {name:"Change model setup"}));
  fireEvent.click(screen.getByRole("button", {name:"Use suggested"}));
  expect(connection.query.fleetClearModelSetup).not.toHaveBeenCalled();
  expect(screen.getByRole("button", {name:"Return to automatic"})).toBeTruthy();
 });
 it("takes no press from the second click of the double click that asked", async () => {
  const connection = await ask();
  fireEvent.click(screen.getByRole("button", {name:"Return to automatic"}));
  expect(connection.query.fleetClearModelSetup).not.toHaveBeenCalled();
 });
 it("moves focus to the question and back to the act when kept, writing nothing", async () => {
  const connection = await ask();
  const question = screen.getByRole("group", {name:/^Returning .* to automatic$/});
  expect(document.activeElement).toBe(question);
  await settle();
  fireEvent.click(screen.getByRole("button", {name:"Keep my choices"}));
  expect(screen.queryByRole("group", {name:/^Returning .* to automatic$/})).toBeNull();
  expect(document.activeElement).toBe(screen.getByRole("button", {name:"Return to automatic"}));
  expect(connection.query.fleetClearModelSetup).not.toHaveBeenCalled();
 });
 it("mentions unsaved changes only when there are some, and keeps them after a refusal", async () => {
  const connection = await mount(labels, {setup: setupFixture});
  fireEvent.click(screen.getByRole("button", {name:"Change model setup"}));
  await chooseTask("Conversation", "small");
  fireEvent.click(screen.getByRole("button", {name:"Return to automatic"}));
  expect(screen.getByRole("group", {name:/^Returning .* to automatic$/}).textContent).toMatch(/changes here/);
  connection.query.fleetClearModelSetup.mockRejectedValueOnce(new Error("Connection interrupted"));
  await settle();
  await act(async () => { fireEvent.click(screen.getByRole("button", {name:"Return to automatic"})); });
  expect(screen.getByText("Connection interrupted")).toBeTruthy();
  // The question stays open beside its refusal, and closing it loses nothing.
  await settle();
  fireEvent.click(screen.getByRole("button", {name:"Keep my choices"}));
  expect(screen.getByRole("button", {name:"Conversation: small"})).toBeTruthy();
  expect(screen.queryByText("Returned to automatic.")).toBeNull();
 });
 it("withdraws the act when a refusal shows the setup changed elsewhere", async () => {
  const connection = await ask();
  connection.query.fleetClearModelSetup.mockRejectedValueOnce(new Error("The model setup changed. Reload it and try again."));
  connection.query.fleetModelSetup.mockResolvedValue(builtinReply("fleetModelSetup", [{ ...setupFixture, revision: "after" }]));
  await settle();
  await act(async () => { fireEvent.click(screen.getByRole("button", {name:"Return to automatic"})); });
  await waitFor(() => expect(screen.getByText("This setup changed elsewhere.")).toBeTruthy());
  expect(screen.getByText("The model setup changed. Reload it and try again.")).toBeTruthy();
  expect(screen.queryByRole("button", {name:"Return to automatic"})).toBeNull();
  expect(screen.queryByRole("group", {name:/^Returning .* to automatic$/})).toBeNull();
  // The act that asked is gone, so focus goes to the way out rather than the page.
  expect(document.activeElement).toBe(screen.getByRole("button", {name:"Done"}));
 });
 it("does not reopen the question, or take focus, when a failed read recovers", async () => {
  const connection = await ask();
  connection.query.fleetClearModelSetup.mockRejectedValueOnce(new Error("Connection interrupted"));
  connection.query.fleetModelSetup.mockRejectedValueOnce(new Error("Still disconnected"));
  await settle();
  await act(async () => { fireEvent.click(screen.getByRole("button", {name:"Return to automatic"})); });
  await waitFor(() => expect(screen.queryByRole("group", {name:/^Returning .* to automatic$/})).toBeNull());
  // The person has moved on by the time the read recovers.
  const elsewhere = screen.getByRole("button", {name:"Select small"});
  elsewhere.focus();
  await rereadWith(connection, setupFixture);
  await waitFor(() => expect(screen.getByRole("button", {name:"Return to automatic"})).toBeTruthy());
  expect(screen.queryByRole("group", {name:/^Returning .* to automatic$/})).toBeNull();
  expect(document.activeElement).toBe(elsewhere);
 });
 it("offers no change to a machine that has been removed", async () => {
  await mount(labels, {setup: setupFixture, revokedAt: "2026-10-01T00:00:00Z"});
  expect(screen.getByText("Your choices")).toBeTruthy();
  expect(screen.queryByRole("button", {name:"Change model setup"})).toBeNull();
 });
 it("says it returned only once the page shows it, and not after a later save", async () => {
  const connection = await ask();
  await settle();
  // The write landed, and the page's read has not caught up yet.
  await act(async () => { fireEvent.click(screen.getByRole("button", {name:"Return to automatic"})); });
  expect(screen.getByText("Your choices")).toBeTruthy();
  expect(screen.queryByText("Returned to automatic.")).toBeNull();
  await rereadWith(connection, automatic);
  await waitFor(() => expect(screen.getByText("Returned to automatic.")).toBeTruthy());
  expect(screen.getByText("MemQL chooses automatically")).toBeTruthy();
  await rereadWith(connection, { ...setupFixture, revision: "saved-elsewhere" });
  await waitFor(() => expect(screen.getByText("Your choices")).toBeTruthy());
  expect(screen.queryByText("Returned to automatic.")).toBeNull();
 });
 it("still reports the return on a machine with nothing installed", async () => {
  const connection = await mount({"runtime:ollama":"1"}, {setup: setupFixture});
  fireEvent.click(screen.getByRole("button", {name:"Change model setup"}));
  fireEvent.click(screen.getByRole("button", {name:"Return to automatic"}));
  connection.query.fleetModelSetup.mockResolvedValue(builtinReply("fleetModelSetup", [automatic]));
  await settle();
  await act(async () => { fireEvent.click(screen.getByRole("button", {name:"Return to automatic"})); });
  await waitFor(() => expect(screen.getByText("Returned to automatic.")).toBeTruthy());
 });
 it("shows a refusal as its sentence, without the engine's frames", async () => {
  const connection = await ask();
  connection.query.fleetClearModelSetup.mockRejectedValueOnce(new Error("fleetClearModelSetup: MemQL engine failed to execute query. Details: The model setup changed. Reload it and try again."));
  await settle();
  await act(async () => { fireEvent.click(screen.getByRole("button", {name:"Return to automatic"})); });
  expect(screen.getByText("The model setup changed. Reload it and try again.")).toBeTruthy();
  expect(screen.queryByText(/MemQL engine failed/)).toBeNull();
 });
 it("shows a refused save without the engine's frames too", async () => {
  const connection = await mount(labels, {setup:setupFixture});
  connection.query.fleetSetModelSetup.mockRejectedValueOnce(new Error("fleetSetModelSetup: MemQL engine failed to execute query. Details: The model setup changed. Reload it and try again."));
  fireEvent.click(screen.getByRole("button", {name:"Change model setup"}));
  await chooseTask("Conversation", "small");
  await settle();
  await act(async () => { fireEvent.click(screen.getByRole("button", {name:"Save setup"})); });
  expect(screen.getByText("The model setup changed. Reload it and try again.")).toBeTruthy();
 });
 it("does not report a return nobody confirmed", async () => {
  const connection = await ask();
  connection.query.fleetClearModelSetup.mockResolvedValueOnce(builtinReply("fleetClearModelSetup", []));
  await settle();
  await act(async () => { fireEvent.click(screen.getByRole("button", {name:"Return to automatic"})); });
  expect(screen.getByText(/did not confirm/)).toBeTruthy();
  expect(screen.queryByText("Returned to automatic.")).toBeNull();
  expect(screen.getByRole("group", {name:/^Returning .* to automatic$/})).toBeTruthy();
 });
 it("holds its answers and prevents a second write while returning", async () => {
  const connection = await ask();
  let finish!: () => void;
  connection.query.fleetClearModelSetup.mockImplementationOnce(() => new Promise(resolve => { finish = () => resolve(builtinReply("fleetClearModelSetup", [{ revision: "", automatic: true }])); }));
  await settle();
  fireEvent.click(screen.getByRole("button", {name:"Return to automatic"}));
  fireEvent.click(screen.getByRole("button", {name:"Returning…"}));
  expect(connection.query.fleetClearModelSetup).toHaveBeenCalledTimes(1);
  expect((screen.getByRole("button", {name:"Keep my choices"}) as HTMLButtonElement).disabled).toBe(true);
  expect((screen.getByRole("button", {name:"Select chat"}) as HTMLButtonElement).disabled).toBe(true);
  connection.query.fleetModelSetup.mockResolvedValue(builtinReply("fleetModelSetup", [automatic]));
  await act(async () => finish());
  await waitFor(() => expect(screen.getByText("Returned to automatic.")).toBeTruthy());
 });
});
