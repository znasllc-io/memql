import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));
import { NexusApp } from "../../src/apps/nexus/NexusApp";
import { LocalNexusSettingsStore } from "../../src/apps/nexus/settings";
import {
  approvalRow,
  constructRow,
  fakeConnection,
  goalRow,
  procedureRow,
  runRow,
  withSession,
} from "./harness";

const rows = (make: (n: number) => Row) =>
  Array.from({ length: 25 }, (_, n) => make(n));
const stamp = (n: number) => new Date(Date.UTC(2026, 8, 1, n)).toISOString();
function mount(
  connection: ReturnType<typeof fakeConnection>,
  section: string,
  intent?: { id: string; payload: Record<string, string> },
) {
  h.connection = connection;
  const store = new LocalNexusSettingsStore({
    getItem: () => null,
    setItem: () => {},
  });
  return render(
    withSession(
      <NexusApp
        sectionId={section}
        navigate={() => {}}
        askContext={() => {}}
        store={store}
        intent={intent}
      />,
    ),
  );
}
const rowCount = () =>
  document.querySelector(".os-livelist-rows")?.children.length ?? 0;

const cases = [
  {
    section: "goals",
    method: "workGoalsBrowse",
    label: "Search your goals",
    placeholder: "What you asked for",
    seed: () => ({
      goals: rows((n) =>
        goalRow({
          id: `goal-${n}`,
          statement: n === 0 ? "Earlier Needle%_ goal" : `Goal ${n}`,
          createdAt: stamp(n),
          input: { large: "x".repeat(4000) },
        }),
      ),
    }),
  },
  {
    section: "runs",
    method: "workRunsBrowse",
    label: "Search your runs",
    placeholder: "Automation, status or mode",
    seed: () => ({
      runs: rows((n) =>
        runRow({
          id: `run-${n}`,
          title: n === 0 ? "Earlier Needle%_ run" : `Run ${n}`,
          createdAt: stamp(n),
          input: { large: "x".repeat(4000) },
          status: "running",
        }),
      ),
    }),
  },
  {
    section: "approvals",
    method: "workApprovalsBrowse",
    label: "Search your approvals",
    placeholder: "Kind, question or reason",
    seed: () => ({
      approvals: rows((n) =>
        approvalRow({
          id: `approval-${n}`,
          question: n === 24 ? "Later Needle%_ approval" : `Approval ${n}`,
          requestedAt: stamp(n),
          createdAt: stamp(n),
        }),
      ),
    }),
  },
  {
    section: "automations",
    method: "nexusAutomationsBrowse",
    label: "Refine automations",
    placeholder: "Search",
    seed: () => ({
      constructs: rows((n) =>
        constructRow({
          id: `automation-${n}`,
          name: n === 0 ? "Earlier Needle%_ automation" : `Automation ${n}`,
          createdAt: stamp(n),
          source: "x".repeat(4000),
        }),
      ),
    }),
  },
] as const;

