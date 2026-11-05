import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ReleaseBrowser } from "../../src/apps/cluster/releases/ReleasesSection";
import { readProfiles } from "../../src/apps/cluster/releases/assembly";
import { readReleaseBuild, readReleaseBuilds, untestedSource } from "../../src/apps/cluster/releases/journey";
import { runFromRow } from "../../src/apps/deployables/pipelines/rows";
import { readTheBar, useFakeDate } from "../readTheBar";
import { releaseResult } from "./releaseFixtures";
import { intentId, profileConfiguration, releaseJourneyFixture, releasePipeline, releaseRun } from "./releaseJourneyFixtures";

async function press(label: string) { await screen.findByRole("button", { name: label }); await readTheBar(); fireEvent.click(screen.getByRole("button", { name: label })); }
async function twice(label: string) { await press(label); await press(label); }
async function open() { fireEvent.click(await screen.findByRole("button", { name: "Open release engine 0.25.0" })); }
async function selectSource() {
  fireEvent.click(await screen.findByRole("button", { name: "Cut release" }));
  fireEvent.click(await screen.findByRole("button", { name: "Release engine" }));
  fireEvent.click(await screen.findByRole("button", { name: "Use pipeline MemQL" }));
}
function writes(f: ReturnType<typeof releaseJourneyFixture>) { return f.journey.calls.filter(c => ["releasePrepareBuild", "releaseStartBuild", "releaseResumeBuild", "releasePublishBuild"].includes(c.name)); }

