import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { EMPTY_DRAFT, checksFor } from "../../src/apps/fleet/addMachine/flow";
import { InstallStop } from "../../src/apps/fleet/addMachine/stops/Install";
import { MachineStop } from "../../src/apps/fleet/addMachine/stops/Machine";
import { machineFromRow } from "../../src/apps/fleet/rows";
import { machineRow } from "./harness";
import type { ModelPull } from "../../src/apps/fleet/machines/models";

afterEach(cleanup);

describe("Linux installation choices", () => {
  it("offers an explicit passwordless location and explains Linux computer use", () => {
    const onDraft = vi.fn();
    render(<MachineStop draft={{ ...EMPTY_DRAFT, platform: "linux" }} onDraft={onDraft} connected mintError="" onMint={() => {}} />);
    fireEvent.click(screen.getByRole("switch", { name: /Install for my account only/ }));
    expect(onDraft).toHaveBeenCalledWith({ userLocal: true });
    expect(screen.queryByText(/Screen Recording/)).toBeNull();
    expect(screen.getByText(/X11/)).toBeTruthy();
  });

  it.each([false, true])("runs setup from the chosen install location (user local: %s)", (userLocal) => {
    render(<InstallStop draft={{ ...EMPTY_DRAFT, platform: "linux", inference: true, userLocal }} token="test-worker-token" domain="example.com" />);
    const install = (screen.getByLabelText("the install command") as HTMLInputElement).value;
    expect(install.includes(" --user-local")).toBe(userLocal);
    expect((screen.getByLabelText("the local models setup command") as HTMLInputElement).value).toBe(
      `${userLocal ? '"$HOME/.memql/bin/memql"' : "/usr/local/bin/memql"} worker setup --inference`,
    );
    expect(screen.getByText(/once the installer prints SUCCESS/)).toBeTruthy();
  });
});

describe("a partially downloaded recommended set", () => {
  const machine = machineFromRow(machineRow({
    id: "installation-machine",
    labels: { "model:qwen3.8:27b": "ctx=32768" },
    hardware: { runtimes: [{ name: "ollama" }] },
  }));
  const pull: ModelPull = {
    pullId: "pull-embedding", workerId: machine.id, model: "qwen3-embedding:0.6b",
    status: "pulling", statusLine: "downloading", layer: "", completedBytes: 0,
    totalBytes: 0, readvertised: false, errorMessage: "", requestedAt: "", updatedAt: "", endedAt: "",
  };
  it("keeps a second model download visible after the first model starts serving", () => {
    const checks = checksFor({ ...EMPTY_DRAFT, inference: true }, machine, 2, new Date(), { live: pull });
    expect(checks.find((c) => c.id === "models")).toMatchObject({ state: "current" });
    expect(checks.find((c) => c.id === "models")?.answer).toContain(pull.model);
  });
  it("keeps a failed second download visible and offers recovery at the selected location", () => {
    const checks = checksFor({ ...EMPTY_DRAFT, inference: true, userLocal: true }, machine, 2, new Date(), {
      live: null, failed: { ...pull, status: "failed", errorMessage: "disk full" },
    });
    expect(checks.find((c) => c.id === "models")).toMatchObject({
      state: "stopped", act: "pullRecommended", command: '"$HOME/.memql/bin/memql" worker setup --inference',
    });
    expect(checks.find((c) => c.id === "models")?.answer).toContain("disk full");
  });
});
