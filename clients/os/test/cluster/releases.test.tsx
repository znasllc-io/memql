import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ReleaseBrowser } from "../../src/apps/cluster/releases/ReleasesSection";
import { releaseFixture, releaseId, releaseResult, candidateRecord } from "./releaseFixtures";

async function open() { fireEvent.click(await screen.findByRole("button", { name: "Review engine 0.25.0" })); await screen.findByText("Versions"); }
async function destination() { fireEvent.click(await screen.findByRole("button", { name: "Review destination rehearsal" })); await screen.findByText("Artifact digest"); }
async function twice(label: string) { fireEvent.click(await screen.findByRole("button", { name: label })); fireEvent.click(await screen.findByRole("button", { name: label })); }

describe("release review", () => {
  it("explains that a file upload remains in an existing draft release", async () => {
    const f = releaseFixture("approved");
    Object.assign(f.state.record.manifest.components[0]!.artifacts[0]!, { kind: "file", imageDigest: "", platform: "darwin/arm64" });
    f.state.targets = [{ ...f.state.targets[0]!, kind: "file", releaseId: 81, tag: "v0.25.0", assetName: "MemQL.zip" }];
    render(<ReleaseBrowser query={f.query} connected />);
    await open(); await destination();
    expect(screen.getByText("Draft #81")).toBeTruthy();
    expect(screen.getByText(/Making the release public is a separate action/)).toBeTruthy();
    fireEvent.click(await screen.findByRole("button", { name: "Upload file" }));
    expect(screen.getByText(/This does not make the release public/)).toBeTruthy();
    expect(f.state.calls.some((c) => c.kind === "publish")).toBe(false);
  });
  it("moves focus into a detail and restores the selected list row on return", async () => {
    const f = releaseFixture();
    render(<ReleaseBrowser query={f.query} connected />);
    await open();
    expect(document.activeElement?.tagName).toBe("H3");
    fireEvent.click(screen.getByRole("button", { name: "Back to Releases" }));
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Review engine 0.25.0" }));
  });
  it("keeps unavailable and empty lists distinct", async () => {
    const f = releaseFixture(); f.state.empty = true;
    const view = render(<ReleaseBrowser query={f.query} connected={false} />);
    expect(screen.queryByText(/No release candidates/)).toBeNull();
    expect(f.state.calls).toHaveLength(0);
    view.rerender(<ReleaseBrowser query={f.query} connected />);
    expect(await screen.findByText(/No release candidates/)).toBeTruthy();
  });
  it("requires separate review and publication actions with exact identities", async () => {
    const f = releaseFixture();
    render(<ReleaseBrowser query={f.query} connected />);
    await open();
    await twice("Approve release");
    await screen.findByText("Approved");
    expect(f.state.calls.filter((c) => c.kind === "approve")).toEqual([{ kind: "approve", args: { candidateId: releaseId } }]);
    expect(f.state.calls.some((c) => c.kind === "publish")).toBe(false);
    await destination();
    await twice("Publish artifact");
    await screen.findByText("Published and verified");
    expect(f.state.calls.filter((c) => c.kind === "publish")).toEqual([{ kind: "publish", args: { candidateId: releaseId, approvalId: "approval-exact", targetId: "rehearsal", component: "engine", artifact: "bff" } }]);
    expect(screen.queryByRole("button", { name: "Publish artifact" })).toBeNull();
  });
  it("recovers unresolved publication after remount and never retries it automatically", async () => {
    const f = releaseFixture("approved"); f.state.lostPublishReply = true;
    const view = render(<ReleaseBrowser query={f.query} connected />);
    await open(); await destination(); await twice("Publish artifact");
    await screen.findByText("Publication reply was lost");
    await screen.findByRole("button", { name: "Reconcile publication" });
    view.unmount();
    render(<ReleaseBrowser query={f.query} connected />);
    await open();
    expect(screen.queryByRole("button", { name: "Retire release" })).toBeNull();
    await destination();
    await screen.findByRole("button", { name: "Reconcile publication" });
    expect(f.state.calls.filter((c) => c.kind === "publish")).toHaveLength(1);
    f.state.lostPublishReply = false;
    await twice("Reconcile publication");
    await screen.findByText("Published and verified");
    expect(f.state.calls.filter((c) => c.kind === "publish")).toHaveLength(2);
  });
  it("keeps last content through a disconnection but hides acts until a fresh read", async () => {
    const f = releaseFixture();
    const view = render(<ReleaseBrowser query={f.query} connected />);
    await open(); await screen.findByRole("button", { name: "Approve release" });
    view.rerender(<ReleaseBrowser query={f.query} connected={false} />);
    expect(screen.getByText("Versions")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Approve release" })).toBeNull();
    let resolve!: (r: ReturnType<typeof releaseResult>) => void;
    vi.spyOn(f.query, "releaseGetCandidate").mockImplementationOnce(() => new Promise((r) => { resolve = r; }));
    view.rerender(<ReleaseBrowser query={f.query} connected />);
    expect(screen.queryByRole("button", { name: "Approve release" })).toBeNull();
    await act(async () => resolve(releaseResult(candidateRecord())));
    await screen.findByRole("button", { name: "Approve release" });
  });
  it("refuses to offer publication to changed destinations", async () => {
    const f = releaseFixture("approved"); f.state.targets = [{ ...f.state.targets[0]!, targetDigest: `sha256:${"9".repeat(64)}` }];
    render(<ReleaseBrowser query={f.query} connected />);
    await open(); await destination();
    await screen.findByText(/destination no longer matches/);
    expect(screen.queryByRole("button", { name: "Publish artifact" })).toBeNull();
  });
  it("ignores stale responses after changing the connection", async () => {
    const first = releaseFixture(), second = releaseFixture(); second.state.empty = true;
    let resolve!: (r: ReturnType<typeof releaseResult>) => void;
    first.query.releaseCandidates = () => new Promise((r) => { resolve = r; });
    const view = render(<ReleaseBrowser query={first.query} connected />);
    view.rerender(<ReleaseBrowser query={second.query} connected />);
    await screen.findByText(/No release candidates/);
    await act(async () => resolve(releaseResult({ candidates: [{ ...candidateRecord(), components: candidateRecord().manifest.components, createdAt: "2026-10-06T18:00:00Z", evidenceCount: 1 }] })));
    expect(screen.queryByRole("button", { name: "Review engine 0.25.0" })).toBeNull();
  });
  it("shows a failed history read as an error and retries only the read", async () => {
    const f = releaseFixture(); f.state.failedRead = true;
    render(<ReleaseBrowser query={f.query} connected />);
    await screen.findByText("Release journal unavailable");
    expect(screen.queryByText(/No release candidates/)).toBeNull();
    f.state.failedRead = false;
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    await screen.findByRole("button", { name: "Review engine 0.25.0" });
    expect(f.state.calls.every((c) => c.kind === "list")).toBe(true);
  });
  it("polls only visible pages without repeating writes", async () => {
    vi.useFakeTimers();
    try {
      const f = releaseFixture();
      const view = render(<ReleaseBrowser query={f.query} connected />);
      await act(async () => {});
      expect(f.state.calls.filter((c) => c.kind === "list")).toHaveLength(1);
      await act(async () => { await vi.advanceTimersByTimeAsync(20_000); });
      expect(f.state.calls.filter((c) => c.kind === "list")).toHaveLength(2);
      view.rerender(<ReleaseBrowser query={f.query} connected visible={false} />);
      await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
      expect(f.state.calls.filter((c) => c.kind === "list")).toHaveLength(2);
      view.unmount();
    } finally { vi.useRealTimers(); }
  });
  it("never displays a different candidate under the selected identity", async () => {
    const f = releaseFixture();
    f.query.releaseGetCandidate = async () => releaseResult({ ...candidateRecord(), candidateId: `sha256:${"9".repeat(64)}` });
    render(<ReleaseBrowser query={f.query} connected />);
    fireEvent.click(await screen.findByRole("button", { name: "Review engine 0.25.0" }));
    await waitFor(() => expect(screen.getByText("The cluster returned a different release.")).toBeTruthy());
    expect(screen.queryByRole("button", { name: "Approve release" })).toBeNull();
  });
});
