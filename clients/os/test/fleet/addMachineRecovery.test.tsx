import { act, cleanup, fireEvent, render, renderHook, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown, mint: vi.fn(), chat: vi.fn() }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));
vi.mock("@znasllc-io/memql-sdk-core/identity", () => ({
  createWorkerToken: (...args: unknown[]) => h.mint(...args),
  revokeWorkerToken: vi.fn(),
}));
vi.mock("@znasllc-io/memql-sdk-core/ai", () => ({ aiChat: (...args: unknown[]) => h.chat(...args) }));

const { MachinesProvider, WORKER_REGISTRATION_CONCEPT } = await import("../../src/live/machines");
const { AddMachinePage } = await import("../../src/apps/fleet/addMachine/AddMachinePage");
const { ChecksStop } = await import("../../src/apps/fleet/addMachine/stops/Checks");
const { useAddMachineFlow } = await import("../../src/apps/fleet/addMachine/useAddMachineFlow");
const { machineFromRow } = await import("../../src/apps/fleet/rows");
const { MODEL_PULL_CONCEPT } = await import("../../src/apps/fleet/machines/useModelPulls");
const { fakeConnection, machineRow, modelPullRow, withSession } = await import("./harness");

function machine(labels: Record<string, string> = {}) {
  return machineRow({
    id: "recovery-machine", identityId: "recovery-identity", name: "Recovery machine",
    lastSeenAt: new Date().toISOString(), labels,
    hardware: { runtimes: [{ name: "ollama", version: "1" }] },
  });
}

beforeEach(() => {
  h.connection = fakeConnection();
  h.mint.mockReset();
  h.mint.mockResolvedValue({ success: true, plainToken: "fixture-only", identityId: "recovery-identity" });
  h.chat.mockReset();
  h.chat.mockResolvedValue({ message: { content: "hello" } });
});
afterEach(cleanup);

it("shows a partial-start refusal while another recommended model is downloading", () => {
  render(<ChecksStop machine={machineFromRow(machine())}
    checks={[{ id: "models", name: "Models", state: "current", answer: "Pulling a-embed" }]}
    pulling={false} pullError="The text download could not start" onPullRecommended={vi.fn()} />);
  expect(screen.getByText("The text download could not start")).toBeTruthy();
  expect(screen.getByText(/Some models may already be downloading/)).toBeTruthy();
});

async function connectedFlow(connection: ReturnType<typeof fakeConnection>) {
  h.connection = connection;
  const view = renderHook(() => useAddMachineFlow(), {
    wrapper: ({ children }: { children: ReactNode }) => withSession(<MachinesProvider>{children}</MachinesProvider>),
  });
  act(() => view.result.current.start({ inference: true }));
  act(() => view.result.current.setDraft({ name: "Recovery machine" }));
  await act(async () => view.result.current.mint());
  await waitFor(() => expect(view.result.current.phase).toBe("connected"));
  return view;
}

it("the connected Back action finishes the flow and returns to Machines", async () => {
  const view = await connectedFlow(fakeConnection({ myWorkersWithStatus: [machine()] }));
  const onLeave = vi.fn();
  render(withSession(<AddMachinePage flow={view.result.current} onLeave={onLeave} />));
  fireEvent.click(screen.getByRole("button", { name: "Back to Machines" }));
  expect(onLeave).toHaveBeenCalledWith("");
  expect(view.result.current.active).toBe(false);
  expect(view.result.current.facts.cancelAsked).toBe(false);
});

it.each([false, true])("cancel offers the matching uninstall command for user-local=%s", async (userLocal) => {
  h.connection = fakeConnection();
  const view = renderHook(() => useAddMachineFlow(), {
    wrapper: ({ children }: { children: ReactNode }) => withSession(<MachinesProvider>{children}</MachinesProvider>),
  });
  act(() => view.result.current.start({}));
  act(() => view.result.current.setDraft({ name: "Recovery machine", userLocal }));
  await act(async () => view.result.current.mint());
  act(() => view.result.current.cancel());
  render(withSession(<AddMachinePage flow={view.result.current} onLeave={vi.fn()} />));
  const command = (screen.getByLabelText("the uninstall command") as HTMLInputElement).value;
  expect(command.includes("--user-local")).toBe(userLocal);
});

