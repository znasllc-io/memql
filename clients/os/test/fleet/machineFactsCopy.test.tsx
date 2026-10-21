import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";

// A machine's Details: copy is offered where a value is worth pasting
// elsewhere -- names, identifiers, versions, specifications and exact times --
// and never on a status or an unreported value (DESIGN.md, "Quiet copying").

const { machineFromRow } = await import("../../src/apps/fleet/rows");
const { MachineDetailsInfo } = await import("../../src/apps/fleet/machines/MachineDetailsInfo");
const { machineRow } = await import("./harness");

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const NOW = new Date("2026-09-22T12:00:00Z");
const HEARTBEAT = "2026-09-22T11:59:55Z";

function show(over: Record<string, unknown>) {
  const machine = machineFromRow(machineRow({ id: "v1:worker:registration:a", displayName: "Alpha", name: "alpha.local", lastSeenAt: HEARTBEAT, version: "0.8.0", capabilityDescriptor: { displayServer: "quartz" }, ...over }));
  return render(<MachineDetailsInfo machine={machine} now={NOW} machineClass="" usableBytes={0} />);
}

it("copies names, identifiers, versions and the exact time behind a relative one", async () => {
  const writeText = vi.fn().mockResolvedValue(undefined);
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  show({});
  for (const label of ["Reported name", "Hostname", "Display server", "Registration id", "Cockpit version"]) {
    expect(screen.getByRole("button", { name: `Copy ${label}` })).toBeTruthy();
  }
  fireEvent.click(screen.getByRole("button", { name: "Copy Last heartbeat" }));
  expect(writeText).toHaveBeenCalledExactlyOnceWith(HEARTBEAT);
  await screen.findByRole("button", { name: "Copied Last heartbeat" });
});

it("reads statuses and unreported values without a copy icon", () => {
  show({ credentialExpiresAt: "", rttAt: "", rttMs: 0 });
  expect(screen.getByText("Does not expire")).toBeTruthy();
  expect(screen.getByText("Not measured")).toBeTruthy();
  expect(screen.getByText("Not reported")).toBeTruthy();
  for (const label of ["Status", "Active calls", "Model inference", "Fleet workbenches", "Commands and files", "CLIs and apps", "Computer use", "Pipeline steps", "Credential expires", "Round trip", "Clock offset"]) {
    expect(screen.queryByRole("button", { name: `Copy ${label}` })).toBeNull();
  }
});
