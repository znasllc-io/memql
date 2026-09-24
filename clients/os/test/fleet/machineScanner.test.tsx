import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { HardwareGroup } = await import("../../src/apps/fleet/machines/HardwareGroup");
const { ModelsGroup } = await import("../../src/apps/fleet/machines/ModelsGroup");
const { machineFromRow } = await import("../../src/apps/fleet/rows");
const { fakeConnection, machineRow, withSession } = await import("./harness");

// The scanner's three groups on the machine page (epic memql#5146).
//
// ===========================================================================
// WHAT IS ASSERTED HERE
// ===========================================================================
// One idea, three times: ABSENCE IS AS LEGIBLE AS PRESENCE, and the three
// kinds of absence must not look alike. A machine that has not reported, a
// machine that reported and is under the floor, and a model nobody has probed
// lead to opposite actions -- do nothing, buy hardware, press a button -- and
// the generic rendering collapses all three into a blank or a zero.
//
// Everything below is a test for one of those collapses.

afterEach(cleanup);

const OWNER = "v1:identity:user:me";
const NOW = new Date("2026-09-07T12:00:00Z");

const M4_MAX = {
  chip: "Apple M4 Max",
  memoryBytes: 64 * 1024 ** 3,
  gpu: { name: "Apple M4 Max", vramBytes: 0, backend: "metal" },
  cpuCores: 16,
  osVersion: "15.3",
  diskFreeBytes: 900 * 1024 ** 3,
  runtimes: [{ name: "ollama", version: "0.5.4" }],
  reportedAt: "2026-09-07T11:58:00Z",
};

function machine(over: Partial<Row> = {}) {
  return machineFromRow(machineRow({ id: "laptop", ownerUserId: OWNER, ...over }));
}

async function settle() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  });
}

// --- Hardware ---------------------------------------------------------------

describe("the Hardware group", () => {
  it("says the cockpit has not reported, and says nothing is wrong", () => {
    // THE STATE EVERY MACHINE IS IN ON THE DAY THIS SHIPS. The reader's
    // question is whether something is wrong with their machine, and the
    // answer is no -- a blank or a dash answers nothing and sends them
    // looking.
    render(
      withSession(
        <HardwareGroup machine={machine()} machineClass="" usableBytes={0} now={NOW} />,
        { userId: OWNER },
      ),
    );
    expect(screen.getByText(/has not reported what hardware it has/i)).toBeTruthy();
    expect(screen.getByText(/working normally/i)).toBeTruthy();
    // And it does NOT accuse the machine of being unsupported.
    expect(screen.queryByText(/under the floor/i)).toBeNull();
  });

  it("leads with the class and shows the figure behind it", () => {
    // The class is the ONE DERIVED value here, so it leads -- and it carries
    // the usable figure, because a class with no working shown is a verdict.
    render(
      withSession(
        <HardwareGroup
          machine={machine({ hardware: M4_MAX })}
          machineClass="32"
          usableBytes={48 * 1024 ** 3}
          now={NOW}
        />,
        { userId: OWNER },
      ),
    );
    expect(screen.getByText(/A 32 GB machine/)).toBeTruthy();
    expect(screen.getByText(/48\.0 GiB usable/)).toBeTruthy();
  });

  it("says no accelerator the runtimes can reach, never no GPU", () => {
    // The machine may have a card whose driver does not work, and that repair
    // lives on the machine. "No GPU" would send somebody shopping.
    render(
      withSession(
        <HardwareGroup
          machine={machine({
            hardware: { ...M4_MAX, gpu: { name: "", vramBytes: 0, backend: "none" } },
          })}
          machineClass="32"
          usableBytes={48 * 1024 ** 3}
          now={NOW}
        />,
        { userId: OWNER },
      ),
    );
    expect(screen.getByText(/No accelerator the runtimes can reach/)).toBeTruthy();
    expect(screen.queryByText(/no GPU/i)).toBeNull();
  });

  it("does not read a unified machine's zero VRAM as a card with no memory", () => {
    // Reading that zero as "no accelerator" would describe every Apple Silicon
    // machine -- the single most common machine this feature is for -- as
    // having nothing.
    render(
      withSession(
        <HardwareGroup
          machine={machine({ hardware: M4_MAX })}
          machineClass="32"
          usableBytes={48 * 1024 ** 3}
          now={NOW}
        />,
        { userId: OWNER },
      ),
    );
    expect(screen.getByText(/sharing 64\.0 GiB of unified memory/)).toBeTruthy();
  });

  it("says the floor when the machine reported and is under it", () => {
    // The OTHER absence, and it is an accusation the previous test's machine
    // must never receive. The facts stay on screen so a person can see the
    // figure it was judged on.
    render(
      withSession(
        <HardwareGroup
          machine={machine({
            hardware: { ...M4_MAX, memoryBytes: 8 * 1024 ** 3, chip: "old laptop" },
          })}
          machineClass="unsupported"
          usableBytes={6 * 1024 ** 3}
          now={NOW}
        />,
        { userId: OWNER },
      ),
    );
    expect(screen.getByText(/Under the floor for local models/)).toBeTruthy();
    expect(screen.getByText(/Apple Silicon with 16 GB/)).toBeTruthy();
    expect(screen.getByText("old laptop")).toBeTruthy();
  });

  it("lists a runtime with its version", () => {
    render(
      withSession(
        <HardwareGroup
          machine={machine({ hardware: M4_MAX })}
          machineClass="32"
          usableBytes={48 * 1024 ** 3}
          now={NOW}
        />,
        { userId: OWNER },
      ),
    );
    expect(screen.getByText("Ollama")).toBeTruthy();
    expect(screen.getByText("0.5.4")).toBeTruthy();
  });
});