it("a live pull failure reaches Checks even with partial inventory, with the runtime reason and a retry", async () => {
  const pull = modelPullRow({ id: "pull-recovery", workerId: "recovery-machine", model: "z-chat", status: "running" });
  const connection = fakeConnection({ myWorkersWithStatus: [machine({ "model:a-embed": "embeddings=1" })], modelPullsForWorker: [pull] });
  const view = await connectedFlow(connection);
  await waitFor(() => expect(view.result.current.checks.find((c) => c.id === "models")?.answer).toContain("Pulling"));
  act(() => connection.subscriptions.emit(MODEL_PULL_CONCEPT,
    { ...pull, status: "failed", errorMessage: "Not enough disk space", endedAt: new Date().toISOString() }, "NODE_UPDATED"));
  await waitFor(() => {
    const check = view.result.current.checks.find((c) => c.id === "models");
    expect(JSON.stringify(check)).toContain("Not enough disk space");
    expect(check?.act).toBe("pullRecommended");
    expect(check?.answer).not.toContain("No models yet");
  });
  render(withSession(<AddMachinePage flow={view.result.current} onLeave={vi.fn()} />));
  expect(screen.getByText(/Not enough disk space/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: /Pull.*recommended models/ }));
  await waitFor(() => expect(connection.query.fleetPullRecommended).toHaveBeenCalledWith({ registrationId: "recovery-machine" }));
});

it("a failed pull-feed read is visible instead of claiming no models exist", async () => {
  const connection = fakeConnection({ myWorkersWithStatus: [machine()] });
  connection.query.modelPullsForWorker.mockRejectedValue(new Error("Pull progress is unavailable"));
  const view = await connectedFlow(connection);
  await waitFor(() => {
    const check = view.result.current.checks.find((c) => c.id === "models");
    expect(JSON.stringify(check)).toContain("Pull progress is unavailable");
    expect(check?.answer).not.toContain("No models yet");
  });
});

it("a later embedding success does not hide a text failure, and a successful text retry clears it", async () => {
  const failed = modelPullRow({
    id: "failed-text", workerId: "recovery-machine", model: "z-chat", status: "failed",
    requestedAt: "2026-09-01T10:00:00Z", errorMessage: "Text download failed",
  });
  const succeeded = modelPullRow({
    id: "successful-embed", workerId: "recovery-machine", model: "a-embed", status: "succeeded",
    requestedAt: "2026-09-01T10:01:00Z",
  });
  const connection = fakeConnection({
    myWorkersWithStatus: [machine({ "model:a-embed": "embeddings=1" })],
    modelPullsForWorker: [failed, succeeded],
  });
  const view = await connectedFlow(connection);
  await waitFor(() => expect(JSON.stringify(view.result.current.checks)).toContain("Text download failed"));
  act(() => connection.subscriptions.emit(MODEL_PULL_CONCEPT,
    { ...failed, id: "text-retry", status: "succeeded", errorMessage: "", requestedAt: "2026-09-01T10:02:00Z" }, "NODE_CREATED"));
  await waitFor(() => {
    expect(JSON.stringify(view.result.current.checks)).not.toContain("Text download failed");
    expect(view.result.current.checks.find((c) => c.id === "models")?.state).toBe("done");
  });
});

it("advertising a manually repaired model clears its historical pull failure", async () => {
  const connection = fakeConnection({
    myWorkersWithStatus: [machine()],
    modelPullsForWorker: [modelPullRow({
      id: "failed-text", workerId: "recovery-machine", model: "z-chat", status: "failed",
      errorMessage: "Text download failed",
    })],
  });
  const view = await connectedFlow(connection);
  await waitFor(() => expect(JSON.stringify(view.result.current.checks)).toContain("Text download failed"));
  act(() => connection.subscriptions.emit(WORKER_REGISTRATION_CONCEPT,
    machine({ "model:z-chat": "tools=1" }), "NODE_UPDATED"));
  await waitFor(() => {
    expect(JSON.stringify(view.result.current.checks)).not.toContain("Text download failed");
    expect(view.result.current.checks.find((c) => c.id === "models")?.state).toBe("done");
  });
});

it("repairing one model does not hide another model's unresolved failure", async () => {
  const connection = fakeConnection({
    myWorkersWithStatus: [machine({ "model:a-embed": "embeddings=1" })],
    modelPullsForWorker: [
      modelPullRow({ id: "repaired-embed", workerId: "recovery-machine", model: "a-embed", status: "failed", requestedAt: "2026-09-01T10:02:00Z", errorMessage: "Old embed error" }),
      modelPullRow({ id: "failed-text", workerId: "recovery-machine", model: "z-chat", status: "failed", requestedAt: "2026-09-01T10:01:00Z", errorMessage: "Text still needs repair" }),
    ],
  });
  const view = await connectedFlow(connection);
  await waitFor(() => expect(JSON.stringify(view.result.current.checks)).toContain("Text still needs repair"));
  expect(view.result.current.checks.find((check) => check.id === "models")?.state).toBe("stopped");
});
