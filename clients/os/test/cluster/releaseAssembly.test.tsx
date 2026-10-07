import { act, fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Result } from "@znasllc-io/memql-sdk-core/client";
import { ReleaseBrowser } from "../../src/apps/cluster/releases/ReleasesSection";
import { buildRun, releaseFixture, releaseResult } from "./releaseFixtures";

async function start() {
  fireEvent.click(await screen.findByRole("button", { name: "Prepare release" }));
  fireEvent.click(await screen.findByRole("button", { name: "Choose plan engine-release" }));
}
async function choose() { fireEvent.click(await screen.findByRole("button", { name: "Use run pipeline-1" })); await screen.findByText("Selected runs"); }

describe("release preparation", () => {
  it("assembles from work run identities and leaves approval separate", async () => {
    const f = releaseFixture();
    f.state.runs.push({ ...buildRun, id: "wrong-source", repository: "example/other" }, { ...buildRun, id: "failed", conclusion: "failure" }, { ...buildRun, id: "affected", mode: "affected" });
    render(<ReleaseBrowser query={f.query} connected />);
    await start();
    expect(screen.queryByRole("button", { name: "Use run wrong-source" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Use run failed" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Use run affected" })).toBeNull();
    await choose();
    fireEvent.click(screen.getByRole("button", { name: "Prepare release" }));
    await screen.findByText("Versions");
    expect(f.state.calls.filter((c) => c.kind === "assemble")).toEqual([{ kind: "assemble", args: { planName: "engine-release", runs: { build: "work-build-1" } } }]);
    expect(f.state.calls.some((c) => c.kind === "approve" || c.kind === "publish")).toBe(false);
  });
  it("allows run input names that match the wizard's own step names", async () => {
    const f = releaseFixture(); f.state.assemblies[0]!.components[0]!.runs = ["plan"];
    f.state.assemblies[0]!.components[0]!.artifacts[0]!.run = "plan";
    render(<ReleaseBrowser query={f.query} connected />);
    await start(); await choose();
    fireEvent.click(screen.getByRole("button", { name: "Prepare release" }));
    await screen.findByText("Versions");
    expect(f.state.calls.find((c) => c.kind === "assemble")?.args).toEqual({ planName: "engine-release", runs: { plan: "work-build-1" } });
  });
  it("restores focus to the creation control when preparation is cancelled", async () => {
    const f = releaseFixture();
    render(<ReleaseBrowser query={f.query} connected />);
    await start();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Prepare release" }));
  });
  it("keeps same-commit inputs distinct within each component", async () => {
    const f = releaseFixture(); f.state.assemblies[0]!.components[0]!.runs.push("tests");
    f.state.runs.push({ ...buildRun, id: "different-commit", workRunId: "work-other", sha: "f".repeat(40) }, { ...buildRun, id: "tests-1", workRunId: "work-tests" });
    render(<ReleaseBrowser query={f.query} connected />);
    await start();
    fireEvent.click(await screen.findByRole("button", { name: "Use run pipeline-1" }));
    await screen.findByRole("button", { name: "Use run tests-1" });
    expect(screen.queryByRole("button", { name: "Use run pipeline-1" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Use run different-commit" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Use run tests-1" }));
    fireEvent.click(await screen.findByRole("button", { name: "Prepare release" }));
    await screen.findByText("Versions");
    expect(f.state.calls.find((c) => c.kind === "assemble")?.args).toEqual({ planName: "engine-release", runs: { build: "work-build-1", tests: "work-tests" } });
  });
  it("preserves selections across disconnect but requires fresh configuration", async () => {
    const f = releaseFixture();
    const view = render(<ReleaseBrowser query={f.query} connected />);
    await start(); await choose();
    view.rerender(<ReleaseBrowser query={f.query} connected={false} />);
    expect(screen.getByText("Selected runs")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Prepare release" })).toBeNull();
    f.state.targets[0] = { ...f.state.targets[0]!, repository: "example/changed", targetDigest: `sha256:${"9".repeat(64)}` };
    view.rerender(<ReleaseBrowser query={f.query} connected />);
    await screen.findByText(/This release plan changed/);
    expect(screen.queryByRole("button", { name: "Prepare release" })).toBeNull();
  });
  it("does not retry preparation after an uncertain response or reopening", async () => {
    const f = releaseFixture(); f.state.lostAssemblyReply = true;
    const view = render(<ReleaseBrowser query={f.query} connected />);
    await start(); await choose();
    fireEvent.click(screen.getByRole("button", { name: "Prepare release" }));
    await screen.findByText("Preparation reply was lost");
    expect(screen.getByText(/Check Releases before preparing again/)).toBeTruthy();
    view.unmount(); render(<ReleaseBrowser query={f.query} connected />);
    await screen.findByRole("button", { name: "Review engine 0.25.0" });
    expect(f.state.calls.filter((c) => c.kind === "assemble")).toHaveLength(1);
  });
  it.each(["source", "metadata"])("notices changed %s rules even when the visible plan fields are unchanged", async (changed) => {
    const f = releaseFixture();
    const view = render(<ReleaseBrowser query={f.query} connected />);
    await start(); await choose();
    view.rerender(<ReleaseBrowser query={f.query} connected={false} />);
    if (changed === "source") f.state.sources[0]!.path = "other/VERSION";
    else Object.assign(f.state.assemblies[0]!.components[0]!.artifacts[0]!, { imageMetadataPath: "other/metadata.json" });
    view.rerender(<ReleaseBrowser query={f.query} connected />);
    await screen.findByText(/This release plan changed/);
    expect(screen.queryByRole("button", { name: "Prepare release" })).toBeNull();
  });
  it("walks run pages rather than treating the first page as all builds", async () => {
    const f = releaseFixture();
    const calls: (string | undefined)[] = [];
    f.query.pipelineRunsForOwner = async (_args, opts) => {
      calls.push(opts?.cursor);
      return new Result({ data: opts?.cursor ? [buildRun] : [], meta: { cursor: opts?.cursor ? "" : "opaque-older" } } as never);
    };
    render(<ReleaseBrowser query={f.query} connected />);
    await start();
    fireEvent.click(await screen.findByRole("button", { name: "Older runs" }));
    await choose();
    expect(calls).toEqual(["", "opaque-older"]);
  });
  it("waits for the current configuration before offering preparation", async () => {
    const f = releaseFixture();
    const view = render(<ReleaseBrowser query={f.query} connected />);
    await start(); await choose();
    view.rerender(<ReleaseBrowser query={f.query} connected={false} />);
    let resolve!: (r: ReturnType<typeof releaseResult>) => void;
    vi.spyOn(f.query, "releaseCandidateConfiguration").mockImplementationOnce(() => new Promise((r) => { resolve = r; }));
    view.rerender(<ReleaseBrowser query={f.query} connected />);
    expect(screen.queryByRole("button", { name: "Prepare release" })).toBeNull();
    await act(async () => resolve(releaseResult({ targets: f.state.targets, sources: f.state.sources, assemblies: f.state.assemblies })));
    await screen.findByRole("button", { name: "Prepare release" });
  });
});