describe("guided release", () => {
  useFakeDate();
  it("checks the selected profile/source before a separately confirmed build", async () => {
    const f = releaseJourneyFixture("new");
    f.journey.pipelines.push({ ...releasePipeline, id: "foreign", name: "Other repository", repository: "someone/else" });
    render(<ReleaseBrowser query={f.query} connected />);
    await selectSource();
    expect(screen.queryByRole("button", { name: "Use pipeline Other repository" })).toBeNull();
    expect(writes(f)).toEqual([]);
    await press("Check source");
    await screen.findByRole("button", { name: "Start release build" });
    expect(writes(f)).toEqual([{ name: "releasePrepareBuild", call: `builtin releasePrepareBuild(profileName: "engine", pipelineId: "pipeline-source", expectedConfiguration: "${profileConfiguration}")` }]);
    const revision = screen.getByText("d".repeat(40)).closest("details")!;
    expect(revision.open).toBe(false);
    await press("Start release build");
    expect(writes(f)).toHaveLength(1);
    await press("Start release build");
    await screen.findByText("Build engine 0.25.0");
    expect(writes(f).map(c => c.name)).toEqual(["releasePrepareBuild", "releaseStartBuild"]);
    expect(f.state.calls.some(c => ["approve", "publish", "seal"].includes(c.kind))).toBe(false);
  });
  it("lets the source be checked again when no recent commit passed its checks", async () => {
    const f = releaseJourneyFixture("new");
    f.journey.untestedPrepare = "None of the newest 20 commits on main has passed its required checks: 2 running, 18 untested. Check the source again once a commit's checks pass.";
    render(<ReleaseBrowser query={f.query} connected />);
    await selectSource();
    expect(screen.getByText(/picks the newest of the last 20 commits on main whose checks passed/)).toBeTruthy();
    await press("Check source");
    await screen.findByText(f.journey.untestedPrepare);
    // Nothing was saved, so there is no request to look for and nothing locks.
    expect(screen.queryByText(/may have been saved/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Read release requests" })).toBeNull();
    f.journey.untestedPrepare = "";
    await press("Check source");
    await screen.findByRole("button", { name: "Start release build" });
    expect(writes(f).map(c => c.name)).toEqual(["releasePrepareBuild", "releasePrepareBuild"]);
  });
  it("shows the commit it released and the newer commits checking the source passed over", async () => {
    const f = releaseJourneyFixture(); f.skipped();
    render(<ReleaseBrowser query={f.query} connected />);
    await open(); await screen.findByRole("button", { name: "Start release build" });
    fireEvent.click(screen.getByRole("button", { name: /^Source / }));
    await screen.findByText("Newest on main when checked");
    expect(screen.getByText("ddddddd")).toBeTruthy();
    const newer = screen.getByRole("list", { name: "Newer commits" });
    expect(within(newer).getAllByRole("listitem").map(item => item.textContent)).toEqual(["eeeeeeeChecks running", "fffffffChecks failed", "1111111Superseded", "2222222Not tested"]);
    expect(within(newer).getByTitle("MemQL / memql / push: failure").textContent).toBe("Checks failed");
    expect(screen.getByText(/newest commit whose checks had passed/)).toBeTruthy();
    expect(writes(f)).toEqual([]);
  });
  it("shows only the revision when the newest commit was released", async () => {
    const f = releaseJourneyFixture();
    render(<ReleaseBrowser query={f.query} connected />);
    await open(); await screen.findByRole("button", { name: "Start release build" });
    fireEvent.click(screen.getByRole("button", { name: /^Source / }));
    await screen.findByText(/keeps its recorded source/);
    expect(screen.getByText("ddddddd")).toBeTruthy();
    expect(screen.queryByText(/when checked/)).toBeNull();
    expect(screen.queryByRole("list", { name: "Newer commits" })).toBeNull();
  });
  it("finds a saved request after a lost preparation reply without preparing again", async () => {
    const f = releaseJourneyFixture("new"); f.journey.lostPrepareReply = true;
    render(<ReleaseBrowser query={f.query} connected />);
    await selectSource(); await press("Check source");
    await screen.findByText("Source check reply was lost");
    expect(screen.queryByRole("button", { name: "Check source" })).toBeNull();
    await press("Read release requests"); await open();
    await screen.findByRole("button", { name: "Start release build" });
    expect(writes(f).map(c => c.name)).toEqual(["releasePrepareBuild"]);
  });
  it("reopens an uncertain request with reads only and explicitly reconciles its same identity", async () => {
    const f = releaseJourneyFixture(); f.journey.lostStartReply = true;
    const view = render(<ReleaseBrowser query={f.query} connected />);
    await open(); await twice("Start release build");
    await screen.findByText("Start reply was lost");
    view.unmount();
    render(<ReleaseBrowser query={f.query} connected openRequest={{ intentId, revision: 1 }} />);
    await screen.findByRole("button", { name: "Check interrupted release" });
    expect(writes(f).map(c => c.name)).toEqual(["releaseStartBuild"]);
    await press("Check interrupted release");
    await screen.findByText("Build engine 0.25.0");
    expect(writes(f).map(c => c.name)).toEqual(["releaseStartBuild", "releaseResumeBuild"]);
    expect(writes(f)[1]!.call).toBe(`builtin releaseResumeBuild(intentId: "${intentId}")`);
  });
  it("observes only an allowed build and does not automatically assemble completed artifacts", async () => {
    const f = releaseJourneyFixture("building");
    const view = render(<ReleaseBrowser query={f.query} connected />);
    await open(); await screen.findByText("Build engine 0.25.0");
    expect(f.journey.calls.some(c => c.name === "releaseObserveBuild")).toBe(true);
    Object.assign(f.journey.build, { phase: "built", actions: ["resume"] });
    view.rerender(<ReleaseBrowser query={f.query} connected={false} />);
    view.rerender(<ReleaseBrowser query={f.query} connected />);
    await screen.findByRole("button", { name: "Prepare candidate" });
    expect(writes(f)).toEqual([]);
    await press("Prepare candidate");
    await screen.findByRole("button", { name: "Approve release" });
    expect(writes(f).map(c => c.name)).toEqual(["releaseResumeBuild"]);
  });
  it("keeps approval separate from configured publication and installation", async () => {
    const f = releaseJourneyFixture("ready"), updates = vi.fn();
    render(<ReleaseBrowser query={f.query} connected onUpdates={updates} />);
    await open(); await twice("Approve release");
    await screen.findByRole("button", { name: "Publish release" });
    expect(f.state.calls.filter(c => c.kind === "approve")).toEqual([{ kind: "approve", args: { candidateId: f.state.record.candidateId } }]);
    expect(writes(f)).toEqual([]);
    await twice("Publish release");
    await screen.findByText("Release available");
    expect(writes(f)).toEqual([{ name: "releasePublishBuild", call: `builtin releasePublishBuild(intentId: "${intentId}", approvalId: "approval-exact")` }]);
    expect(updates).not.toHaveBeenCalled();
    await press("Review installation choices"); expect(updates).toHaveBeenCalledTimes(1);
    expect(f.state.calls.some(c => ["draft", "publish", "promote", "seal"].includes(c.kind))).toBe(false);
  });
  it("confirms a completed publication after a lost reply without sending it again", async () => {
    const f = releaseJourneyFixture("ready"); f.journey.lostPublishReply = true;
    render(<ReleaseBrowser query={f.query} connected />);
    await open(); await twice("Approve release"); await twice("Publish release");
    await screen.findByText("Publication reply was lost");
    await press("Read status again");
    fireEvent.click(await screen.findByRole("button", { name: /^Catalog / }));
    await screen.findByText("Release available");
    expect(writes(f).filter(c => c.name === "releasePublishBuild")).toHaveLength(1);
    expect(screen.queryByRole("button", { name: "Publish release" })).toBeNull();
  });
  it("does not offer publication for a mismatched candidate or approval", async () => {
    const f = releaseJourneyFixture("ready"); f.journey.syncApproval = false;
    Object.assign(f.journey.build, { approvalId: "recorded-approval", actions: ["publish"] });
    Object.assign(f.state.record, { state: "approved", approvalId: "different-approval" });
    render(<ReleaseBrowser query={f.query} connected />); await open();
    await screen.findByText(/Publish the approved artifacts/);
    expect(screen.queryByRole("button", { name: "Publish release" })).toBeNull();
    expect(writes(f)).toEqual([]);
  });
  it("does not approve a candidate for another exact source", async () => {
    const f = releaseJourneyFixture("ready"); f.state.record.manifest.components[0]!.commit = "e".repeat(40);
    render(<ReleaseBrowser query={f.query} connected />); await open();
    await screen.findByText("The release candidate does not match this request’s source and version.");
    expect(screen.queryByRole("button", { name: "Approve release" })).toBeNull();
  });
  it("keeps unavailable catalog status distinct from a completed release", async () => {
    const f = releaseJourneyFixture("published"); f.journey.build.catalog = { state: "unavailable" };
    const updates = vi.fn();
    render(<ReleaseBrowser query={f.query} connected onUpdates={updates} />); await open();
    await screen.findByText(/Catalog status is unavailable/);
    expect(screen.queryByText("Release available")).toBeNull();
    expect(screen.queryByRole("button", { name: "Review installation choices" })).toBeNull();
  });
  it("links a consumed version to its existing repository without making a version change", async () => {
    const f = releaseJourneyFixture("blocked"), openRepository = vi.fn();
    Object.assign(f.journey.build, { reasonCode: "version_prepare_required", reason: "This version was already released from another revision.", actions: [] });
    render(<ReleaseBrowser query={f.query} connected onOpenRepository={openRepository} />); await open();
    await screen.findByText(/Prepare a normal version change/);
    await press("Open repository");
    expect(openRepository).toHaveBeenCalledWith("source-package"); expect(writes(f)).toEqual([]);
  });
  it("uses the exact owning live run and refuses a same-ID row with different source facts", async () => {
    const f = releaseJourneyFixture("building");
    const direct = vi.spyOn(f.query, "pipelineRunForOwner");
    const view = render(<ReleaseBrowser query={f.query} connected runs={[runFromRow(releaseRun)]} runsFresh />);
    await open(); await screen.findByText("Build engine 0.25.0"); expect(direct).not.toHaveBeenCalled();
    view.rerender(<ReleaseBrowser query={f.query} connected runs={[runFromRow({ ...releaseRun, repository: "another/repo" })]} runsFresh />);
    await screen.findByText("The build details do not match this release request.");
    expect(screen.queryByText("Build engine 0.25.0")).toBeNull();
  });
  it("does not take focus or change the selected step on background progress", async () => {
    const f = releaseJourneyFixture("building");
    const view = render(<ReleaseBrowser query={f.query} connected />); await open();
    await screen.findByText("Build engine 0.25.0");
    fireEvent.click(screen.getByRole("button", { name: /^Source / }));
    await screen.findByText(/keeps its recorded source/);
    const source = screen.getByRole("button", { name: /^Source / });
    source.focus();
    expect(document.activeElement).toBe(source);
    Object.assign(f.journey.build, { phase: "built", actions: ["resume"] });
    view.rerender(<ReleaseBrowser query={f.query} connected={false} />);
    view.rerender(<ReleaseBrowser query={f.query} connected />);
    await waitFor(() => expect(f.journey.calls.filter(c => c.name === "releaseGetBuild").length).toBeGreaterThan(1));
    expect(screen.getByText(/keeps its recorded source/)).toBeTruthy();
    expect(document.activeElement).toBe(source);
    expect(writes(f)).toEqual([]);
  });
  it("holds effects while disconnected or a status read is unavailable", async () => {
    const f = releaseJourneyFixture(), view = render(<ReleaseBrowser query={f.query} connected />);
    await open(); await screen.findByRole("button", { name: "Start release build" });
    view.rerender(<ReleaseBrowser query={f.query} connected={false} />);
    expect(screen.queryByRole("button", { name: "Start release build" })).toBeNull();
    f.journey.failedGet = true;
    view.rerender(<ReleaseBrowser query={f.query} connected />);
    await screen.findByText("Release progress unavailable");
    expect(screen.queryByRole("button", { name: "Start release build" })).toBeNull(); expect(writes(f)).toEqual([]);
  });
  it("does not derive permission from a prepared phase without a native Start action", async () => {
    const f = releaseJourneyFixture(); f.journey.build.actions = [];
    render(<ReleaseBrowser query={f.query} connected />); await open();
    await screen.findByText(/Starting the build creates or verifies/);
    expect(screen.queryByRole("button", { name: "Start release build" })).toBeNull();
    expect(f.journey.calls.some(c => c.name === "releaseObserveBuild")).toBe(false);
    expect(writes(f)).toEqual([]);
  });
});

describe("release progress boundary", () => {
  it("refuses foreign identities, unknown actions, conflicting lists and unbound sealed catalogs", () => {
    const { journey } = releaseJourneyFixture();
    expect(() => readReleaseBuild(releaseResult(journey.build), "another-request")).toThrow(/different/);
    expect(() => readReleaseBuild(releaseResult({ ...journey.build, actions: ["install"] }))).toThrow(/unsupported/);
    expect(() => readReleaseBuilds(releaseResult({ intents: [journey.build, journey.build] }))).toThrow(/conflicting/);
    expect(() => readReleaseBuild(releaseResult({ ...journey.build, catalog: { state: "sealed", digest: `sha256:${"7".repeat(64)}` } }))).toThrow(/approval/);
    expect(readReleaseBuild(releaseResult({ ...journey.build, catalog: undefined })).catalog.state).toBe("unavailable");
  });
  it("reads how a source was chosen and refuses a history that contradicts it", () => {
    const f = releaseJourneyFixture(); f.skipped();
    const chosen = readReleaseBuild(releaseResult(f.journey.build));
    expect(chosen).toMatchObject({ branch: "main", headSha: "e".repeat(40), lookBack: 20 });
    expect(chosen.newer.map(c => c.state)).toEqual(["pending", "failed", "cancelled", "missing"]);
    const older: Record<string, unknown> = { ...f.journey.build };
    for (const key of ["branch", "headSha", "lookBack", "newer"]) delete older[key];
    expect(readReleaseBuild(releaseResult(older))).toMatchObject({ branch: "", headSha: "", lookBack: 0, newer: [] });
    const newer = f.journey.build.newer;
    for (const broken of [
      { newer: [{ ...newer[0]!, state: "success" }, ...newer.slice(1)] },
      { newer: [{ ...newer[0]!, state: "toString" }, ...newer.slice(1)] },
      { newer: [...newer, { ...newer[1]! }] },
      { newer: [...newer.slice(0, 3), { ...newer[3]!, sha: "d".repeat(40) }] },
      { newer: [...newer.slice(0, 3), { ...newer[3]!, sha: "HEAD" }] },
      { newer: newer.slice(1) },
      { newer: [] },
      { headSha: "" },
      { headSha: "d".repeat(40), newer: [{ ...newer[0]!, sha: "d".repeat(40) }] },
      { lookBack: 3 },
      { lookBack: 4 },
      { lookBack: 2.5 },
    ]) expect(() => readReleaseBuild(releaseResult({ ...f.journey.build, ...broken }))).toThrow(/newer commits/);
  });
  it("tells a source check that found nothing to release from a lost reply", () => {
    const sentence = "None of the newest 20 commits on main has passed its required checks: 20 untested. Check the source again once a commit's checks pass.";
    expect(untestedSource(new Error(`releasePrepareBuild: MemQL engine failed to execute query. Details: release_source_untested: ${sentence}`))).toBe(sentence);
    expect(untestedSource(new Error("Source check reply was lost"))).toBeNull();
    expect(untestedSource(new Error("not_release_source_untested: a different code"))).toBeNull();
  });
  it("preserves assembly-only profiles and refuses ambiguous build inputs", () => {
    const f = releaseJourneyFixture();
    expect(readProfiles(releaseResult({ profiles: [{ ...f.state.profiles[0]!, build: undefined }] }))[0]!.build).toBeUndefined();
    expect(() => readProfiles(releaseResult({ profiles: [{ ...f.state.profiles[0]!, build: { component: "cockpit", run: "release", branch: "main" } }] }))).toThrow(/conflicting/);
  });
});
