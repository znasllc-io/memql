import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";

// A machine's Settings facts: every row that reports a value copies it, and a
// row saying nothing was reported offers no copy icon (DESIGN.md, "Quiet
// copying" and "Readable, truthful data").

const { machineFromRow } = await import("../../src/apps/fleet/rows");
const { MachineDetailsInfo } = await import("../../src/apps/fleet/machines/MachineDetailsInfo");
const { machineRow } = await import("./harness");

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const NOW = new Date("2026-09-22T12:00:00Z");

function show(over: Record<string, unknown>) {
  const machine = machineFromRow(machineRow({ id: "v1:worker:registration:a", displayName: "Alpha", name: "alpha.local", lastSeenAt: "2026-09-22T11:59:55Z", ...over }));
  return render(<MachineDetailsInfo machine={machine} now={NOW} machineClass="" usableBytes={0} />);
}

it("copies a credential that does not expire as the answer it is", async () => {
  const writeText = vi.fn().mockResolvedValue(undefined);
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  show({ credentialExpiresAt: "" });
  expect(screen.getByText("Does not expire")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Copy Credential expires" }));
  expect(writeText).toHaveBeenCalledExactlyOnceWith("Does not expire");
  await screen.findByRole("button", { name: "Copied Credential expires" });
});

it("offers no copy icon for a measurement that was never reported", () => {
  show({ rttAt: "", rttMs: 0 });
  expect(screen.getByText("Not measured")).toBeTruthy();
  expect(screen.getByText("Not reported")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Copy Round trip" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Copy Clock offset" })).toBeNull();
  expect(screen.getByRole("button", { name: "Copy Status" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Copy Hostname" })).toBeTruthy();
});
