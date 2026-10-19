import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

// What SETUP left for a person, on the machine's own page (memql#6235).
//
// Add a machine watches each step settle, and once it is closed a skipped
// permission, a failed download or an installer that stopped halfway showed
// nowhere: the machine read Online and the feature simply did not work. These
// say what the page says about it -- and, as with the credential warnings,
// most of what matters is SILENCE: nothing while the installer is at work,
// nothing for a machine with nothing to do, nothing an older cockpit never
// measured.

const { machineFromRow } = await import("../../src/apps/fleet/rows");
const { SetupHealth, setupNeedsAttention, setupProblems } = await import("../../src/apps/fleet/machines/SetupHealth");
const { SETUP_QUIET_MS } = await import("../../src/apps/fleet/machines/setup");
const { machineRow } = await import("./harness");

const NOW = new Date("2026-10-18T15:30:00Z");
const ago = (seconds: number) => new Date(NOW.getTime() - seconds * 1000).toISOString();
const GiB = 1024 ** 3;

const MAC_CU = {
  platformInfo: { os: "darwin", arch: "arm64", hostname: "studio.local" },
  capabilities: ["HEADLESS", "COMPUTERUSE"],
  buildTag: "computeruse",
};
const RUNTIME = { hardware: { chip: "M4 Max", memoryBytes: 36 * GiB, runtimes: [{ name: "ollama", version: "0.40.6" }] } };
const ONE = { accessibility: true, screen_recording: false, x11_display: false, detail: "" };
const BOTH = { accessibility: true, screen_recording: true, x11_display: false, detail: "" };

function report(state: string, stages: Array<Record<string, unknown>>, spokeSecondsAgo = 5) {
  return { state, stages, updatedAt: ago(spokeSecondsAgo), reportedAt: ago(spokeSecondsAgo) };
}

function machine(over: Record<string, unknown>) {
  return machineFromRow(machineRow({ id: "v1:worker:registration:a", displayName: "studio", lastSeenAt: ago(3), ...over }));
}

function show(over: Record<string, unknown>) {
  return render(<SetupHealth machine={machine(over)} now={NOW} />);
}

afterEach(cleanup);

describe("silence", () => {
  it("says nothing about a machine that asked for nothing setup does", () => {
    const { container } = show({ capabilities: ["HEADLESS"], buildTag: "headless" });
    expect(container.textContent).toBe("");
  });

  it("says nothing once everything that was asked for is there", () => {
    const { container } = show({
      ...MAC_CU,
      ...RUNTIME,
      permissions: BOTH,
      labels: { "model:qwen3.8:27b": "tools=1" },
      setup: report("finished", [{ name: "permissions", state: "done" }, { name: "runtime", state: "done" }, { name: "models", state: "done" }], 3600),
    });
    expect(container.textContent).toBe("");
  });

  it("says nothing while the installer is still downloading", () => {
    const m = machine({
      ...RUNTIME,
      setup: report("running", [
        { name: "runtime", state: "done" },
        { name: "models", state: "running", model: "qwen3.8:27b", completedBytes: 4 * GiB, totalBytes: 18 * GiB, modelsTotal: 2 },
        { name: "workbench", state: "waiting" },
      ]),
    });
    expect(setupProblems(m, NOW)).toEqual([]);
    expect(setupNeedsAttention(m, NOW)).toBe(false);
  });

  it("says nothing about permissions an older cockpit never measured", () => {
    const { container } = show({ ...MAC_CU });
    expect(container.textContent).toBe("");
  });

  it("says nothing on a revoked machine", () => {
    const { container } = show({ ...MAC_CU, permissions: ONE, revokedAt: ago(60) });
    expect(container.textContent).toBe("");
  });
});