// --- Sharing ----------------------------------------------------------------
//
// The Sharing group's tests moved to test/fleet/sharing.test.tsx when the
// two-way toggle became a panel and a share dialog (epic memql#5344): both
// consents always drawn with their own repair, the ledger only while the
// machine serves somebody else and a notice when it could not be read, and
// the act offered to the owner alone. Each of those promises is kept there.

// --- Models: the recommended set and the measured figures --------------------

const RECOMMENDED = {
  workerId: "laptop",
  machineClass: "32",
  usableBytes: 48 * 1024 ** 3,
  reported: true,
  entries: [
    {
      modelId: "qwen3.5:9b",
      level: "fast",
      runtime: "ollama",
      category: "text",
      params: 9_000_000_000,
      size: 5_400_000_000,
      notes: "the balanced pick",
      blocked: "",
      pullable: true,
    },
    {
      modelId: "kokoro:1b",
      level: "strong",
      runtime: "kokoro",
      category: "audioOut",
      params: 1_000_000_000,
      size: 900_000_000,
      notes: "",
      blocked: "Needs the Kokoro runtime, which this machine has not reported.",
      pullable: false,
    },
  ],
  runtimeGap: ["kokoro"],
};

async function mountModels(opts: { recommended?: Row[]; measurements?: Row[] } = {}) {
  const connection = fakeConnection({
    modelPullsForWorker: [],
    fleetRecommended: opts.recommended ?? [],
    measurementsForMachine: opts.measurements ?? [],
    fleetSharingLedger: [],
  });
  h.connection = connection;
  render(
    withSession(
      <ModelsGroup
        machine={machine({ labels: { "model:qwen3.5:9b": "ctx=128000,structured=1" } })}
      />,
      { userId: OWNER },
    ),
  );
  await settle();
  fireEvent.click(screen.getByRole("button", { name: /^qwen3.5:9b/ }));
  return connection;
}

