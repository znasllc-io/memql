import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Result } from "@znasllc-io/memql-sdk-core/client";
import { ReleaseBrowser } from "../../src/apps/cluster/releases/ReleasesSection";
import { readTheBar } from "../readTheBar";
import { buildRun, releaseFixture, releaseResult } from "./releaseFixtures";

// A fake Date, so reading a bar costs nothing: the release reads poll on real timers.
beforeEach(() => { vi.useFakeTimers({ toFake: ["Date"] }); });
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

/** Presses a bar act once the bar has been read: a step that opens changes the bar, and a bar that has just changed takes no press for a moment. */
async function pressBar(label: string) { await screen.findByRole("button", { name: label }); await readTheBar(); fireEvent.click(screen.getByRole("button", { name: label })); }

async function start() {
  fireEvent.click(await screen.findByRole("button", { name: "Prepare release" }));
  fireEvent.click(await screen.findByRole("button", { name: "Choose plan engine-release" }));
}
async function choose() { fireEvent.click(await screen.findByRole("button", { name: /^Use run: Build engine 0\.25\.0,/ })); await screen.findByText("Selected runs"); }

describe("release preparation", () => {
  it("keeps exact source revisions optional and copyable without changing the selected run", async () => {
    const f = releaseFixture();
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    render(<ReleaseBrowser query={f.query} connected />);
    await start();
    await screen.findByRole("button", { name: /^Use run: Build engine 0\.25\.0,/ });
    const runButton = screen.getByRole("button", { name: /^Use run: Build engine 0\.25\.0,/ });
    expect(runButton.getAttribute("aria-label")).toContain(buildRun.releaseTag);
    expect(runButton.getAttribute("aria-label")).not.toContain(buildRun.id);
    const pickerDetails = screen.getByText(buildRun.sha).closest("details")!;
    expect(pickerDetails.open).toBe(false);
    expect(screen.getByText(buildRun.title).closest("details")).toBeNull();
    fireEvent.click(screen.getByText("Source revision"));
    expect(pickerDetails.open).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Copy Revision" }));
    await screen.findByRole("button", { name: "Copied Revision" });
    expect(writeText).toHaveBeenLastCalledWith(buildRun.sha);
    await choose();
    const selectedDetails = screen.getByText(buildRun.sha).closest("details")!;
    expect(selectedDetails.open).toBe(false);
    fireEvent.click(screen.getByText("Source revisions"));
    fireEvent.click(screen.getByRole("button", { name: "Copy engine / build" }));
    await screen.findByRole("button", { name: "Copied engine / build" });
    expect(writeText).toHaveBeenLastCalledWith(buildRun.sha);
    await pressBar("Prepare release");
    await screen.findByText("Versions");
    expect(f.state.calls.find((c) => c.kind === "assemble")?.args).toEqual({ planName: "engine-release", runs: { build: "work-build-1" } });
    expect(f.state.calls.some((c) => ["approve", "publish", "seal"].includes(c.kind))).toBe(false);
  });
  it("offers tagged release runs to profiles while retaining full push runs for legacy plans", async () => {
    const f = releaseFixture();
    f.state.profiles = [{ name: "engine", configuration: `sha256:${"4".repeat(64)}`, components: [{ name: "engine", repository: "example/memql", runs: ["release"] }] }];
    f.state.runs.push({ ...buildRun, id: "main-push", title: "Build latest engine change", event: "push", releaseTag: "", workRunId: "work-push" }, { ...buildRun, id: "missing-tag", title: "Build without release tag", releaseTag: "", workRunId: "work-no-tag" });
    render(<ReleaseBrowser query={f.query} connected />);
    fireEvent.click(await screen.findByRole("button", { name: "Prepare release" }));
    fireEvent.click(await screen.findByRole("button", { name: "Choose profile engine" }));
    await screen.findByRole("button", { name: /^Use run: Build engine 0\.25\.0,/ });
    expect(screen.queryByRole("button", { name: /^Use run: Build latest engine change,/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Use run: Build without release tag,/ })).toBeNull();
    await pressBar("Cancel");
    await start();
    fireEvent.click(await screen.findByRole("button", { name: /^Use run: Build latest engine change,/ }));
    await pressBar("Prepare release");
    await screen.findByText("Versions");
    expect(f.state.calls.filter((c) => c.kind === "assemble")).toEqual([{ kind: "assemble", args: { planName: "engine-release", runs: { build: "work-push" } } }]);
    expect(f.state.calls.some((c) => c.kind === "profile")).toBe(false);
  });
  it("prepares a stable profile with its reviewed digest while legacy configuration is unavailable", async () => {
    const f = releaseFixture();
    const configuration = `sha256:${"4".repeat(64)}`;
    f.state.profiles = [{ name: "engine", configuration, components: [{ name: "engine", repository: "example/memql", runs: ["release"] }] }];
    f.query.releaseCandidateConfiguration = async () => { throw new Error("Legacy plans unavailable"); };
    render(<ReleaseBrowser query={f.query} connected />);
    fireEvent.click(await screen.findByRole("button", { name: "Prepare release" }));
    fireEvent.click(await screen.findByRole("button", { name: "Choose profile engine" }));
    await choose();
    expect(screen.getByText(/review the exact result before approving/)).toBeTruthy();
    await pressBar("Prepare release");
    await screen.findByText("Versions");
    expect(f.state.calls.filter((c) => c.kind === "profile")).toEqual([{ kind: "profile", args: { profileName: "engine", expectedConfiguration: configuration, runs: { release: "work-build-1" } } }]);
    expect(f.state.calls.some((c) => ["approve", "publish", "seal"].includes(c.kind))).toBe(false);
    await screen.findByRole("button", { name: "Approve release" });
  });
  it("keeps a profile selection through reconnect but refuses a changed resolver digest", async () => {
    const f = releaseFixture();
    f.state.profiles = [{ name: "engine", configuration: `sha256:${"4".repeat(64)}`, components: [{ name: "engine", repository: "example/memql", runs: ["release"] }] }];
    const view = render(<ReleaseBrowser query={f.query} connected />);
    fireEvent.click(await screen.findByRole("button", { name: "Prepare release" }));
    fireEvent.click(await screen.findByRole("button", { name: "Choose profile engine" }));
    await choose();
    view.rerender(<ReleaseBrowser query={f.query} connected={false} />);
    f.state.profiles[0]!.configuration = `sha256:${"5".repeat(64)}`;
    view.rerender(<ReleaseBrowser query={f.query} connected />);
    await screen.findByText(/This release plan changed/);
    expect(screen.queryByRole("button", { name: "Prepare release" })).toBeNull();
    expect(f.state.calls.some((c) => c.kind === "profile")).toBe(false);
  });
  it("keeps legacy plans usable when the profile read fails", async () => {
    const f = releaseFixture();
    f.query.releaseCandidateProfiles = async () => { throw new Error("Profiles unavailable"); };
    render(<ReleaseBrowser query={f.query} connected />);
    await start(); await choose(); await pressBar("Prepare release");
    await screen.findByText("Versions");
    expect(f.state.calls.filter((c) => c.kind === "assemble")).toHaveLength(1);
  });
  it("assembles from work run identities and leaves approval separate", async () => {
    const f = releaseFixture();
    f.state.runs.push({ ...buildRun, id: "wrong-source", title: "Build another repository", repository: "example/other" }, { ...buildRun, id: "failed", title: "Failed engine build", conclusion: "failure" }, { ...buildRun, id: "affected", title: "Build affected engine files", mode: "affected" });
    render(<ReleaseBrowser query={f.query} connected />);
    await start();
    expect(screen.queryByRole("button", { name: /^Use run: Build another repository,/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Use run: Failed engine build,/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Use run: Build affected engine files,/ })).toBeNull();
    await choose();
    await pressBar("Prepare release");
    await screen.findByText("Versions");
    expect(f.state.calls.filter((c) => c.kind === "assemble")).toEqual([{ kind: "assemble", args: { planName: "engine-release", runs: { build: "work-build-1" } } }]);
    expect(f.state.calls.some((c) => c.kind === "approve" || c.kind === "publish")).toBe(false);
  });
  it("allows run input names that match the wizard's own step names", async () => {
    const f = releaseFixture(); f.state.assemblies[0]!.components[0]!.runs = ["plan"];
    f.state.assemblies[0]!.components[0]!.artifacts[0]!.run = "plan";
    render(<ReleaseBrowser query={f.query} connected />);
    await start(); await choose();
    await pressBar("Prepare release");
    await screen.findByText("Versions");
    expect(f.state.calls.find((c) => c.kind === "assemble")?.args).toEqual({ planName: "engine-release", runs: { plan: "work-build-1" } });
  });
  it("restores focus to the creation control when preparation is cancelled", async () => {
    const f = releaseFixture();
    render(<ReleaseBrowser query={f.query} connected />);
    await start();
    await pressBar("Cancel");
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Prepare release" }));
  });
  it("keeps same-commit inputs distinct within each component", async () => {
    const f = releaseFixture(); f.state.assemblies[0]!.components[0]!.runs.push("tests");
    f.state.runs.push({ ...buildRun, id: "different-commit", title: "Build another revision", workRunId: "work-other", sha: "f".repeat(40) }, { ...buildRun, id: "tests-1", title: "Verify engine release", workRunId: "work-tests" });
    render(<ReleaseBrowser query={f.query} connected />);
    await start();
    fireEvent.click(await screen.findByRole("button", { name: /^Use run: Build engine 0\.25\.0,/ }));
    await screen.findByRole("button", { name: /^Use run: Verify engine release,/ });
    expect(screen.queryByRole("button", { name: /^Use run: Build engine 0\.25\.0,/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Use run: Build another revision,/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /^Use run: Verify engine release,/ }));
    await pressBar("Prepare release");
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
    await pressBar("Prepare release");
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
  it("keeps an unchanged plan when another replica returns reordered object keys", async () => {
    const f = releaseFixture();
    const view = render(<ReleaseBrowser query={f.query} connected />);
    await start(); await choose();
    view.rerender(<ReleaseBrowser query={f.query} connected={false} />);
    const reordered = (value: unknown): unknown => Array.isArray(value) ? value.map(reordered) : value !== null && typeof value === "object" ? Object.fromEntries(Object.entries(value).reverse().map(([key, v]) => [key, reordered(v)])) : value;
    f.query.releaseCandidateConfiguration = async () => releaseResult(reordered({ targets: f.state.targets, sources: f.state.sources, assemblies: f.state.assemblies }) as object);
    view.rerender(<ReleaseBrowser query={f.query} connected />);
    await screen.findByRole("button", { name: "Prepare release" });
    expect(screen.queryByText(/This release plan changed/)).toBeNull();
  });
  it("ignores changing builtin node IDs and timestamps on reconnect", async () => {
    const f = releaseFixture();
    const view = render(<ReleaseBrowser query={f.query} connected />);
    await start(); await choose();
    // The configuration fixture uses the real id-keyed builtin reply envelope
    // and creates a different node ID and createdAt for every read.
    view.rerender(<ReleaseBrowser query={f.query} connected={false} />);
    view.rerender(<ReleaseBrowser query={f.query} connected />);
    await screen.findByRole("button", { name: "Prepare release" });
    expect(screen.queryByText(/This release plan changed/)).toBeNull();
  });
});
