import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { EMPTY_DRAFT, checksFor } from "../../src/apps/fleet/addMachine/flow";
import { InstallStop } from "../../src/apps/fleet/addMachine/stops/Install";
import { MachineStop } from "../../src/apps/fleet/addMachine/stops/Machine";
import { machineFromRow } from "../../src/apps/fleet/rows";
import { machineRow } from "./harness";
import type { ModelPull } from "../../src/apps/fleet/machines/models";
import { allowAppsCommand } from "../../src/apps/fleet/addMachine/install";

afterEach(cleanup);

describe("Linux installation choices", () => {
  it("offers an explicit passwordless location and explains Linux computer use", () => {
    const onDraft = vi.fn();
    render(<MachineStop draft={{ ...EMPTY_DRAFT, platform: "linux" }} onDraft={onDraft} connected mintError="" onMint={() => {}} />);
    fireEvent.click(screen.getByRole("switch", { name: /Install for my account only/ }));
    expect(onDraft).toHaveBeenCalledWith({ userLocal: true });
    expect(screen.queryByText(/Screen Recording/)).toBeNull();
    expect(screen.getByText(/X11/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^About / })).toBeNull();
  });

  it.each([false, true])("runs setup from the chosen install location (user local: %s)", (userLocal) => {
    render(<InstallStop draft={{ ...EMPTY_DRAFT, platform: "linux", inference: true, userLocal }} token="test-worker-token" domain="example.com" />);
    const install = (screen.getByLabelText("the install command") as HTMLInputElement).value;
    expect(install.includes(" --user-local")).toBe(userLocal);
    expect((screen.getByLabelText("the local models setup command") as HTMLInputElement).value).toBe(
      `${userLocal ? '"$HOME/.memql/bin/memql"' : "/usr/local/bin/memql"} worker setup --inference`,
    );
    expect(screen.getByText(/Run after Cockpit is installed/)).toBeTruthy();
  });
});

describe("concise install instructions", () => {
  it.each([false, true])("keeps a copyable app grant beside the install command (user local: %s)", async (userLocal) => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
    render(<InstallStop draft={{ ...EMPTY_DRAFT, platform: "linux", userLocal }} token="test-worker-token" domain="example.com" />);
    expect(screen.getByText(/installer detects Claude Code and Codex/)).toBeTruthy();
    const details = screen.getByLabelText("the app permissions command").closest("details")!;
    expect(details.open).toBe(false);
    fireEvent.click(screen.getByText("App permissions"));
    fireEvent.click(screen.getByRole("button", { name: "Copy the app permissions command" }));
    expect(writeText).toHaveBeenCalledWith(`${userLocal ? '"$HOME/.memql/bin/memql"' : "/usr/local/bin/memql"} worker apps --allow claude-code --allow codex --home 'https://api.example.com'`);
    expect(await screen.findByRole("button", { name: "Copied" })).toBeTruthy();
    expect(writeText.mock.calls[0]![0]).not.toContain("test-worker-token");
  });

  it("never creates an app grant for an unknown cluster or app", () => {
    expect(allowAppsCommand("")).toBe("");
    expect(allowAppsCommand("https://api.example.com", ["codex; bad"])).toBe("");
    expect(allowAppsCommand("https://api.example.com", [])).toBe("");
  });
  it.each([false, true])("shows only the commands needed for local models=%s and reveals the separate token on demand", (inference) => {
    render(<InstallStop draft={{ ...EMPTY_DRAFT, inference }} token="test-worker-token" domain="example.com" />);
    expect(screen.getByRole("button", { name: "Copy the install command" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Copy the local models setup command" }) !== null).toBe(inference);
    const tokenDetails = screen.getByLabelText("the worker token").closest("details")!;
    expect(tokenDetails.open).toBe(false);
    fireEvent.click(screen.getByText("Connection token"));
    expect(tokenDetails.open).toBe(true);
    expect(screen.getByRole("button", { name: "Copy the worker token" })).toBeTruthy();
    expect(screen.getByText(/private token shown only during this setup/)).toBeTruthy();
  });

  it("keeps a missing cluster address visible as a required change before running the command", () => {
    render(<InstallStop draft={EMPTY_DRAFT} token="test-worker-token" domain="" />);
    expect(screen.getByText("Set the cluster address before running this command.")).toBeTruthy();
    expect(screen.getByText(/including https:\/\//)).toBeTruthy();
    expect(screen.queryByLabelText("the app permissions command")).toBeNull();
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