describe("what needs a person", () => {
  it("says setup is waiting for the person on the machine, without calling it a fault", () => {
    show({
      ...MAC_CU,
      permissions: ONE,
      setup: report("running", [{ name: "permissions", state: "running" }, { name: "runtime", state: "waiting" }]),
    });
    expect(screen.getByText("Setup is waiting for you.")).toBeTruthy();
    expect(screen.getByText("Allow MemQL in the Permissions window on studio, or choose Skip for now there.")).toBeTruthy();
    expect(document.querySelector(".os-notice")?.getAttribute("data-tone")).toBe("info");
    expect(setupNeedsAttention(machine({ ...MAC_CU, permissions: ONE, setup: report("running", [{ name: "permissions", state: "running" }]) }), NOW)).toBe(false);
  });

  it("names a permission skipped during setup, and where to turn it on", () => {
    const over = { ...MAC_CU, permissions: ONE, setup: report("finished", [{ name: "permissions", state: "skipped", detail: "Continued without Screen Recording." }]) };
    show(over);
    expect(screen.getByText("Screen Recording is not allowed, so computer use cannot work yet.")).toBeTruthy();
    expect(screen.getByText("Turn it on from MemQL in the menu bar → macOS permissions → Set Up Permissions…")).toBeTruthy();
    expect(setupNeedsAttention(machine(over), NOW)).toBe(true);
  });

  it("names a permission that was never allowed, also on a machine whose cockpit reports no setup", () => {
    show({ ...MAC_CU, permissions: { accessibility: false, screen_recording: false, x11_display: false, detail: "" } });
    expect(screen.getByText("Accessibility and Screen Recording are not allowed, so computer use cannot work yet.")).toBeTruthy();
    expect(screen.getByText(/System Settings → Privacy & Security → Accessibility and Screen Recording\.$/)).toBeTruthy();
  });

  it("gives a failed workbench stage in the installer's words, with the command that retries it", () => {
    show({
      capabilities: ["HEADLESS"],
      setup: report("finished", [{ name: "workbench", state: "failed", detail: "Start Docker with Linux containers, then retry: Docker is not running" }]),
    });
    expect(screen.getByText("Fleet workbenches are not set up.")).toBeTruthy();
    expect(screen.getByText("Start Docker with Linux containers, then retry: Docker is not running")).toBeTruthy();
    expect((screen.getByLabelText("the workbench command") as HTMLInputElement).value).toContain("worker setup --workbench");
  });

  it("picks a stopped download up with the setup command, never a Download button", () => {
    show({
      ...RUNTIME,
      setup: report("running", [
        { name: "runtime", state: "done" },
        { name: "models", state: "running", model: "qwen3.8:27b", completedBytes: 9 * GiB, totalBytes: 18 * GiB },
      ], SETUP_QUIET_MS / 1000 + 60),
    });
    expect(screen.getByText("The model download stopped before it finished.")).toBeTruthy();
    expect(screen.getByText("Run this on the machine to pick up where it left off.")).toBeTruthy();
    expect((screen.getByLabelText("the models command") as HTMLInputElement).value).toContain("worker setup --inference");
    expect(screen.queryByRole("button", { name: "Download models" })).toBeNull();
  });

  it("carries the installer's sentence for a failed download", () => {
    show({
      ...RUNTIME,
      setup: report("finished", [
        { name: "runtime", state: "done" },
        { name: "models", state: "failed", detail: "Not enough free disk for qwen3.8:27b: 18.2 GB needed, 4.0 GB free." },
      ]),
    });
    expect(screen.getByText("The models could not be downloaded.")).toBeTruthy();
    expect(screen.getByText("Not enough free disk for qwen3.8:27b: 18.2 GB needed, 4.0 GB free.")).toBeTruthy();
  });

  it("names a runtime that could not be installed, with its command", () => {
    show({
      setup: report("finished", [
        { name: "runtime", state: "failed", detail: "Homebrew is not installed; install Ollama from ollama.com, then retry." },
        { name: "models", state: "skipped", detail: "Not started: the model runtime is not ready." },
      ]),
    });
    expect(screen.getByText("The model runtime could not be installed.")).toBeTruthy();
    expect(screen.getByText("Homebrew is not installed; install Ollama from ollama.com, then retry.")).toBeTruthy();
    expect((screen.getByLabelText("the model runtime command") as HTMLInputElement).value).toContain("worker setup --inference");
  });
});
