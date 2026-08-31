import { act, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { CallHistory } = await import("../../src/apps/fleet/routing/CallHistory");
const { fakeConnection, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

async function click(el: Element) {
  await act(async () => {
    (el as HTMLElement).click();
  });
}

function mount(connection: Conn) {
  h.connection = connection;
  return render(
    withSession(<CallHistory workerId="v1:worker:registration:live" machineLabel="Studio mini" />),
  );
}

const CALL = {
  id: "v1:worker:invocation:1",
  tool: "workerHost",
  action: "exec",
  outcome: "rerouted",
  durationMs: 1500,
  startedAt: "2026-08-30T10:00:00Z",
  routing: {
    policyId: "p1",
    strategy: "labelMatch",
    candidatesConsidered: ["reg-b", "reg-a"],
    attempts: 2,
    selectedBy: "policy",
    reroutedFrom: "workbench",
  },
};

beforeEach(() => {
  h.connection = null;
});

describe("the per-machine call history", () => {
  it("reads NOTHING until the panel is opened", async () => {
    const connection = fakeConnection({ invocationsForWorker: [CALL] });
    mount(connection);
    // Twenty machines rendering a collapsed history would be twenty queries
    // on load, for a panel nobody has opened.
    expect(connection.query.invocationsForWorker).not.toHaveBeenCalled();

    await click(screen.getByRole("button", { name: "Recent calls on Studio mini" }));
    await waitFor(() =>
      expect(connection.query.invocationsForWorker).toHaveBeenCalledWith({
        workerId: "v1:worker:registration:live",
      }),
    );
  });

  it("says when it was read, because nothing pushes an update to it", async () => {
    mount(fakeConnection({ invocationsForWorker: [CALL] }));
    await click(screen.getByRole("button", { name: "Recent calls on Studio mini" }));
    // v1:worker:invocation is deliberately excluded from broadcast routing on
    // volume grounds, so an absence of new rows must not read as "nothing is
    // happening" when it means "nobody has asked lately".
    expect(await screen.findByText(/refreshes on request rather than on its own/)).toBeTruthy();
  });

  it("re-reads on request", async () => {
    const connection = fakeConnection({ invocationsForWorker: [CALL] });
    mount(connection);
    await click(screen.getByRole("button", { name: "Recent calls on Studio mini" }));
    await waitFor(() => expect(connection.query.invocationsForWorker).toHaveBeenCalledTimes(1));

    await click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(connection.query.invocationsForWorker).toHaveBeenCalledTimes(2));
  });

  it("renders a call's outcome and, on expand, the routing decision behind it", async () => {
    mount(fakeConnection({ invocationsForWorker: [CALL] }));
    await click(screen.getByRole("button", { name: "Recent calls on Studio mini" }));

    const head = await screen.findByText("rerouted");
    expect(screen.getByText("workerHost.exec")).toBeTruthy();
    expect(screen.getByText("1.5s")).toBeTruthy();

    await click(head);
    expect(screen.getByText("labelMatch")).toBeTruthy();
    expect(screen.getByText(/Rerouted from/).textContent).toContain("workbench");
  });

  it("renders an outcome this build does not classify by its own name", async () => {
    mount(
      fakeConnection({
        invocationsForWorker: [{ ...CALL, outcome: "some_future_outcome", routing: {} }],
      }),
    );
    await click(screen.getByRole("button", { name: "Recent calls on Studio mini" }));
    // The enum grows server-side; a value we cannot classify is still a value
    // an operator needs to read.
    expect(await screen.findByText("some_future_outcome")).toBeTruthy();
  });

  it("distinguishes an idle machine from a failed read", async () => {
    const connection = fakeConnection({ invocationsForWorker: [] });
    mount(connection);
    await click(screen.getByRole("button", { name: "Recent calls on Studio mini" }));
    expect(await screen.findByText("No calls recorded for this machine.")).toBeTruthy();

    connection.query.invocationsForWorker.mockRejectedValue(new Error("read refused"));
    await click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(screen.getByText("read refused")).toBeTruthy());
    expect(screen.getByText("The call history could not be read.")).toBeTruthy();
  });

  it("keeps the calls it has when a refresh fails", async () => {
    const connection = fakeConnection({ invocationsForWorker: [CALL] });
    mount(connection);
    await click(screen.getByRole("button", { name: "Recent calls on Studio mini" }));
    await screen.findByText("rerouted");

    connection.query.invocationsForWorker.mockRejectedValue(new Error("read refused"));
    await click(screen.getByRole("button", { name: "Refresh" }));

    await waitFor(() => expect(screen.getByText("read refused")).toBeTruthy());
    // A stale answer beats no answer.
    expect(screen.getByText("rerouted")).toBeTruthy();
    expect(screen.getByText(/from the last successful read/)).toBeTruthy();
  });
});
