import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { readTheBar } from "../readTheBar";
import { ReleaseBrowser } from "../../src/apps/cluster/releases/ReleasesSection";
import { fileReleaseFixture, npmReleaseFixture, releaseFixture, releaseId, releaseResult, candidateRecord } from "./releaseFixtures";

async function open() { fireEvent.click(await screen.findByRole("button", { name: "Review engine 0.25.0" })); await screen.findByText("Versions"); }
async function destination() { fireEvent.click(await screen.findByRole("button", { name: "Review destination rehearsal" })); await screen.findByText("Artifact digest"); }
/** Presses a bar act once the bar has been read: its acts change as each read lands, and a bar that has just changed takes no press for a moment. */
async function pressBar(label: string) { await screen.findByRole("button", { name: label }); await readTheBar(); fireEvent.click(screen.getByRole("button", { name: label })); }
/** Asks, reads the question, and confirms: the confirmation stands where the ask did, under its label. */
async function twice(label: string) { await pressBar(label); await pressBar(label); }

// A fake Date, so reading a bar costs nothing: the release reads poll on real timers.
beforeEach(() => { vi.useFakeTimers({ toFake: ["Date"] }); });
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

describe("release review", () => {
  it.each(["sealed", "unavailable"])("reads archived release history with %s catalog evidence without offering effects", async (catalog) => {
    const f = releaseFixture("archived");
    f.state.record.catalog = catalog === "sealed" ? { state: "sealed", approvalId: "approval-exact", catalogDigest: `sha256:${"7".repeat(64)}` } : { state: "unavailable" };
    // Empty histories must not accidentally make an archived record retireable.
    render(<ReleaseBrowser query={f.query} connected />);
    await screen.findByText("Archived");
    await open();
    await screen.findByText(/build inputs are archived/);
    if (catalog === "sealed") await screen.findByText("In release catalog");
    else expect(screen.queryByText("In release catalog")).toBeNull();
    for (const name of ["Approve release", "Retire release", "Resume verification", "Add to release catalog"]) expect(screen.queryByRole("button", { name })).toBeNull();
    await destination();
    expect(screen.queryByRole("button", { name: /Publish artifact|Reconcile publication/ })).toBeNull();
    expect(f.state.calls.some((c) => ["approve", "retire", "prepare", "publish", "seal"].includes(c.kind))).toBe(false);
  });
  it("keeps candidate and source references optional while preserving exact copying and approval", async () => {
    const f = releaseFixture();
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    render(<ReleaseBrowser query={f.query} connected />);
    await open();
    const details = screen.getByText(releaseId).closest("details")!;
    expect(details.open).toBe(false);
    expect(screen.getByText("d".repeat(40)).closest("details")).toBe(details);
    expect(screen.getByRole("heading", { name: "engine 0.25.0" }).closest("details")).toBeNull();
    fireEvent.click(screen.getByText("Release references"));
    expect(details.open).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Copy Candidate" }));
    await screen.findByRole("button", { name: "Copied Candidate" });
    expect(writeText).toHaveBeenLastCalledWith(releaseId);
    fireEvent.click(screen.getByRole("button", { name: "Copy engine revision" }));
    await screen.findByRole("button", { name: "Copied engine revision" });
    expect(writeText).toHaveBeenLastCalledWith("d".repeat(40));
    await twice("Approve release");
    await screen.findByText("Approved");
    expect(f.state.calls.filter((c) => c.kind === "approve")).toEqual([{ kind: "approve", args: { candidateId: releaseId } }]);
    expect(f.state.calls.some((c) => ["publish", "seal"].includes(c.kind))).toBe(false);
  });
  it("shows the resolved image tag before its explicit publication", async () => {
    const f = releaseFixture("approved");
    f.state.targets[0] = { ...f.state.targets[0]!, tag: "v0.25.0" };
    render(<ReleaseBrowser query={f.query} connected />);
    await open(); await destination();
    expect(screen.getByText("Image tag")).toBeTruthy();
    expect(screen.getByText("v0.25.0")).toBeTruthy();
    await pressBar("Publish artifact");
    expect(screen.getByText(/to https:\/\/registry.example.test\/memql\/bff as v0.25.0/)).toBeTruthy();
    expect(f.state.calls.some((c) => c.kind === "publish")).toBe(false);
  });
  it("adds verified publication to the catalog only after a separate confirmation", async () => {
    const f = releaseFixture("approved");
    await f.query.releasePublishCandidate({ candidateId: releaseId, approvalId: "approval-exact", targetId: "rehearsal", component: "engine", artifact: "bff" });
    render(<ReleaseBrowser query={f.query} connected />);
    await open();
    await pressBar("Add to release catalog");
    expect(screen.getByText(/This does not install it/)).toBeTruthy();
    expect(f.state.calls.some((c) => c.kind === "seal")).toBe(false);
    await pressBar("Add to release catalog");
    await screen.findByText("In release catalog");
    expect(f.state.calls.filter((c) => c.kind === "seal")).toEqual([{ kind: "seal", args: { candidateId: releaseId, approvalId: "approval-exact" } }]);
    expect(screen.queryByRole("button", { name: "Add to release catalog" })).toBeNull();
    expect(screen.getByText(/Installation is a separate choice/)).toBeTruthy();
  });
  it("does not offer catalog completion for uploaded files until their release is public", async () => {
    const f = fileReleaseFixture();
    await f.query.releaseCreateCandidateDraft({ candidateId: releaseId, approvalId: "approval-exact", targetId: "rehearsal" });
    await f.query.releasePublishCandidate({ candidateId: releaseId, approvalId: "approval-exact", targetId: "rehearsal", component: "engine", artifact: "bff" });
    const view = render(<ReleaseBrowser query={f.query} connected />);
    await open();
    expect(screen.queryByRole("button", { name: "Add to release catalog" })).toBeNull();
    view.rerender(<ReleaseBrowser query={f.query} connected={false} />);
    await f.query.releasePromoteCandidateDraft({ candidateId: releaseId, approvalId: "approval-exact", targetId: "rehearsal" });
    view.rerender(<ReleaseBrowser query={f.query} connected />);
    await screen.findByRole("button", { name: "Add to release catalog" });
    expect(f.state.calls.some((c) => c.kind === "seal")).toBe(false);
  });
  it("does not claim catalog readiness for a candidate containing installation effects", async () => {
    const f = releaseFixture("approved");
    await f.query.releasePublishCandidate({ candidateId: releaseId, approvalId: "approval-exact", targetId: "rehearsal", component: "engine", artifact: "bff" });
    // The native public-catalog contract accepts publication destinations only.
    f.state.record.manifest.destinations.push(Object.assign({ ...f.state.record.manifest.destinations[0]! }, { targetId: "installation", operation: "install" }));
    f.state.targets.push({ ...f.state.targets[0]!, targetId: "installation", operation: "install" });
    render(<ReleaseBrowser query={f.query} connected />);
    await open();
    expect(screen.queryByRole("button", { name: "Add to release catalog" })).toBeNull();
    expect(f.state.calls.some((c) => c.kind === "seal")).toBe(false);
  });
  it("recovers a lost catalog reply by reading durable state without another write", async () => {
    const f = releaseFixture("approved"); f.state.lostSealReply = true;
    await f.query.releasePublishCandidate({ candidateId: releaseId, approvalId: "approval-exact", targetId: "rehearsal", component: "engine", artifact: "bff" });
    const view = render(<ReleaseBrowser query={f.query} connected />);
    await open(); await twice("Add to release catalog");
    await screen.findByText("In release catalog");
    view.unmount();
    render(<ReleaseBrowser query={f.query} connected />);
    await open(); await screen.findByText("In release catalog");
    expect(f.state.calls.filter((c) => c.kind === "seal")).toHaveLength(1);
  });
  it("preserves catalog signing failure and distinguishes unavailable state from an absent record", async () => {
    const f = releaseFixture("approved"); f.state.failedSeal = true;
    await f.query.releasePublishCandidate({ candidateId: releaseId, approvalId: "approval-exact", targetId: "rehearsal", component: "engine", artifact: "bff" });
    const view = render(<ReleaseBrowser query={f.query} connected />);
    await open(); await twice("Add to release catalog");
    await screen.findByText("Catalog signing is unavailable");
    expect(screen.queryByText("In release catalog")).toBeNull();
    view.rerender(<ReleaseBrowser query={f.query} connected={false} />);
    f.state.record.catalog = { state: "unavailable" };
    view.rerender(<ReleaseBrowser query={f.query} connected />);
    await screen.findByRole("button", { name: "Read status again" });
    expect(screen.queryByRole("button", { name: "Add to release catalog" })).toBeNull();
    expect(f.state.calls.filter((c) => c.kind === "seal")).toHaveLength(1);
  });
  it("refuses a catalog record for another approval and destinations for another candidate", async () => {
    const f = releaseFixture("approved");
    f.state.record.catalog = { state: "sealed", approvalId: "different-approval", catalogDigest: `sha256:${"7".repeat(64)}` };
    f.query.releaseCandidateTargets = async () => releaseResult({ candidateId: `sha256:${"9".repeat(64)}`, targets: f.state.targets });
    render(<ReleaseBrowser query={f.query} connected />);
    fireEvent.click(await screen.findByRole("button", { name: "Review engine 0.25.0" }));
    await screen.findByText("The catalog record does not match this release approval.");
    await screen.findByText("The cluster returned destinations for another release.");
    expect(f.state.calls.some((c) => ["seal", "approve", "publish"].includes(c.kind))).toBe(false);
  });
  it("reviews the exact npm package, version and tag before publication without a draft", async () => {
    const f = npmReleaseFixture();
    render(<ReleaseBrowser query={f.query} connected />);
    await open(); await destination();
    expect(screen.getByText("Package version")).toBeTruthy();
    expect(screen.getByText("Publish as")).toBeTruthy();
    await pressBar("Publish package");
    expect(screen.getByText(/Publish @acme\/sdk@0.25.0 to https:\/\/registry.example.test as latest/)).toBeTruthy();
    expect(f.state.calls.some((c) => c.kind === "publish")).toBe(false);
    await pressBar("Publish package");
    await screen.findByText("Published and verified");
    expect(screen.queryByText("File uploaded and verified")).toBeNull();
    expect(f.state.calls.some((c) => c.kind === "draft" || c.kind === "promote")).toBe(false);
    expect(f.state.calls.filter((c) => c.kind === "publish")).toEqual([{kind:"publish", args:{candidateId:releaseId,approvalId:"approval-exact",targetId:"rehearsal",component:"engine",artifact:"bff"}}]);
  });
  it("refuses npm publication when the dist-tag cannot be reviewed", async () => {
    const f = npmReleaseFixture(); delete f.state.targets[0]!.distTag;
    render(<ReleaseBrowser query={f.query} connected />);
    await open(); await destination();
    await screen.findByText("The cluster returned an incomplete release record.");
    expect(screen.queryByRole("button", {name:"Publish package"})).toBeNull();
    expect(f.state.calls.some((c) => c.kind === "publish")).toBe(false);
  });
  it("explains that a file upload remains in an existing draft release", async () => {
    const f = releaseFixture("approved");
    Object.assign(f.state.record.manifest.components[0]!.artifacts[0]!, { kind: "file", imageDigest: "", platform: "darwin/arm64" });
    f.state.targets = [{ ...f.state.targets[0]!, kind: "file", releaseId: 81, tag: "v0.25.0", assetName: "MemQL.zip" }];
    render(<ReleaseBrowser query={f.query} connected />);
    await open(); await destination();
    expect(screen.getByText("Draft #81")).toBeTruthy();
    expect(screen.getByText(/Making the release public is a separate action/)).toBeTruthy();
    await pressBar("Upload file");
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
  it("a double-click on Retire release asks and does not retire", async () => {
    // [Retire release, Approve release] asks as [Cancel, Retire release]: the
    // confirmation is the danger act on the right edge -- the label just
    // pressed, at another place, and within reach of the double-click's
    // second half. The bar that changed takes no press until it is read.
    const f = releaseFixture();
    render(<ReleaseBrowser query={f.query} connected />);
    await open();
    await screen.findByRole("button", { name: "Approve release" });
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Retire release" }));
    fireEvent.click(screen.getByRole("button", { name: "Retire release" }));
    expect(screen.getByText("Retirement is permanent and releases this candidate’s artifact retention.")).toBeTruthy();
    expect(f.state.calls.some((c) => c.kind === "retire")).toBe(false);
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Retire release" }));
    await waitFor(() => expect(f.state.calls.filter((c) => c.kind === "retire")).toHaveLength(1));
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

describe("draft release lifecycle", () => {
  it("reviews deferred metadata before approval, without inventing draft zero", async () => {
    const f = fileReleaseFixture("ready");
    render(<ReleaseBrowser query={f.query} connected />);
    await open();
    expect(screen.getByText("MemQL 0.25.0")).toBeTruthy();
    expect(screen.getByText(/Verified desktop release/)).toBeTruthy();
    expect(screen.getByText("Select as latest")).toBeTruthy();
    await twice("Approve release");
    await destination();
    await screen.findByRole("button", { name: "Create draft" });
    expect(screen.queryByText(/Draft #0/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Upload file" })).toBeNull();
    expect(f.state.calls.some((c) => c.kind === "draft")).toBe(false);
  });
  it("separates draft, upload and public publication with exact approved identities", async () => {
    const f = fileReleaseFixture();
    render(<ReleaseBrowser query={f.query} connected />);
    await open(); await destination();
    await pressBar("Create draft");
    expect(screen.getByText(/The tag may trigger repository automation/)).toBeTruthy();
    await pressBar("Create draft");
    await screen.findByText("Draft #81");
    expect(f.state.calls.filter((c) => ["publish", "promote"].includes(c.kind))).toHaveLength(0);
    await twice("Upload file");
    await screen.findByRole("button", { name: "Publish release" });
    expect(f.state.calls.filter((c) => c.kind === "promote")).toHaveLength(0);
    await pressBar("Publish release");
    expect(screen.getByText(/and its uploaded files public/)).toBeTruthy();
    expect(screen.getByText(/Select as the latest release/)).toBeTruthy();
    await pressBar("Publish release");
    await screen.findByText("Release public and verified");
    expect(screen.getByText("Release #81")).toBeTruthy();
    const identity = { candidateId: releaseId, approvalId: "approval-exact", targetId: "rehearsal" };
    expect(f.state.calls.filter((c) => ["draft", "publish", "promote"].includes(c.kind))).toEqual([
      { kind: "draft", args: identity }, { kind: "publish", args: { ...identity, component: "engine", artifact: "bff" } }, { kind: "promote", args: identity },
    ]);
    expect(screen.queryByRole("button", { name: "Upload file" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Publish release" })).toBeNull();
  });
  it("keeps uncertain creation across remount, forbids retirement and only reconciles explicitly", async () => {
    const f = fileReleaseFixture(); f.state.lostDraftReply = true;
    const view = render(<ReleaseBrowser query={f.query} connected />);
    await open(); await destination(); await twice("Create draft");
    await screen.findByText("Draft creation reply was lost");
    view.unmount(); render(<ReleaseBrowser query={f.query} connected />);
    await open();
    expect(screen.queryByRole("button", { name: "Retire release" })).toBeNull();
    await destination(); await screen.findByRole("button", { name: "Reconcile draft" });
    expect(screen.queryByRole("button", { name: "Create draft" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Upload file" })).toBeNull();
    expect(f.state.calls.filter((c) => c.kind === "draft")).toHaveLength(1);
    f.state.lostDraftReply = false;
    await twice("Reconcile draft"); await screen.findByRole("button", { name: "Upload file" });
    expect(f.state.calls.filter((c) => c.kind === "draft")).toHaveLength(2);
  });
  it("does not repeat an uncertain public publication on remount", async () => {
    const f = fileReleaseFixture(); f.state.lostPromotionReply = true;
    const view = render(<ReleaseBrowser query={f.query} connected />);
    await open(); await destination(); await twice("Create draft"); await twice("Upload file"); await twice("Publish release");
    await screen.findByText("Public publication reply was lost");
    view.unmount(); render(<ReleaseBrowser query={f.query} connected />);
    await open(); await destination(); await screen.findByRole("button", { name: "Reconcile release" });
    expect(screen.queryByText("Verified when published; remote availability has not been checked again.")).toBeNull();
    expect(screen.getByText("The public release outcome has not been verified. Reconcile the recorded attempt.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Upload file" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Publish release" })).toBeNull();
    expect(f.state.calls.filter((c) => c.kind === "promote")).toHaveLength(1);
    f.state.lostPromotionReply = false;
    await twice("Reconcile release"); await screen.findByText("Release public and verified");
    expect(f.state.calls.filter((c) => c.kind === "promote")).toHaveLength(2);
  });
  it("requires every candidate destination before public publication", async () => {
    const f = fileReleaseFixture();
    const other = { ...f.state.targets[0]!, targetId: "second", artifact: "other" };
    f.state.targets.push(other);
    f.state.record.manifest.destinations.push({ ...f.state.record.manifest.destinations[0]!, targetId: "second", artifact: "other" });
    render(<ReleaseBrowser query={f.query} connected />);
    await open(); await destination(); await twice("Create draft"); await twice("Upload file");
    await screen.findByText(/Upload and verify every candidate destination/);
    expect(screen.queryByRole("button", { name: "Publish release" })).toBeNull();
  });
  it("hides writes when draft history fails, without treating it as empty", async () => {
    const f = fileReleaseFixture(); f.state.failedDraftRead = true;
    render(<ReleaseBrowser query={f.query} connected />);
    await open(); await screen.findByText("Draft history unavailable");
    expect(screen.queryByRole("button", { name: "Retire release" })).toBeNull();
    await destination();
    expect(screen.queryByRole("button", { name: "Create draft" })).toBeNull();
    f.state.failedDraftRead = false;
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    await screen.findByRole("button", { name: "Create draft" });
  });
  it("refuses a draft history belonging to a different approval", async () => {
    const f = fileReleaseFixture();
    f.state.drafts = [{ candidateId: releaseId, intentId: "intent-old", approvalId: "approval-old", state: "ready", targets: ["rehearsal"], releaseId: 81 }];
    render(<ReleaseBrowser query={f.query} connected />);
    await open(); await destination();
    await screen.findByText(/history does not match this approval/);
    expect(screen.queryByRole("button", { name: "Upload file" })).toBeNull();
  });
});
