import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Result } from "@znasllc-io/memql-sdk-core/client";
import { InstallationUpdates, readInstallationStatus } from "../../src/apps/cluster/updates/InstallationUpdates";
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
