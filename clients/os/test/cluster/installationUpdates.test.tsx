import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Result } from "@znasllc-io/memql-sdk-core/client";
import { InstallationUpdates, readInstallationStatus } from "../../src/apps/cluster/updates/InstallationUpdates";
import { readTheBar, useFakeDate } from "../readTheBar";
import { readInstallationProposal } from "../../src/apps/cluster/updates/InstallationProposals";

const candidate = { candidateId: "sha256:release", catalogDigest: "sha256:catalog", overlayRevision: "a".repeat(40), label: "MemQL 1.2" };
const plan = { ...candidate, installationId: "installation-one", preparationId: "preparation-one", planId: "plan-one", expectedCurrentRevision: "b".repeat(40), artifactExpiresAt: "2026-12-22T10:00:00Z", state: "prepared", canManage: true };
function answer(value: object): Result { return new Result({ bundle: { nodes: [{ id: "answer", payload: value }] } } as never); }
function fixture(over: { unavailable?: boolean; uncertain?: boolean } = {}) {
  let active: typeof plan | null = null;
  const executeNamed = vi.fn(async (name: string) => {
    if (name === "releaseInstallationStatus") {
      if (over.unavailable) throw new Error("Installation delivery is not configured.");
      return answer({ installationId: "installation-one", currentRevision: "b".repeat(40), choices: [candidate], active });
    }
    if (name === "releasePrepareInstallation") { active = plan; if (over.uncertain) throw new Error("Connection interrupted after preparing."); return answer(active); }
    if (name === "releaseApplyInstallation") { active = { ...plan, state: "applying" }; return answer(active); }
    throw new Error("Unexpected operation");
  });
  return { executeNamed };
}

