import { act, cleanup, render, screen, within } from "@testing-library/react";
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
  opts: { owner?: string; viewer?: string; pulls?: Row[] } = {},
) {
  const connection = fakeConnection({ modelPullsForWorker: opts.pulls ?? [] });
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
  it.each(["qwen3.5:9b", "qwen3.8:27b", "qwen3.8:27b-q8_0"])("offers the owner a chat check for %s alongside the default embedder", async (modelId) => {
    await mount({ ["model:" + modelId]: "tools=1,ctx=32768", "model:qwen3-embedding:0.6b": "embeddings=1" });
    expect(screen.getByRole("button", { name: "Ask it something" })).toBeTruthy();
    expect(screen.getByText(new RegExp(`Sends.*to ${modelId.replaceAll(".", "\\.")}`))).toBeTruthy();
  });

  it("does not offer a targeted chat check for embeddings or someone else's machine", async () => {
    await mount({ "model:qwen3-embedding:0.6b": "embeddings=1" });
    expect(screen.queryByRole("button", { name: "Ask it something" })).toBeNull();
    cleanup();
    await mount({ "model:qwen3.8:27b": "tools=1" }, { owner: "another-owner" });
    expect(screen.queryByRole("button", { name: "Ask it something" })).toBeNull();
  });

  it("lists what the machine advertises, with its size and quantization", async () => {
    await mount({
      "runtime:ollama": "1",
      "model:llama3.1:8b": "ctx=131072,structured=1,tools=1,params=8000000000,quant=Q4_K_M",
    });
    // SCOPED TO THE ROW, not to the document. The Pull form's placeholder and
    // its Hugging Face example legitimately contain a model id and a
    // quantization, so a document-wide match would pass on the help text while
    // the row itself rendered nothing.
    const row = screen.getByRole("listitem");
    expect(within(row).getByText("llama3.1:8b")).toBeTruthy();
    expect(within(row).getByText(/8B/)).toBeTruthy();
    expect(within(row).getByText(/Q4_K_M/)).toBeTruthy();
    expect(within(row).getByText(/128k context/)).toBeTruthy();
    expect(within(row).getByText(/structured output/)).toBeTruthy();
  });

  // ===========================================================================
  // TWO EMPTY STATES, NOT ONE
  // ===========================================================================
  // "No runtime" and "a runtime serving nothing" have different fixes, and a
  // models list alone renders both as the same blank. The second is the state
  // this whole feature exists for, so it is the one that has to invite the act.
  it("distinguishes no runtime from a runtime with no models", async () => {
    await mount({});
    expect(screen.getByText(/No model runtime on this machine/)).toBeTruthy();
    cleanup();

    await mount({ "runtime:ollama": "1" });
    expect(screen.getByText(/Running ollama with no models yet/)).toBeTruthy();
  });

  // ===========================================================================
  // THE PULL ACT IS ABSENT FOR A NON-OWNER, NOT DISABLED
  // ===========================================================================
  // Rule 12's reading, and the engine enforces it independently:
  // fleetModelPull refuses a machine that is not the caller's. A disabled
  // button would advertise an act nobody on this page can reach.
  it("offers Pull to the machine's owner", async () => {
    await mount({ "runtime:ollama": "1" });
    expect(screen.getByRole("button", { name: "Pull" })).toBeTruthy();
  });

  it("shows no Pull act at all to somebody else", async () => {
    await mount({ "runtime:ollama": "1" }, { owner: "v1:identity:user:alice", viewer: OWNER });
    expect(screen.queryByRole("button", { name: "Pull" })).toBeNull();
    expect(screen.getByText(/Only this machine's owner can pull/)).toBeTruthy();
  });

  // A canonical id and a bare one name the same subject, and the shell never
  // composes ids. Comparing them naively would hide the act from the person
  // whose machine it is -- which reads exactly like a permission bug.
  it("recognises the owner across id spellings", async () => {
    await mount({ "runtime:ollama": "1" }, { owner: "v1:identity:user:me", viewer: "me" });
    expect(screen.getByRole("button", { name: "Pull" })).toBeTruthy();
  });

  it("starts a pull with the model id untouched", async () => {
    const connection = await mount({ "runtime:ollama": "1" });
    const input = screen.getByLabelText(/Model to pull onto/) as HTMLInputElement;
    await act(async () => {
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
      setter.call(input, "hf.co/TheBloke/Llama-2-70B-GGUF:Q4_K_M");
      input.dispatchEvent(new Event("input", { bubbles: true }));
    });
    await act(async () => {
      (screen.getByRole("button", { name: "Pull" }) as HTMLElement).click();
    });
    expect(connection.query.fleetModelPull).toHaveBeenCalledWith({
      registrationId: "laptop",
      model: "hf.co/TheBloke/Llama-2-70B-GGUF:Q4_K_M",
    });
  });

  it("shows the cluster's own refusal rather than a sentence of its own", async () => {
    const connection = await mount({ "runtime:ollama": "1" });
    connection.query.fleetModelPull.mockRejectedValueOnce(
      new Error("Studio is not connected to the cluster right now"),
    );
    const input = screen.getByLabelText(/Model to pull onto/) as HTMLInputElement;
    await act(async () => {
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
      setter.call(input, "llama3.1:8b");
      input.dispatchEvent(new Event("input", { bubbles: true }));
    });
    await act(async () => {
      (screen.getByRole("button", { name: "Pull" }) as HTMLElement).click();
    });
    expect(screen.getByText(/is not connected to the cluster right now/)).toBeTruthy();
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
    expect(screen.getByRole("button", { name: "Pull" })).toBeTruthy();
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
