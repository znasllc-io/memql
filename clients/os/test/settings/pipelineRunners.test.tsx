import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import { PipelineRunners } from "../../src/apps/settings/PipelineRunners";

function answer(nodeId = "workbench-a") {
  return { rows: () => [{ integrationStatus: {
    checkedAt: "2026-10-05T20:00:00Z",
    integrations: [{ name: "pipelines", runners: [{ nodeId, available: true, isolation: "passed", validUntil: "2026-10-05T21:00:00Z" }] }],
  } }] };
}

afterEach(() => { h.connection = null; });

describe("runner readiness surface", () => {
  it("reads without probing, keeps rows on refresh, and withdraws readiness when the read fails", async () => {
    const read = vi.fn().mockResolvedValueOnce(answer()).mockRejectedValueOnce(new Error("node disconnected"));
    h.connection = { query: { pipelinesStatus: read } };
    render(<PipelineRunners />);
    expect(await screen.findByText("Ready")).toBeTruthy();
    expect(read.mock.calls[0]?.[0]).toEqual({});
    fireEvent.focus(window);
    expect(await screen.findByText("Runner readiness could not be read.")).toBeTruthy();
    expect(screen.getByText("workbench-a")).toBeTruthy();
    expect(screen.queryByText("Ready")).toBeNull();
    expect(screen.queryByRole("button", { name: "Refresh readiness" })).toBeNull();
  });

  it("drops a late response from a previous connection", async () => {
    let finish!: (value: ReturnType<typeof answer>) => void;
    const read = vi.fn((_args: unknown, _options?: { signal?: AbortSignal }) => new Promise<ReturnType<typeof answer>>((resolve) => { finish = resolve; }));
    h.connection = { query: { pipelinesStatus: read } };
    const view = render(<PipelineRunners />);
    h.connection = { query: { pipelinesStatus: vi.fn().mockResolvedValue(answer("workbench-b")) } };
    view.rerender(<PipelineRunners />);
    expect(await screen.findByText("workbench-b")).toBeTruthy();
    await act(async () => { finish(answer("old-workbench")); });
    expect(screen.queryByText("old-workbench")).toBeNull();
    expect(read.mock.calls[0]?.[1]?.signal?.aborted).toBe(true);
  });

  it("does not keep a previous cluster's ready report after disconnect", async () => {
    h.connection = { query: { pipelinesStatus: vi.fn().mockResolvedValue(answer()) } };
    const view = render(<PipelineRunners />);
    await screen.findByText("Ready");
    h.connection = null;
    view.rerender(<PipelineRunners />);
    await waitFor(() => expect(screen.queryByText("Ready")).toBeNull());
    expect(screen.queryByRole("button", { name: "Refresh readiness" })).toBeNull();
  });
});
