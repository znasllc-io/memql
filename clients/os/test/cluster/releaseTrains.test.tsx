import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Result } from "@znasllc-io/memql-sdk-core/client";
import { ReleaseTrainPanel, type TrainQueries } from "../../src/apps/cluster/releases/ReleaseTrainPanel";

beforeEach(() => { vi.useFakeTimers({ toFake: ["Date"] }); });
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

function row(fields: Record<string, unknown>) {
  return { id: String(fields.id ?? "t1"), concept: "v1:pipelines:train", payload: { repository: "acme/shop", branch: "train/2026-11-02-1", state: "landed", version: "0.31.5", departedAt: "2026-11-02T04:00:00Z", pullRequestUrl: "https://github.com/acme/shop/pull/7", ...fields } };
}

function fixture(rows: object[], express: TrainQueries["pipelinesTrainExpress"] = async () => new Result({ data: [] } as never)) {
  const started: unknown[] = [];
  const query: TrainQueries = {
    pipelineTrainsForOwner: async () => new Result({ data: rows } as never),
    pipelinesTrainExpress: async (args, opts) => { started.push(args); return express(args, opts); },
  };
  return { query, started };
}

describe("the latest release", () => {
  it("shows nothing when no release has ever left", async () => {
    const { query } = fixture([]);
    const { container } = render(<ReleaseTrainPanel query={query} connected />);
    await act(async () => { await Promise.resolve(); });
    expect(container.textContent).toBe("");
  });

  it("shows the newest release of each repository, its state and why it stopped", async () => {
    const { query } = fixture([
      row({ id: "t3", state: "held", heldReason: "tests.go-unit#1 failed again on its re-run" }),
      row({ id: "t2", state: "landed", branch: "train/2026-11-01-1" }),
      row({ id: "t1", repository: "acme/other", state: "landed", version: "1.2.3" }),
    ]);
    render(<ReleaseTrainPanel query={query} connected />);
    expect(await screen.findByText("Held")).toBeTruthy();
    expect(screen.getByText("tests.go-unit#1 failed again on its re-run")).toBeTruthy();
    expect(screen.getByRole("link", { name: "acme/shop" }).getAttribute("href")).toBe("https://github.com/acme/shop/pull/7");
    expect(screen.queryByText(/train\/2026-11-01-1/)).toBeNull();
    expect(screen.getByText("acme/other")).toBeTruthy();
  });

  it("starts a release with no pull requests named", async () => {
    const { query, started } = fixture([row({ state: "landed" })]);
    render(<ReleaseTrainPanel query={query} connected />);
    fireEvent.click(await screen.findByRole("button", { name: "Start release now" }));
    await act(async () => { await Promise.resolve(); });
    expect(started).toEqual([{ repository: "acme/shop", pullRequests: [] }]);
  });

  it("says plainly that starting one replaces a held release", async () => {
    const { query } = fixture([row({ state: "held" })]);
    render(<ReleaseTrainPanel query={query} connected />);
    expect(await screen.findByRole("button", { name: "Replace with a new release" })).toBeTruthy();
  });

  it("offers nothing while one is building, testing or landing", async () => {
    const { query } = fixture([row({ state: "running" })]);
    render(<ReleaseTrainPanel query={query} connected />);
    expect(await screen.findByText("Testing")).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("shows the engine's refusal in its own words", async () => {
    const { query } = fixture([row({ state: "landed" })], async () => { throw new Error("pipelines: train/2026-11-02-2 is running; start the express train when it has landed or is held"); });
    render(<ReleaseTrainPanel query={query} connected />);
    fireEvent.click(await screen.findByRole("button", { name: "Start release now" }));
    expect(await screen.findByText(/is running; start the express train/)).toBeTruthy();
  });
});
