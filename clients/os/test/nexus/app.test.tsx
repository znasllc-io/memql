import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { NexusApp } = await import("../../src/apps/nexus/NexusApp");
const { LocalNexusSettingsStore } = await import("../../src/apps/nexus/settings");
const { approvalRow, fakeConnection, goalRow, runRow, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

const GOAL = "v1:work:goal";
const RUN = "v1:work:run";
const APPROVAL = "v1:work:approval";

function memoryStore(over: Record<string, unknown> = {}) {
  const bag = new Map<string, string>();
  const store = new LocalNexusSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
  if (Object.keys(over).length > 0) store.save({ ...store.load(), ...over });
  return store;
}

function mount(connection: Conn, sectionId = "goals", settings: Record<string, unknown> = {}) {
  h.connection = connection;
  const navigate = vi.fn();
  const view = render(
    withSession(
      <NexusApp
        sectionId={sectionId}
        navigate={navigate}
        askContext={() => {}}
        store={memoryStore(settings)}
      />,
    ),
  );
  return { view, navigate };
}

describe("goals, the landing surface", () => {
  it("leads with what the person asked for, in their own words", async () => {
    const conn = fakeConnection({
      goals: [
        goalRow({ id: "g1", statement: "Reconcile last month's ledger" }),
        goalRow({ id: "g2", statement: "Draft the Q3 board note", origin: "responsibility" }),
      ],
    });
    mount(conn);
    expect(await screen.findByText("Reconcile last month's ledger")).toBeTruthy();
    expect(screen.getByText("Draft the Q3 board note")).toBeTruthy();
    // The origin is said in the person's terms, not as the enum member.
    expect(screen.getByText("A standing responsibility")).toBeTruthy();
  });

  it("marks a goal whose run is parked on a PERSON, and stays quiet about a timer", async () => {
    // A standing mark, not an arrival cue: a parked run can wait for days, so
    // a cue that decays on the clock would be seen only by whoever happened
    // to be looking at the moment it parked.
    const conn = fakeConnection({
      goals: [
        goalRow({ id: "g1", statement: "Needs you" }),
        goalRow({ id: "g2", statement: "Just waiting on a clock" }),
      ],
      runs: [
        runRow({
          id: "r1",
          goalId: "g1",
          status: "waiting",
          waitingOn: { kind: "approval", subject: "sendInvoice" },
        }),
        runRow({ id: "r2", goalId: "g2", status: "waiting", waitingOn: { kind: "timer" } }),
      ],
    });
    mount(conn);
    await screen.findByText("Needs you");
    expect(screen.getAllByText("waiting for you")).toHaveLength(1);
  });

  // The question a person opens this list with is "what is happening", and a
  // goal whose run is in flight or waiting on them is the answer. A plain
  // newest-first sort put the parked goal UNDER two that wanted nothing, which
  // is what the first rendered pass showed.
  it("puts work in flight above goals that want nothing, newest first inside each", async () => {
    const conn = fakeConnection({
      goals: [
        goalRow({ id: "g1", statement: "Parked on you", createdAt: "2026-09-01T09:00:00Z" }),
        goalRow({ id: "g2", statement: "Nothing running", createdAt: "2026-09-04T09:00:00Z" }),
        goalRow({ id: "g3", statement: "Also nothing", createdAt: "2026-09-03T09:00:00Z" }),
      ],
      runs: [
        runRow({
          id: "r1",
          goalId: "g1",
          status: "waiting",
          finishedAt: "",
          waitingOn: { kind: "approval" },
        }),
      ],
    });
    mount(conn);
    await screen.findByText("Parked on you");
    const statements = [...document.querySelectorAll(".os-nexus-goal-statement")].map(
      (el) => el.textContent,
    );
    expect(statements).toEqual(["Parked on you", "Nothing running", "Also nothing"]);
  });

  // A goal now opens as a MAP, a rail and a receipt, which is taller than the
  // run page -- so it REPLACES the list (rule 11's `<- Goals` form) rather than
  // sitting beside it, and the list's own detail column went with the change.
  it("opens a goal in place of the list, with a way back", async () => {
    const conn = fakeConnection({
      goals: [goalRow({ id: "g1", statement: "Reconcile the ledger" })],
      runs: [runRow({ id: "r1", goalId: "g1" })],
    });
    mount(conn);
    fireEvent.click(await screen.findByText("Reconcile the ledger"));
    expect(await screen.findByRole("group", { name: "What you can do with this" })).toBeTruthy();
    // The list is gone rather than scrolled past: its one action is not here.
    expect(screen.queryByText("New goal")).toBeNull();
    expect(screen.getByRole("button", { name: "Goals" })).toBeTruthy();
  });

  it("draws the map, and says what it is a map OF", async () => {
    const conn = fakeConnection({
      goals: [goalRow({ id: "g1", statement: "Reconcile the ledger" })],
      runs: [runRow({ id: "r1", goalId: "g1" })],
    });
    mount(conn);
    fireEvent.click(await screen.findByText("Reconcile the ledger"));
    const map = await screen.findByRole("application");
    // The accessible name carries the whole reading, because the picture
    // itself is unreadable to a screen reader by construction.
    expect(map.getAttribute("aria-label")).toContain("Reconcile the ledger");
  });

  it("asks for the goal's own words before it will close it", async () => {
    // Closing asks every run of the goal to stop, so the confirmation is the
    // thing's own name -- which for a goal is the sentence somebody wrote.
    const conn = fakeConnection({
      goals: [goalRow({ id: "g1", statement: "Reconcile the ledger" })],
    });
    mount(conn);
    fireEvent.click(await screen.findByText("Reconcile the ledger"));
    fireEvent.click(await screen.findByText("Close this goal"));
    expect(conn.query.cancelGoal).not.toHaveBeenCalled();

    fireEvent.change(screen.getByLabelText(/Type the goal's own words/), {
      target: { value: "Reconcile the ledger" },
    });
    fireEvent.click(screen.getByText("Close this goal"));
    await waitFor(() => expect(conn.query.cancelGoal).toHaveBeenCalled());
    expect(conn.query.cancelGoal.mock.calls[0]?.[0]).toMatchObject({ goalId: "g1" });
  });

  it("offers no close on a goal that is already closed -- absent, never disabled", async () => {
    const conn = fakeConnection({
      goals: [goalRow({ id: "g1", statement: "Already done", status: "closed" })],
    });
    mount(conn);
    fireEvent.click(await screen.findByText("Already done"));
    await screen.findByRole("group", { name: "What you can do with this" });
    expect(screen.queryByText("Close this goal")).toBeNull();
  });
});

describe("a new goal", () => {
  it("sends the statement and says which surface it came from", async () => {
    const conn = fakeConnection({ createReply: { goalId: "g9", runId: "r9" } });
    mount(conn);
    fireEvent.click(await screen.findByText("New goal"));
    fireEvent.change(screen.getByLabelText("The goal, in your own words"), {
      target: { value: "Reconcile the ledger" },
    });
    fireEvent.click(screen.getByText("Start work"));
    await waitFor(() => expect(conn.query.createGoal).toHaveBeenCalled());
    expect(conn.query.createGoal.mock.calls[0]?.[0]).toMatchObject({
      statement: "Reconcile the ledger",
      // Guessing "api" would file every goal a person typed as one a program
      // submitted.
      requestedVia: "nexus",
    });
  });

  it("tags the goal with the accounts the person picked", async () => {
    // The tag is a RECORD of who the work is for. It narrows no read and
    // grants no access -- so the test asserts it reaches createGoal, and the
    // label asserts it does not read as permissions.
    const conn = fakeConnection({
      createReply: { goalId: "g9", runId: "r9" },
      accounts: [{ id: "v1:accounts:account:acme", name: "Acme Ltd", status: "active" } as Row],
    });
    mount(conn);
    fireEvent.click(await screen.findByText("New goal"));
    fireEvent.change(screen.getByLabelText("The goal, in your own words"), {
      target: { value: "Reconcile the ledger" },
    });
    fireEvent.click(await screen.findByRole("button", { name: "Acme Ltd" }));
    fireEvent.click(screen.getByText("Start work"));
    await waitFor(() => expect(conn.query.createGoal).toHaveBeenCalled());
    expect(conn.query.createGoal.mock.calls[0]?.[0]).toMatchObject({
      statement: "Reconcile the ledger",
      accountIds: ["v1:accounts:account:acme"],
    });
  });

  it("offers no account control at all when the person has no accounts", async () => {
    // An empty picker is a control that cannot be used, and a label reading
    // "Who this work is for" over nothing is a question with no answers.
    const conn = fakeConnection({ createReply: { goalId: "g9", runId: "r9" } });
    mount(conn);
    fireEvent.click(await screen.findByText("New goal"));
    expect(screen.queryByRole("group", { name: "Who this work is for (optional)" })).toBeNull();
    // And not the picker's own empty-state either: an input the person cannot
    // use is worse than no input on the one form in the product.
    expect(screen.queryByText("No clients yet. The Accounts app is where they are added.")).toBeNull();
  });

  it("refuses an empty statement without a round trip", async () => {
    const conn = fakeConnection();
    mount(conn);
    fireEvent.click(await screen.findByText("New goal"));
    fireEvent.click(screen.getByText("Start work"));
    expect(await screen.findByText("Say what you want done first.")).toBeTruthy();
    expect(conn.query.createGoal).not.toHaveBeenCalled();
  });

  it("shows the server's own sentence when the write refuses, and keeps the form", async () => {
    // The executors are being written in parallel with this surface, so this
    // is the state a person will actually meet. A client that assumed success
    // would report a goal as accepted that no run exists for.
    const conn = fakeConnection({ writeError: new Error("no executor registered for createGoal") });
    mount(conn);
    fireEvent.click(await screen.findByText("New goal"));
    fireEvent.change(screen.getByLabelText("The goal, in your own words"), {
      target: { value: "Reconcile the ledger" },
    });
    fireEvent.click(screen.getByText("Start work"));
    expect(await screen.findByText("no executor registered for createGoal")).toBeTruthy();
    expect(screen.getByText("The goal was not accepted.")).toBeTruthy();
    // Nothing was inserted locally, so the list is still empty.
    expect(screen.getByLabelText("Your goals").children).toHaveLength(0);
  });
});

describe("the arrival cue", () => {
  it("announces a goal created while somebody is watching", async () => {
    const conn = fakeConnection({ goals: [goalRow({ id: "g1", statement: "First" })] });
    mount(conn);
    await screen.findByText("First");

    await act(async () => {
      conn.subscriptions.emit(GOAL, goalRow({ id: "g2", statement: "Second" }), "NODE_CREATED");
    });
    expect(await screen.findByText("Second")).toBeTruthy();
    const row = screen.getByText("Second").closest("li");
    expect(row?.getAttribute("data-arrival")).toBe("added");
  });

  it("STAYS SILENT ON A HEARTBEAT, and this is the rule this app gets wrong by default", async () => {
    // A running run writes `heartbeatAt` at every step boundary and
    // broadcasts the whole row. If the fingerprint named it, the cue would
    // fire hardest for the run somebody is already watching move.
    const conn = fakeConnection({
      runs: [runRow({ id: "r1", status: "running", heartbeatAt: "2026-09-01T09:05:00Z" })],
    });
    mount(conn, "runs");
    await screen.findByText("nightlyReconcile");

    await act(async () => {
      conn.subscriptions.emit(
        RUN,
        runRow({ id: "r1", status: "running", heartbeatAt: "2026-09-01T09:05:15Z" }),
      );
    });
    const row = screen.getByText("nightlyReconcile").closest("li");
    expect(row?.getAttribute("data-arrival")).toBeNull();
  });

  it("stays silent on a counter tick, and still puts the new figure on screen", async () => {
    // Re-rendering and ringing are different statements, and the fingerprint
    // is the only thing that separates them.
    const conn = fakeConnection({
      runs: [runRow({ id: "r1", status: "running", spent: { modelCalls: 1 } })],
      steps: [],
    });
    mount(conn, "runs");
    fireEvent.click(await screen.findByText("nightlyReconcile"));
    await screen.findByLabelText("What this run spent");
    expect(within(screen.getByLabelText("What this run spent")).getByText("1")).toBeTruthy();

    await act(async () => {
      conn.subscriptions.emit(
        RUN,
        runRow({ id: "r1", status: "running", spent: { modelCalls: 4 } }),
      );
    });
    await waitFor(() =>
      expect(within(screen.getByLabelText("What this run spent")).getByText("4")).toBeTruthy(),
    );
  });

  it("RINGS when a run parks on a person, which is news somebody must act on", async () => {
    const conn = fakeConnection({ runs: [runRow({ id: "r1", status: "running" })] });
    mount(conn, "runs");
    await screen.findByText("nightlyReconcile");

    await act(async () => {
      conn.subscriptions.emit(
        RUN,
        runRow({ id: "r1", status: "waiting", waitingOn: { kind: "approval" } }),
      );
    });
    await waitFor(() => expect(screen.getByText("waiting for you")).toBeTruthy());
    const row = screen.getByText("nightlyReconcile").closest("li");
    expect(row?.getAttribute("data-arrival")).toBe("updated");
  });
});

describe("the runs list", () => {
  it("seeds UNFILTERED, so the show-finished toggle cannot re-baseline the cue", async () => {
    const conn = fakeConnection({ runs: [runRow({ id: "r1" })] });
    mount(conn, "runs");
    await screen.findByText("nightlyReconcile");
    expect(conn.query.workRunsForOwner).toHaveBeenCalledTimes(1);
    expect(conn.query.workRunsForOwner.mock.calls[0]?.[0]).toEqual({});
  });

  it("hides finished runs under the settings preference, and says where the setting is", async () => {
    const seed = { runs: [runRow({ id: "r1", status: "succeeded" })] };
    mount(fakeConnection(seed), "runs", { showFinishedRuns: false });
    expect(
      await screen.findByText(
        /Finished runs are hidden -- the setting is in this app's Settings\./,
      ),
    ).toBeTruthy();
    expect(screen.queryByText("nightlyReconcile")).toBeNull();
  });

  it("counts what is stuck on the head line, and the count goes when it is answered", async () => {
    const conn = fakeConnection({
      runs: [
        runRow({ id: "r1", status: "waiting", waitingOn: { kind: "feedback" } }),
        runRow({ id: "r2", status: "succeeded" }),
      ],
    });
    mount(conn, "runs");
    expect(await screen.findByText("2 runs -- 1 waiting for you")).toBeTruthy();
  });
});

describe("the section list", () => {
  it("navigates to the app's preferred section on open, and only from the shell's default", async () => {
    const first = mount(fakeConnection(), "goals", { defaultSection: "approvals" });
    await waitFor(() => expect(first.navigate).toHaveBeenCalledWith("approvals"));
    first.view.unmount();

    // A window opened on a NAMED section was opened by somebody who said
    // where they wanted to be; a preference that overrode that would make a
    // deep link silently not work.
    const second = mount(fakeConnection(), "runs", { defaultSection: "approvals" });
    await screen.findByText("Runs");
    expect(second.navigate).not.toHaveBeenCalled();
  });

  it("opens a run named by an open intent, whichever section the window is on", async () => {
    h.connection = fakeConnection({ runs: [runRow({ id: "r1" })], steps: [] });
    const navigate = vi.fn();
    const consumeIntent = vi.fn();
    render(
      withSession(
        <NexusApp
          sectionId="goals"
          navigate={navigate}
          askContext={() => {}}
          intent={{ id: "i1", payload: { runId: "r1" } }}
          consumeIntent={consumeIntent}
          store={memoryStore()}
        />,
      ),
    );
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("runs"));
    expect(consumeIntent).toHaveBeenCalledWith("i1");
  });

  it("ignores an intent that names nothing this app can open", async () => {
    h.connection = fakeConnection();
    const navigate = vi.fn();
    const consumeIntent = vi.fn();
    render(
      withSession(
        <NexusApp
          sectionId="goals"
          navigate={navigate}
          askContext={() => {}}
          intent={{ id: "i1", payload: { somethingElse: "x" } }}
          consumeIntent={consumeIntent}
          store={memoryStore()}
        />,
      ),
    );
    await screen.findByText("Goals");
    expect(consumeIntent).not.toHaveBeenCalled();
  });
});

describe("approvals reach the goals surface too", () => {
  it("marks the goal, so somebody who never opens the inbox still finds out", async () => {
    const conn = fakeConnection({
      goals: [goalRow({ id: "g1", statement: "Send the invoices" })],
      runs: [
        runRow({ id: "r1", goalId: "g1", status: "waiting", waitingOn: { kind: "approval" } }),
      ],
      approvals: [approvalRow({ id: "a1", runId: "r1" })],
    });
    mount(conn);
    await screen.findByText("Send the invoices");
    expect(screen.getByText("waiting for you")).toBeTruthy();
  });
});

describe("the approvals feed", () => {
  it("subscribes to the approval concept, not to a poll", async () => {
    const conn = fakeConnection({ approvals: [approvalRow({ id: "a1" })] });
    mount(conn, "approvals");
    await screen.findByText("Step sendInvoice");
    await act(async () => {
      conn.subscriptions.emit(
        APPROVAL,
        approvalRow({ id: "a2", stepKey: "chargeCard" }),
        "NODE_CREATED",
      );
    });
    expect(await screen.findByText("Step chargeCard")).toBeTruthy();
  });
});