describe("installation release selection", () => {
  useFakeDate();
  it("keeps preparation and apply separate and carries only trusted selection facts", async () => {
    const query = fixture(); render(<InstallationUpdates query={query} connected />);
    fireEvent.click(await screen.findByRole("button", { name: /MemQL 1.2/ }));
    expect(query.executeNamed.mock.calls.map(c => c[0])).toEqual(["releaseInstallationStatus"]);
    fireEvent.click(screen.getByRole("button", { name: "Prepare installation plan" }));
    await screen.findByRole("button", { name: "Apply selected release" });
    expect(query.executeNamed.mock.calls.map(c => c[0])).not.toContain("releaseApplyInstallation");
    const call = (query.executeNamed.mock.calls as unknown as string[][]).find(c => c[0] === "releasePrepareInstallation")?.[1] ?? "";
    expect(call).toContain(candidate.overlayRevision); expect(call).toContain(candidate.catalogDigest);
    expect(call).not.toMatch(/endpoint|approved|approvalRequired/);
    fireEvent.click(screen.getByRole("button", { name: "Apply selected release" }));
    await screen.findByText(/selected release is being applied/);
    expect(query.executeNamed.mock.calls.filter(c => c[0] === "releaseApplyInstallation")).toHaveLength(1);
    expect(screen.queryByRole("button", { name: "Apply selected release" })).toBeNull();
  });

  it("reads the saved plan after an uncertain response without repeating preparation", async () => {
    const query = fixture({ uncertain: true }); render(<InstallationUpdates query={query} connected />);
    fireEvent.click(await screen.findByRole("button", { name: /MemQL 1.2/ }));
    fireEvent.click(screen.getByRole("button", { name: "Prepare installation plan" }));
    await screen.findByText("The operation could not be confirmed.");
    await waitFor(() => expect(query.executeNamed.mock.calls.filter(c => c[0] === "releaseInstallationStatus").length).toBeGreaterThan(1));
    expect(query.executeNamed.mock.calls.filter(c => c[0] === "releasePrepareInstallation")).toHaveLength(1);
    expect(screen.queryByRole("button", { name: "Apply selected release" })).toBeNull();
  });

  it("keeps unavailable configuration distinct from no release choices", async () => {
    render(<InstallationUpdates query={fixture({ unavailable: true })} connected />);
    await screen.findByText("Installation updates are unavailable.");
    expect(screen.queryByText("No release choices are configured for this installation.")).toBeNull();
    expect(() => readInstallationStatus(answer({ installationId: "x", currentRevision: "y" }))).toThrow(/choices/);
  });

  it("explains the observed missing setup without exposing engine details by default, then recovers through a read", async () => {
    const unavailable = "releaseInstallationStatus: MemQL engine failed to execute query. Details: installation selection is not configured";
    let configured = false;
    const executeNamed = vi.fn(async () => {
      if (!configured) throw new Error(unavailable);
      return answer({ installationId: "installation-one", currentRevision: "b".repeat(40), choices: [candidate], active: null });
    });
    render(<InstallationUpdates query={{ executeNamed }} connected />);
    expect(await screen.findByText("Installation updates need setup.")).toBeTruthy();
    expect(screen.getByText("A cluster owner must configure this installation's release updates before a release can be selected here.")).toBeTruthy();
    const detail = screen.getByText(unavailable).closest("details");
    expect(detail).not.toBeNull();
    expect(detail?.open).toBe(false);
    expect(screen.queryByRole("button", { name: "Prepare installation plan" })).toBeNull();
    configured = true;
    fireEvent.click(screen.getByRole("button", { name: "Check again" }));
    expect(await screen.findByRole("button", { name: /MemQL 1.2/ })).toBeTruthy();
    expect(screen.queryByText("Installation updates need setup.")).toBeNull();
    expect((executeNamed.mock.calls as unknown as string[][]).every(call => call[0] === "releaseInstallationStatus")).toBe(true);
  });

  it("preserves an unexpected status failure and its retry instead of labelling it missing setup", async () => {
    const failure = "The release service could not read its database.";
    const executeNamed = vi.fn(async () => { throw new Error(failure); });
    render(<InstallationUpdates query={{ executeNamed }} connected />);
    expect(await screen.findByText("Installation updates are unavailable.")).toBeTruthy();
    expect(screen.getByText(failure).closest("details")).toBeNull();
    expect(screen.queryByText("Installation updates need setup.")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Check again" }));
    await waitFor(() => expect(executeNamed).toHaveBeenCalledTimes(2));
  });

  it("requires a reviewed proposal before a candidate becomes selectable and never applies from proposal progress", async () => {
    let proposal: Record<string, unknown> | null = null;
    let merged = false;
    const executeNamed = vi.fn(async (name: string) => {
      if (name === "releaseInstallationStatus") return answer({ installationId: "installation-one", currentRevision: "b".repeat(40), proposalBaseRevision: "c".repeat(40), choices: merged ? [candidate] : [], proposals: proposal ? [proposal] : [], active: null });
      if (name === "releaseProposeInstallation") { proposal = { proposalId: "proposal-one", installationId: "installation-one", candidateId: candidate.candidateId, catalogDigest: candidate.catalogDigest, state: "proposed", pullRequestURL: "https://github.com/example/installation/pull/12" }; return answer(proposal); }
      if (name === "releaseObserveInstallationProposal") { merged = true; proposal = { ...proposal, state: "merged", overlayRevision: candidate.overlayRevision }; return answer(proposal); }
      throw new Error("No installation action was requested");
    });
    render(<InstallationUpdates query={{ executeNamed }} connected proposedRelease={candidate} />);
    fireEvent.click(await screen.findByRole("button", { name: "Prepare update pull request" }));
    await screen.findByRole("link", { name: "Review update pull request" });
    expect(screen.queryByRole("button", { name: /MemQL 1.2/ })).toBeNull();
    const sent = (executeNamed.mock.calls as unknown as string[][]).find(c => c[0] === "releaseProposeInstallation")?.[1] ?? "";
    expect(sent).toContain('expectedBaseRevision: "' + "c".repeat(40));
    expect(sent).not.toMatch(/endpoint|approved|overlayRevision/);
    await waitFor(() => expect((screen.getByRole("button", { name: "Check proposal progress" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(screen.getByRole("button", { name: "Check proposal progress" }));
    await screen.findByRole("button", { name: /MemQL 1.2/ });
    expect(executeNamed.mock.calls.some(c => ["releasePrepareInstallation", "releaseApplyInstallation"].includes(c[0]))).toBe(false);
  });

  it("shows completed verification from durable history and does not confuse controller health with completion", async () => {
    const executeNamed = vi.fn(async () => answer({ installationId: "installation-one", currentRevision: candidate.overlayRevision, choices: [], active: { ...plan, state: "applying", observation: { healthy: true, synced: true } } }));
    const view = render(<InstallationUpdates query={{ executeNamed }} connected />);
    await screen.findByText(/release is being applied/);
    expect(screen.queryByText("The selected release is installed and verified.")).toBeNull();
    view.unmount();
    render(<InstallationUpdates query={{ executeNamed: vi.fn(async () => answer({ installationId: "installation-one", currentRevision: candidate.overlayRevision, choices: [], active: null, latest: { ...plan, state: "complete" } })) }} connected />);
    await screen.findByText("The selected release is installed and verified.");
    expect(screen.queryByRole("button", { name: "Apply selected release" })).toBeNull();
    expect(() => readInstallationProposal({ state: "merged" })).toThrow(/fully reported/);
  });
  it("discovers an interrupted preparation after remount and resumes its durable identity without applying", async () => {
    const pending = { ...candidate, installationId: "installation-one", preparationId: "saved-preparation", requestId: "original-request", expectedCurrentRevision: "b".repeat(40), state: "preparing", canResume: true };
    let resumed = false;
    const executeNamed = vi.fn(async (name: string) => {
      if (name === "releaseInstallationStatus") return answer({ installationId: "installation-one", currentRevision: "b".repeat(40), choices: [candidate], preparation: resumed ? null : pending, active: resumed ? plan : null });
      if (name === "releaseResumeInstallation") { resumed = true; return answer(plan); }
      throw new Error("Unexpected installation write");
    });
    const first = render(<InstallationUpdates query={{ executeNamed }} connected />);
    await screen.findByRole("button", { name: "Resume preparation" });
    first.unmount();
    render(<InstallationUpdates query={{ executeNamed }} connected />);
    expect(screen.queryByRole("button", { name: "Prepare installation plan" })).toBeNull();
    fireEvent.click(await screen.findByRole("button", { name: "Resume preparation" }));
    await screen.findByRole("button", { name: "Apply selected release" });
    const sent = (executeNamed.mock.calls as unknown as string[][]).find(c => c[0] === "releaseResumeInstallation")?.[1] ?? "";
    expect(sent).toContain('preparationId: "saved-preparation"');
    expect(sent).not.toMatch(/requestId|candidateId|overlayRevision/);
    expect(executeNamed.mock.calls.some(c => ["releasePrepareInstallation", "releaseApplyInstallation"].includes(c[0]))).toBe(false);
  });

  it("requires explicit cancellation before selecting another release", async () => {
    let active: typeof plan | null = plan;
    const executeNamed = vi.fn(async (name: string) => {
      if (name === "releaseInstallationStatus") return answer({ installationId: "installation-one", currentRevision: "b".repeat(40), choices: [candidate], active });
      if (name === "releaseCancelInstallation") { active = null; return answer({ ...plan, state: "cancelled" }); }
      throw new Error("Unexpected installation write");
    });
    render(<InstallationUpdates query={{ executeNamed }} connected />);
    fireEvent.click(await screen.findByRole("button", { name: "Choose a different release" }));
    expect(executeNamed.mock.calls.some(c => c[0] === "releaseCancelInstallation")).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "Cancel installation plan" }));
    await screen.findByRole("button", { name: /MemQL 1.2/ });
    expect(executeNamed.mock.calls.filter(c => c[0] === "releaseCancelInstallation")).toHaveLength(1);
    expect(executeNamed.mock.calls.some(c => c[0] === "releaseApplyInstallation")).toBe(false);
  });

  it("does not offer another operator's preparation or plan controls", async () => {
    const executeNamed = vi.fn(async () => answer({ installationId: "installation-one", currentRevision: "b".repeat(40), choices: [candidate], active: { ...plan, canManage: false } }));
    render(<InstallationUpdates query={{ executeNamed }} connected />);
    await screen.findByText("The operator who prepared this plan must apply or cancel it.");
    expect(screen.queryByRole("button", { name: "Apply selected release" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Choose a different release" })).toBeNull();
  });

  it.each([
    ["preparing", "Resume preparation", "releaseResumeInstallation"],
    ["prepared", "Apply selected release", "releaseApplyInstallation"],
    ["prepared", "Cancel installation plan", "releaseCancelInstallation"],
    ["applying", "Check progress", "releaseObserveInstallation"],
  ])("keeps %s recovery usable during optional proposal and inventory outages (%s)", async (state, action, operation) => {
    const pending = { ...candidate, installationId: "installation-one", preparationId: "saved-preparation", requestId: "original-request", expectedCurrentRevision: "b".repeat(40), state: "preparing", canResume: true };
    const executeNamed = vi.fn(async (name: string) => {
      if (name === "releaseInstallationStatus") return answer({ installationId: "installation-one", currentRevision: "b".repeat(40), choices: [], choicesWarning: "Reviewed release choices could not be read for this installation.", proposalWarning: "The installation repository connection is unavailable.", proposals: [{ proposalId: "proposal-one", installationId: "installation-one", candidateId: candidate.candidateId, catalogDigest: candidate.catalogDigest, state: "proposed", pullRequestURL: "https://github.com/example/installation/pull/12" }], preparation: state === "preparing" ? pending : null, active: state === "preparing" ? null : { ...plan, state } });
      if (name === operation) return answer({ ...plan, state: operation === "releaseCancelInstallation" ? "cancelled" : "prepared" });
      throw new Error("Unexpected operation during repository outage");
    });
    render(<InstallationUpdates query={{ executeNamed }} connected proposedRelease={candidate} />);
    await screen.findByText("Update proposals are unavailable.");
    expect(screen.queryByText("Installation updates are unavailable.")).toBeNull();
    expect(screen.queryByText("Update proposals are not configured for this installation.")).toBeNull();
    expect(screen.queryByText(/No reviewed release is ready/)).toBeNull();
    expect((screen.getByRole("button", { name: "Check proposal progress" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByRole("link", { name: "Review update pull request" }).getAttribute("href")).toContain("/pull/12");
    if (operation === "releaseCancelInstallation") fireEvent.click(screen.getByRole("button", { name: "Choose a different release" }));
    const button = screen.getByRole("button", { name: action });
    expect((button as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(button);
    await waitFor(() => expect(executeNamed.mock.calls.filter(c => c[0] === operation)).toHaveLength(1));
    expect(executeNamed.mock.calls.some(c => c[0] === "releaseProposeInstallation" || c[0] === "releaseObserveInstallationProposal")).toBe(false);
  });

});


describe("selected installation restoration", () => {
  useFakeDate();
  const members = [
    { key: "engine", name: "Engine", namespace: "argocd", currentRevision: "a".repeat(40), started: true, observation: { operationPending: false, revisionObserved: true, synced: true, healthy: true } },
    { key: "product", name: "Workspace", namespace: "argocd", currentRevision: "b".repeat(40), started: true, observation: { operationPending: true, revisionObserved: false, synced: false, healthy: false } },
  ];
  function restoreFixture(state = "applying", lostReply = false) {
    let active: Record<string, unknown> | null = state === "complete" ? null : { ...plan, state, canRollback: true, applications: members };
    let latest: Record<string, unknown> | null = state === "complete" ? { ...plan, state, canRollback: true } : null;
    let currentRevision = state === "complete" ? candidate.overlayRevision : "";
    const executeNamed = vi.fn(async (name: string) => {
      if (name === "releaseInstallationStatus") return answer({ installationId: plan.installationId, currentRevision, applications: members, choices: [candidate], active, latest });
      if (name === "releaseRollbackInstallation") {
        active = { ...plan, state: "applying", direction: "rollback", canRollback: true, applications: members };
        if (lostReply) { lostReply = false; throw new Error("Connection interrupted after restoration was recorded."); }
        return answer(active);
      }
      if (name === "releaseObserveInstallation") {
        latest = { ...active, state: "rolled_back", canRollback: false };
        active = null; currentRevision = plan.expectedCurrentRevision;
        return answer(latest);
      }
      throw new Error("Unexpected installation effect");
    });
    return { executeNamed };
  }
  async function confirmRestore(query: ReturnType<typeof restoreFixture>) {
    fireEvent.click(await screen.findByRole("button", { name: "Restore previous release" }));
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Keep current release" }));
    fireEvent.click(screen.getByRole("button", { name: "Restore previous release" }));
    expect(query.executeNamed.mock.calls.some(c => c[0] === "releaseRollbackInstallation")).toBe(false);
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Restore previous release" }));
  }

  it.each(["applying", "complete"])("requires deliberate confirmation for %s and completes only from the native restoration receipt", async state => {
    const query = restoreFixture(state);
    render(<InstallationUpdates query={query} connected />);
    await confirmRestore(query);
    await screen.findByText(/previous release is being restored/);
    expect(screen.queryByText("The previous release is restored and verified.")).toBeNull();
    expect(screen.queryByText("The selected release is installed and verified.")).toBeNull();
    expect(screen.queryByRole("button", { name: "Apply selected release" })).toBeNull();
    const sent = (query.executeNamed.mock.calls as unknown as string[][]).find(c => c[0] === "releaseRollbackInstallation")?.[1];
    expect(sent).toBe('builtin releaseRollbackInstallation(installationId: "installation-one", planId: "plan-one")');
    expect(screen.getByText("Restoring configuration")).toBeTruthy();
    await waitFor(() => expect((screen.getByRole("button", { name: "Check progress" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(screen.getByRole("button", { name: "Check progress" }));
    await screen.findByText("The previous release is restored and verified.");
    expect(screen.queryByRole("button", { name: "Resume restoration" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Restore previous release" })).toBeNull();
    expect(query.executeNamed.mock.calls.filter(c => c[0] === "releaseRollbackInstallation")).toHaveLength(1);
    expect(query.executeNamed.mock.calls.some(c => ["releaseApplyInstallation", "releasePrepareInstallation"].includes(c[0]))).toBe(false);
  });

  it("reads interrupted restoration after remount and resumes only on an explicit action", async () => {
    const query = restoreFixture("applying", true);
    const initial = render(<InstallationUpdates query={query} connected />);
    await confirmRestore(query);
    await screen.findByText("The operation could not be confirmed.");
    expect(query.executeNamed.mock.calls.filter(c => c[0] === "releaseRollbackInstallation")).toHaveLength(1);
    expect(screen.queryByRole("button", { name: "Resume restoration" })).toBeNull();
    initial.unmount();
    render(<InstallationUpdates query={query} connected />);
    const resume = await screen.findByRole("button", { name: "Resume restoration" });
    expect(query.executeNamed.mock.calls.filter(c => c[0] === "releaseRollbackInstallation")).toHaveLength(1);
    fireEvent.click(resume);
    await waitFor(() => expect(query.executeNamed.mock.calls.filter(c => c[0] === "releaseRollbackInstallation")).toHaveLength(2));
    const writes = (query.executeNamed.mock.calls as unknown as string[][]).filter(c => c[0] === "releaseRollbackInstallation");
    expect(writes[1]?.[1]).toBe(writes[0]?.[1]);
  });

  it.each([
    { canManage: false, canRollback: true },
    { canManage: true, canRollback: false },
    { canManage: true },
  ])("requires server rollback eligibility and the original operator: %j", async eligibility => {
    const executeNamed = vi.fn(async () => answer({ installationId: plan.installationId, currentRevision: candidate.overlayRevision, choices: [candidate], latest: { ...plan, state: "complete", ...eligibility } }));
    render(<InstallationUpdates query={{ executeNamed }} connected />);
    await screen.findByText("The selected release is installed and verified.");
    expect(screen.queryByRole("button", { name: "Restore previous release" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Resume restoration" })).toBeNull();
  });

  it("does not reuse confirmation when a fresh read withdraws eligibility", async () => {
    let allowed = true;
    const executeNamed = vi.fn(async (name: string) => {
      if (name !== "releaseInstallationStatus") throw new Error("No effect expected");
      return answer({ installationId: plan.installationId, currentRevision: candidate.overlayRevision, choices: [], active: { ...plan, state: "applying", canRollback: allowed } });
    });
    render(<InstallationUpdates query={{ executeNamed }} connected />);
    fireEvent.click(await screen.findByRole("button", { name: "Restore previous release" }));
    await screen.findByText("Restore the previous release?");
    allowed = false;
    fireEvent.click(screen.getByRole("button", { name: "Read saved plan" }));
    await waitFor(() => expect(screen.queryByText("Restore the previous release?")).toBeNull());
    expect(executeNamed.mock.calls.every(c => c[0] === "releaseInstallationStatus")).toBe(true);
  });

  it("keeps mixed application revisions readable and never prepares against an invented common revision", async () => {
    const executeNamed = vi.fn(async () => answer({ installationId: plan.installationId, currentRevision: "", applications: members, choices: [candidate] }));
    render(<InstallationUpdates query={{ executeNamed }} connected />);
    await screen.findByText("The applications are at different revisions.");
    expect(screen.queryByText("Installation updates are unavailable.")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /MemQL 1.2/ }));
    expect(screen.queryByRole("button", { name: "Prepare installation plan" })).toBeNull();
    expect(executeNamed).toHaveBeenCalledTimes(1);
    expect(() => readInstallationStatus(answer({ installationId: plan.installationId, currentRevision: "", choices: [] }))).toThrow(/incomplete/);
  });
});
