import { describe, expect, it } from "vitest";

import { barFor, checksFor, checksSettled, stopsFor, type Draft, type FlowFacts } from "../../src/apps/fleet/addMachine/flow";
import { setupDownloadLabel, setupFrom, setupNow, setupWaitingAnswer, SETUP_QUIET_MS } from "../../src/apps/fleet/machines/setup";
import { machineFromRow, type MachineRow } from "../../src/apps/fleet/rows";
import { machineRow } from "./harness";

// WHAT THE INSTALLER IS DOING, READ FROM ITS OWN ACCOUNT (memql#6229).
//
// The install command connects the machine first and then spends minutes on
// the model runtime, the models and the workbenches. These are the states the
// add-machine wizard used to misread as "nothing here yet": Download models
// for a download already running, and "needs attention" over a stage the
// installer had not reached.

const NOW = new Date("2026-10-18T15:30:00Z");
const ago = (seconds: number) => new Date(NOW.getTime() - seconds * 1000).toISOString();
const GiB = 1024 ** 3;

const DRAFT: Draft = { name: "znass-mac", platform: "mac", computerUse: false, inference: true, workbench: true };

type Stage = { name: string; state: string; detail?: string; model?: string; completedBytes?: number; totalBytes?: number; modelsDone?: number; modelsTotal?: number };

function setup(state: string, stages: Stage[], reportedAt = ago(5)) {
  return { state, stages, updatedAt: reportedAt, reportedAt };
}

function machine(over: Record<string, unknown> = {}): MachineRow {
  return machineFromRow(
    machineRow({
      id: "v1:worker:registration:m1",
      identityId: "v1:identity:identity:tok-1",
      capabilities: ["HEADLESS"],
      lastSeenAt: ago(3),
      ...over,
    }),
  );
}

const WITH_RUNTIME = { hardware: { chip: "M2 Max", memoryBytes: 32 * GiB, runtimes: [{ name: "ollama", version: "0.40.6" }] } };

const DOWNLOADING = setup("running", [
  { name: "runtime", state: "done" },
  { name: "models", state: "running", model: "qwen3.8:27b", completedBytes: 4 * GiB, totalBytes: 18 * GiB, modelsDone: 0, modelsTotal: 2 },
  { name: "workbench", state: "waiting" },
]);

function facts(m: MachineRow): FlowFacts {
  return {
    draft: DRAFT, connected: true, minting: false, mintError: "",
    mint: { token: "tok", identityId: "v1:identity:identity:tok-1" }, mintedAt: null,
    machine: m, beats: 2, cancelAsked: false, revokeError: "", revoking: false, now: NOW,
  };
}

function find(checks: ReturnType<typeof checksFor>, id: string) {
  return checks.find((c) => c.id === id);
}

describe("reading the installer's report", () => {
  it("keeps only what it can say, and reads a quiet running report as stopped", () => {
    expect(setupFrom(undefined)).toBeNull();
    expect(setupFrom({ state: "installing", stages: [] })).toBeNull();
    const parsed = setupFrom({
      state: "running",
      reportedAt: ago(5),
      stages: [
        { name: "models", state: "running", model: "qwen3.8:27b", totalBytes: "18" },
        { name: "menu", state: "running" },
        { name: "models", state: "failed" },
      ],
    });
    expect(parsed?.stages).toHaveLength(1);
    expect(parsed?.stages[0]).toMatchObject({ name: "models", state: "running", totalBytes: 18 });
    expect(setupNow(parsed, NOW)?.state).toBe("running");
    expect(setupNow({ ...parsed!, reportedAt: ago(SETUP_QUIET_MS / 1000 + 1) }, NOW)?.state).toBe("stopped");
  });

  it("says what a waiting stage is after, and the download as the whole model", () => {
    const report = setupFrom(DOWNLOADING)!;
    expect(setupWaitingAnswer(report, "workbench")).toBe("After the models");
    expect(setupDownloadLabel(report.stages[1]!)).toBe("Model 1 of 2 · 4.0 GiB of 18.0 GiB");
  });
});

