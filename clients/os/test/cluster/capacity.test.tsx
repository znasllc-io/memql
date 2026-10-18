import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CapacityPage } from "../../src/apps/cluster/capacity/CapacitySection";
import { readCapacity } from "../../src/apps/cluster/capacity/model";
import type { CapacityQueries } from "../../src/apps/cluster/capacity/useCapacity";

const snapshot = () => ({ available: true, observedAt: "2026-10-17T20:00:00Z", revision: "q:1", policyRevision: "p:1", accepting: true,
  ceiling: { cpuMilli: 6500, memoryMiB: 22528, diskMiB: 81920, jobs: 6 },
  budget: { cpuMilli: 5000, memoryMiB: 16384, diskMiB: 81920, jobs: 4 }, waiting: 3, running: 1 });
function fixture() {
  const read = vi.fn(async (_: unknown, _options?: unknown) => ({ single: () => snapshot() }));
  const write = vi.fn(async (_: unknown) => ({ single: () => snapshot() }));
  return { read, write, query: { clusterBackgroundCapacity: read, clusterSetBackgroundCapacity: write } as unknown as CapacityQueries };
}
afterEach(() => { vi.restoreAllMocks(); });

describe("background capacity", () => {
  it("preserves unknown measurements and refuses malformed reports", () => {
    expect(readCapacity(snapshot()).reserved).toBeUndefined();
    expect(() => readCapacity({ ...snapshot(), budget: { ...snapshot().budget, jobs: -1 } })).toThrow();
    expect(() => readCapacity({ ...snapshot(), running: Number.NaN })).toThrow();
    expect(readCapacity({ available: false, observedAt: snapshot().observedAt, reason: "Unavailable" }).budget).toBeUndefined();
  });
  it("does not invent usage from a reservation or read while hidden", async () => {
    const f = fixture(); const view = render(<CapacityPage query={f.query} visible={false} connected />);
    expect(f.read).not.toHaveBeenCalled();
    view.rerender(<CapacityPage query={f.query} visible connected />);
    await screen.findByText("3");
    expect(screen.getByText("Not reported / 5 cores")).toBeTruthy();
    expect(f.read.mock.calls[0]?.[1]).toMatchObject({ priority: "background" });
  });
  it("refuses an allocation over the ceiling before sending a write", async () => {
    const f = fixture(); render(<CapacityPage query={f.query} visible connected />);
    await screen.findByText("3"); fireEvent.click(screen.getByRole("button", { name: "Settings" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Processing cores" }), { target: { value: "99" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await screen.findByText("Enter positive values within the limits shown below.");
    expect(f.write).not.toHaveBeenCalled();
  });
  it("sends the displayed revision once and does not repeat an uncertain write", async () => {
    const f = fixture(); f.write.mockRejectedValue(new Error("Connection lost after send"));
    render(<CapacityPage query={f.query} visible connected />);
    await screen.findByText("3"); fireEvent.click(screen.getByRole("button", { name: "Settings" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Maximum concurrent jobs" }), { target: { value: "5" } });
    const save = screen.getByRole("button", { name: "Save changes" });
    fireEvent.click(save); fireEvent.click(save);
    await screen.findByText("The change could not be confirmed. Check the current values before saving again.");
    expect(f.write).toHaveBeenCalledTimes(1);
    expect(f.write).toHaveBeenCalledWith(expect.objectContaining({ revision: "q:1", policyRevision: "p:1", jobs: 5 }));
    await waitFor(() => expect(f.read).toHaveBeenCalledTimes(2));
  });
  it("ignores an old connection's late answer", async () => {
    const first=fixture(), next=fixture(); let finish!: (r: { single: () => ReturnType<typeof snapshot> }) => void;
    first.read.mockImplementation(() => new Promise(resolve => {finish=resolve;}));
    next.read.mockResolvedValue({single: () => ({...snapshot(),waiting:7})});
    const view=render(<CapacityPage query={first.query} visible connected />);
    view.rerender(<CapacityPage query={next.query} visible connected />);
    await screen.findByText("7");
    await act(async () => finish({single:snapshot}));
    expect(screen.queryByText("3")).toBeNull();
  });
});