describe("Nexus resource directory budgets", () => {
  it.each(cases)(
    "$section loads10, appends10, retains20 across reopen, and searches beyond loaded pages",
    async (c) => {
      const connection = fakeConnection(c.seed());
      let view = mount(connection, c.section);
      await waitFor(() => expect(rowCount()).toBe(10));
      const read = connection.query[c.method];
      expect(read).toHaveBeenCalledTimes(1);
      expect(connection.query.workRunSummariesForOwner).not.toHaveBeenCalled();
      expect(connection.query.workGoalsForOwner).not.toHaveBeenCalled();
      expect(
        connection.query.cataloguedConstructsForOwner,
      ).not.toHaveBeenCalled();
      expect(connection.query.learnedProceduresForOwner).not.toHaveBeenCalled();
      expect(connection.query.workStepsForOwnerRun).not.toHaveBeenCalled();
      const first = await read.mock.results[0]!.value;
      expect(first.rows()).toHaveLength(10);
      expect(JSON.stringify(first.rows())).not.toContain("x".repeat(4000));
      fireEvent.click(screen.getByRole("button", { name: "Load more" }));
      await waitFor(() => expect(rowCount()).toBe(20));
      expect(read).toHaveBeenCalledTimes(2);
      view.unmount();
      view = mount(connection, c.section);
      await waitFor(() => expect(rowCount()).toBe(20));
      expect(read).toHaveBeenCalledTimes(2);
      fireEvent.click(screen.getByRole("button", { name: c.label }));
      fireEvent.change(screen.getByRole("textbox", { name: c.placeholder }), {
        target: { value: "needle%_" },
      });
      await waitFor(() => expect(rowCount()).toBe(1));
      expect(read).toHaveBeenCalledTimes(3);
      expect(read.mock.calls[2]![0]).toMatchObject({ search: "needle%_" });
      fireEvent.change(screen.getByRole("textbox", { name: c.placeholder }), {
        target: { value: "" },
      });
      await waitFor(() => expect(rowCount()).toBe(20));
      expect(read).toHaveBeenCalledTimes(3);
      view.unmount();
    },
  );

  it.each([
    ["More access", { kind: "scopeElevation" }, { kindLabels: ["scopeElevation"] }],
    ["Repository change", { kind: "planReview", subject: { reviewType: "repository-change" } }, { repositoryLabel: true }],
    ["Work stopped", { kind: "planReview", options: [{ value: "retry" }, { value: "abandon" }] }, { failureLabel: true }],
  ])("searches the visible approval label %s beyond the first ten", async (label, fields, expected) => {
    const connection = fakeConnection({ approvals: rows(n => approvalRow({ id: `label-${n}`, requestedAt: stamp(n), ...(n === 24 ? fields : {}) })) });
    mount(connection, "approvals");
    await waitFor(() => expect(rowCount()).toBe(10));
    fireEvent.click(screen.getByRole("button", { name: "Search your approvals" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Kind, question or reason" }), { target: { value: label } });
    await waitFor(() => expect(rowCount()).toBe(1));
    expect(connection.query.workApprovalsBrowse.mock.calls.at(-1)?.[0]).toMatchObject(expected);
    expect(screen.getByText(label, { selector: ".os-record-status" })).toBeTruthy();
  });

  it("holds a failed continuation at the same cursor and does not reread the first page", async () => {
    const connection = fakeConnection(cases[1].seed());
    mount(connection, "runs");
    await waitFor(() => expect(rowCount()).toBe(10));
    connection.query.workRunsBrowse.mockRejectedValueOnce(
      new Error("Next page unavailable"),
    );
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    await screen.findByText("Next page unavailable");
    expect(rowCount()).toBe(10);
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(rowCount()).toBe(20));
    expect(
      connection.query.workRunsBrowse.mock.calls.map(
        (call) => (call[1] as { cursor?: string })?.cursor,
      ),
    ).toEqual([undefined, "10", "10"]);
  });

  it("reads a deep-linked goal independently and keeps unrelated history out of its detail", async () => {
    const goal = goalRow({
      id: "earlier-goal",
      statement: "An older saved goal",
      input: { retained: "full detail" },
    });
    const connection = fakeConnection({
      goals: [...cases[0].seed().goals, goal],
      runs: [
        runRow({ id: "mine", goalId: goal.id, status: "running" }),
        runRow({ id: "other", goalId: "some-other-goal", status: "running" }),
      ],
    });
    mount(connection, "goals", {
      id: "open",
      payload: { goalId: "earlier-goal" },
    });
    expect(
      (await screen.findAllByText("An older saved goal")).length,
    ).toBeGreaterThan(0);
    await waitFor(() =>
      expect(connection.query.workRunSummariesForGoal).toHaveBeenCalledWith(
        { goalId: "earlier-goal" },
        expect.anything(),
      ),
    );
    expect(connection.query.workGoalForOwner).toHaveBeenCalledWith(
      { goalId: "earlier-goal" },
      expect.anything(),
    );
    expect(connection.query.workRunSummariesForOwner).not.toHaveBeenCalled();
    expect(
      document.querySelector(".os-nexus-goals .os-livelist-rows"),
    ).toBeNull();
  });

  it("merges authored and learned records in one global recent page without procedure bodies", async () => {
    const connection = fakeConnection({
      constructs: rows((n) =>
        constructRow({
          id: `a-${n}`,
          name: `Authored ${n}`,
          createdAt: stamp(n),
        }),
      ),
      learnedProcedures: rows((n) =>
        procedureRow({
          id: `p-${n}`,
          createdAt: new Date(Date.parse(stamp(n)) + 1000).toISOString(),
        }),
      ),
    });
    mount(connection, "automations");
    await waitFor(() => expect(rowCount()).toBe(10));
    const result =
      await connection.query.nexusAutomationsBrowse.mock.results[0]!.value;
    expect(result.rows().map((row: Row) => row.id)).toEqual([
      "p-24",
      "a-24",
      "p-23",
      "a-23",
      "p-22",
      "a-22",
      "p-21",
      "a-21",
      "p-20",
      "a-20",
    ]);
    expect(
      result
        .rows()
        .some((row: Row) => row.procedure || row.source || row.compiledForm),
    ).toBe(false);
    expect(screen.getAllByText(/Learned from 2 runs/)).toHaveLength(5);
    await act(async () =>
      connection.subscriptions.emit(
        "v1:authoring:construct",
        constructRow({
          id: "foreign",
          ownerUserId: "someone-else",
          name: "Foreign live record",
          createdAt: stamp(30),
        }),
        "NODE_CREATED",
      ),
    );
    expect(screen.queryByText("Foreign live record")).toBeNull();
  });
});