describe("the checks while the installer works", () => {
  it("shows the installer's download instead of offering a second one, and holds workbenches after it", () => {
    const m = machine({ ...WITH_RUNTIME, setup: DOWNLOADING });
    const checks = checksFor(DRAFT, m, 2, NOW);
    const models = find(checks, "models")!;
    expect(models).toMatchObject({ state: "current", answer: "Downloading qwen3.8:27b" });
    expect(models.act).toBeUndefined();
    expect(models.download?.totalBytes).toBe(18 * GiB);
    expect(find(checks, "workbench")).toMatchObject({ state: "ahead", answer: "After the models" });
    expect(find(checks, "workbench")?.command).toBeUndefined();
    expect(find(checks, "response")?.state).toBe("ahead");

    const bar = barFor(facts(m), checks);
    expect(bar).toMatchObject({ state: "Downloading models", tone: "busy" });
    expect(bar.detail).toBe("qwen3.8:27b · Model 1 of 2 · 4.0 GiB of 18.0 GiB");
    // Back is the only act: no Download models for a download in progress.
    expect(bar.acts.map((a) => a.label)).toEqual(["Back"]);
    expect(stopsFor(facts(m), checks).find((s) => s.id === "checks")?.answer).toMatch(/· Downloading models$/);
  });

  it("says the runtime is being installed, with the models after it", () => {
    const m = machine({
      setup: setup("running", [
        { name: "runtime", state: "running" },
        { name: "models", state: "waiting" },
        { name: "workbench", state: "waiting" },
      ]),
    });
    const checks = checksFor(DRAFT, m, 2, NOW);
    expect(find(checks, "runtime")).toMatchObject({ state: "current", answer: "Installing" });
    expect(find(checks, "runtime")?.command).toBeUndefined();
    expect(find(checks, "models")).toMatchObject({ state: "ahead", answer: "After the model runtime" });
    // Each stage waits on the one just before it, not on the first.
    expect(find(checks, "workbench")).toMatchObject({ state: "ahead", answer: "After the models" });
    expect(barFor(facts(m), checks).state).toBe("Installing the model runtime");
  });

  it("says workbenches are being set up while the installer does it", () => {
    const m = machine({
      ...WITH_RUNTIME,
      labels: { "model:qwen3.8:27b": "tools=1" },
      setup: setup("running", [
        { name: "runtime", state: "done" },
        { name: "models", state: "done" },
        { name: "workbench", state: "running" },
      ]),
    });
    const checks = checksFor(DRAFT, m, 2, NOW);
    expect(find(checks, "workbench")).toMatchObject({ state: "current", answer: "Setting up" });
    expect(barFor(facts(m), checks).state).toBe("Setting up workbenches");
  });

  it("keeps a permission the person must grant on its line without taking over the floor", () => {
    const m = machine({ ...WITH_RUNTIME, setup: DOWNLOADING, buildTag: "computeruse", capabilities: ["HEADLESS", "COMPUTERUSE"] });
    const draft = { ...DRAFT, computerUse: true };
    const checks = checksFor(draft, m, 2, NOW);
    expect(find(checks, "permissions")?.state).not.toBe("done");
    expect(barFor({ ...facts(m), draft }, checks).state).toBe("Downloading models");
  });
});

describe("the checks once the installer has spoken", () => {
  it("asks the person for workbenches only when the installer says it could not, in its words", () => {
    const m = machine({
      ...WITH_RUNTIME,
      labels: { "model:qwen3.8:27b": "tools=1" },
      setup: setup("finished", [
        { name: "runtime", state: "done" },
        { name: "models", state: "done" },
        { name: "workbench", state: "failed", detail: "Start Docker with Linux containers, then retry: Docker is not running" },
      ]),
    });
    const checks = checksFor(DRAFT, m, 2, NOW, { live: null }, { state: "passed" });
    expect(find(checks, "workbench")).toMatchObject({
      state: "open",
      repair: "Start Docker with Linux containers, then retry: Docker is not running",
    });
    expect(find(checks, "workbench")?.command).toContain("worker setup --workbench");
    expect(barFor(facts(m), checks).state).toBe("Fleet workbenches needs attention");
  });

  it("offers Download models after an installer that stopped mid-download, and says it resumes", () => {
    const stopped = { ...DOWNLOADING, state: "running", reportedAt: ago(SETUP_QUIET_MS / 1000 + 30) };
    const m = machine({ ...WITH_RUNTIME, setup: stopped });
    const checks = checksFor(DRAFT, m, 2, NOW);
    expect(find(checks, "models")).toMatchObject({ state: "open", answer: "Download stopped", act: "pullRecommended" });
    expect(find(checks, "workbench")).toMatchObject({ state: "open", answer: "Not set up" });
    expect(barFor(facts(m), checks).acts.map((a) => a.label)).toEqual(["Back", "Download models"]);
  });

  it("settles a machine the hardware floor rules out instead of waiting on models forever", () => {
    const m = machine({
      capabilities: ["HEADLESS", "WORKBENCH"],
      capabilityDescriptor: { actionContracts: { workerWorkbench: 1 } },
      setup: setup("finished", [
        { name: "runtime", state: "skipped", detail: "This machine has 8 GB of unified memory; the floor is 16 GB." },
        { name: "models", state: "skipped", detail: "Not started: the model runtime is not ready." },
        { name: "workbench", state: "done" },
      ]),
    });
    const checks = checksFor(DRAFT, m, 2, NOW);
    expect(find(checks, "runtime")).toMatchObject({ state: "skipped", repair: "This machine has 8 GB of unified memory; the floor is 16 GB." });
    expect(find(checks, "models")?.state).toBe("skipped");
    expect(find(checks, "response")?.state).toBe("skipped");
    expect(checksSettled(checks.filter((c) => c.id !== "workbench"))).toBe(true);
  });

  it("is unchanged for a machine whose cockpit reports no install", () => {
    const checks = checksFor(DRAFT, machine(WITH_RUNTIME), 2, NOW);
    expect(find(checks, "models")).toMatchObject({ state: "open", answer: "None yet", act: "pullRecommended" });
    expect(find(checks, "workbench")).toMatchObject({ state: "open", answer: "Not available yet" });
  });
});

describe("one set of words for every surface", () => {
  it("summarises the install for a machine's page while it runs, and says nothing once it is done", async () => {
    const { setupSummary } = await import("../../src/apps/fleet/machines/setup");
    expect(setupSummary(setupFrom(DOWNLOADING))).toBe("Downloading qwen3.8:27b · Model 1 of 2 · 4.0 GiB of 18.0 GiB");
    expect(setupSummary(setupFrom(setup("running", [{ name: "workbench", state: "running" }])))).toBe("Setting up workbenches");
    expect(setupSummary(setupNow(setupFrom({ ...DOWNLOADING, reportedAt: ago(SETUP_QUIET_MS / 1000 + 1) }), NOW))).toBe("Stopped before it finished");
    expect(setupSummary(setupFrom(setup("finished", [{ name: "models", state: "done" }])))).toBe("");
    expect(setupSummary(null)).toBe("");
  });
});