describe("the Models group's recommended set", () => {
  it("counts unique downloads while preserving each recommendation level", async () => {
    const chat = { ...RECOMMENDED.entries[0], modelId: "qwen3.8:27b" };
    await mountModels({ recommended: [{
      ...RECOMMENDED,
      entries: [
        ...["fast", "strong", "reasoning"].map((level) => ({ ...chat, level })),
        { ...chat, modelId: "qwen3-embedding:0.6b", level: "embeddings", category: "embedding" },
      ],
    } as unknown as Row] });
    expect(screen.getByText(/2 models, pulled one after another/)).toBeTruthy();
    expect(screen.getAllByText("qwen3.8:27b")).toHaveLength(3);
    expect(screen.getByText("qwen3-embedding:0.6b")).toBeTruthy();
  });

  it("shows the set with one act, and keeps a blocked entry with its reason", async () => {
    // A BLOCKED ENTRY STAYS ON SCREEN. Dropping it answers "why can my machine
    // not do voice" with silence, which is indistinguishable from a catalog
    // that never had one.
    await mountModels({ recommended: [RECOMMENDED as unknown as Row] });
    expect(screen.getByRole("button", { name: "Pull recommended set" })).toBeTruthy();
    expect(screen.getByText(/Needs the Kokoro runtime/)).toBeTruthy();
    expect(screen.getByText(/Installing the Kokoro runtime/)).toBeTruthy();
  });

  it("shows nothing at all when the machine has not reported its hardware", async () => {
    // Not an empty set with every profile blocked -- NOTHING. The Hardware
    // group above already says the cockpit has not spoken, and rule 7 is that
    // a thing is said once.
    await mountModels({
      recommended: [{ ...RECOMMENDED, reported: false, entries: [] } as unknown as Row],
    });
    expect(screen.queryByRole("button", { name: "Pull recommended set" })).toBeNull();
    expect(screen.queryByText(/the catalog recommends/)).toBeNull();
  });

  it("counts only the pullable half in the act's sentence", async () => {
    // The button pulls what it can and reports the rest; saying "2 models"
    // when one of them is blocked would promise work the act will not do.
    await mountModels({ recommended: [RECOMMENDED as unknown as Row] });
    expect(screen.getByText(/One model, pulled in the background/)).toBeTruthy();
  });
});

describe("the Models group's measured figures", () => {
  it("says a model is not measured rather than showing zeroes", async () => {
    // THE COLLAPSE THIS WHOLE EPIC EXISTS TO PREVENT. "0%" and "0 tok/s" read
    // exactly like a model that failed every case, and the two lead to
    // opposite actions.
    await mountModels({ recommended: [RECOMMENDED as unknown as Row] });
    expect(screen.getByText(/Not measured on this machine yet/)).toBeTruthy();
    expect(screen.queryByText("0%")).toBeNull();
    expect(screen.queryByText(/0 tok\/s/)).toBeNull();
  });

  it("renders a MEASURED ZERO as a zero, because it is a measurement", async () => {
    // The other half of the pair. Five cases that ran and all failed is a real
    // and useful 0%, and it must not read as a model nobody probed.
    await mountModels({
      recommended: [RECOMMENDED as unknown as Row],
      measurements: [
        {
          machineId: "laptop",
          modelId: "qwen3.5:9b",
          suiteVersion: "1",
          measuredAt: "2026-09-07T10:00:00Z",
          structuredValidity: { measured: true, median: 0, n: 5 },
          toolCallCorrectness: { measured: true, median: 0.67, n: 3 },
          throughputTps: { measured: true, median: 42 },
          ttftMs: { measured: false, absentReason: "unmeasured" },
          probeError: "",
        } as unknown as Row,
      ],
    });
    expect(screen.getByText("0%")).toBeTruthy();
    expect(screen.getByText("67%")).toBeTruthy();
    expect(screen.getByText("42")).toBeTruthy();
    expect(screen.queryByText(/Not measured on this machine yet/)).toBeNull();
  });

  it("reads the measured FLAG and never a median beside a false one", async () => {
    // The kit's figureFrom reads KEY PRESENCE, which on this shape returns a
    // measured 12.4 for a figure that says it is not one. This asserts the
    // adapter reads the flag first.
    await mountModels({
      recommended: [RECOMMENDED as unknown as Row],
      measurements: [
        {
          machineId: "laptop",
          modelId: "qwen3.5:9b",
          suiteVersion: "1",
          measuredAt: "2026-09-07T10:00:00Z",
          structuredValidity: { measured: false, median: 12.4, absentReason: "failed" },
          toolCallCorrectness: { measured: false, absentReason: "unmeasured" },
          throughputTps: { measured: false, absentReason: "unmeasured" },
          ttftMs: { measured: false, absentReason: "unmeasured" },
          probeError: "",
        } as unknown as Row,
      ],
    });
    expect(screen.queryByText(/1240%/)).toBeNull();
    expect(screen.queryByText("12.4")).toBeNull();
  });

  it("offers Probe this model to the owner", async () => {
    await mountModels({ recommended: [RECOMMENDED as unknown as Row] });
    expect(screen.getByRole("button", { name: "Probe this model" })).toBeTruthy();
  });
});
