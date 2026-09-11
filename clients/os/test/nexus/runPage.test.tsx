import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { NexusApp } = await import("../../src/apps/nexus/NexusApp");
const { LocalNexusSettingsStore } = await import("../../src/apps/nexus/settings");
const { fakeConnection, goalRow, runRow, stepRow, withSession } = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;

const STEP = "v1:work:step";

function mount(connection: Conn, sectionId = "runs") {
  h.connection = connection;
  const bag = new Map<string, string>();
  const navigate = vi.fn();
  const view = render(
    withSession(
      <NexusApp
        sectionId={sectionId}
        navigate={navigate}
        askContext={() => {}}
        store={
          new LocalNexusSettingsStore({
            getItem: (k: string) => bag.get(k) ?? null,
            setItem: (k: string, v: string) => void bag.set(k, v),
          })
        }
      />,
    ),
  );
  return { view, navigate };
}

/** A run of five steps, three of which are free and one of which thought. */
function fiveSteps() {
  return [
    stepRow({ id: "s3", key: "classify", seq: 2, kind: "reasoning", tokens: 1240, cost: 0.0041, durationMs: 2100 }),
    stepRow({ id: "s1", key: "fetchLedger", seq: 0, durationMs: 38 }),
    stepRow({ id: "s5", key: "notify", seq: 4, kind: "human", status: "waiting" }),
    stepRow({ id: "s2", key: "normalise", seq: 1, kind: "deterministic", durationMs: 4 }),
    stepRow({ id: "s4", key: "writeReport", seq: 3, stepType: "mutation", status: "failed", symptom: "contract", errorMessage: "the postcondition did not hold" }),
  ];
}

async function openRun(conn: Conn) {
  mount(conn);
  fireEvent.click(await screen.findByText("nightlyReconcile"));
  return screen.findByLabelText("What this run did, in order");
}

describe("the timeline", () => {
  it("draws the steps in SEQ order, not in the order the read answered", async () => {
    // `workStepsForOwnerRun` carries @unbounded, which excludes sort, so the
    // rows arrive in fold order. A timeline drawn in fold order reshuffles
    // itself the moment any step updates -- exactly when somebody is watching.
    const conn = fakeConnection({ runs: [runRow({ id: "run-1" })], steps: fiveSteps() });
    const timeline = await openRun(conn);
    const keys = within(timeline)
      .getAllByRole("button")
      .map((b) => b.getAttribute("aria-label") ?? "");
    expect(keys.map((label) => label.split(", ")[1])).toEqual([
      "fetchLedger",
      "normalise",
      "classify",
      "writeReport",
      "notify",
    ]);
  });

  it("marks the step that thought, and only that one", async () => {
    // The whole product story: most of a run is free machine motion and a
    // little of it is expensive thought, and that has to be legible before a
    // word is read.
    const conn = fakeConnection({ runs: [runRow({ id: "run-1" })], steps: fiveSteps() });
    const timeline = await openRun(conn);
    const thought = within(timeline)
      .getAllByRole("button")
      .filter((b) => b.getAttribute("data-thought") === "true");
    expect(thought).toHaveLength(1);
    expect(thought[0]?.getAttribute("aria-label")).toContain("classify");
  });

  it("says in WORDS what the drawing says, for a reader who cannot see it", async () => {
    const conn = fakeConnection({ runs: [runRow({ id: "run-1" })], steps: fiveSteps() });
    const timeline = await openRun(conn);
    expect(
      within(timeline).getByLabelText("Step 3, classify, Reasoning, called a model, Done"),
    ).toBeTruthy();
    expect(
      within(timeline).getByLabelText(
        "Step 1, fetchLedger, Deterministic, ran without calling a model, Done",
      ),
    ).toBeTruthy();
  });

  it("puts the cost only on the step that cost something", async () => {
    const conn = fakeConnection({ runs: [runRow({ id: "run-1" })], steps: fiveSteps() });
    const timeline = await openRun(conn);
    // A dash on four rows to say "free" is four things to read past.
    expect(within(timeline).getAllByText("1.2k tok")).toHaveLength(1);
    expect(within(timeline).getAllByText("$0.0041")).toHaveLength(1);
  });

  it("names the classifier's answer under the step it is about", async () => {
    const conn = fakeConnection({ runs: [runRow({ id: "run-1" })], steps: fiveSteps() });
    const timeline = await openRun(conn);
    expect(within(timeline).getByText("A contract was broken")).toBeTruthy();
    expect(within(timeline).getByText("the postcondition did not hold")).toBeTruthy();
  });

  it("reports an absent postcondition as 'none declared', never as a failure", async () => {
    // Epic A1 declares no postcondition on any step. Reading an absent one as
    // false would mark every run in the cluster failed on a field nobody wrote.
    const conn = fakeConnection({ runs: [runRow({ id: "run-1" })], steps: fiveSteps() });
    const timeline = await openRun(conn);
    fireEvent.click(within(timeline).getByLabelText(/Step 1, fetchLedger/));
    expect(await screen.findByText("none declared")).toBeTruthy();
  });

  it("counts the unclassified steps separately rather than assuming they were free", async () => {
    const conn = fakeConnection({
      runs: [runRow({ id: "run-1" })],
      steps: [stepRow({ id: "s1", key: "a", seq: 0, kind: "" })],
    });
    await openRun(conn);
    expect(
      await screen.findByText(/this build cannot yet say whether they called a model/),
    ).toBeTruthy();
  });
});

describe("the kind band", () => {
  it("speaks its proportions, because a bar a screen reader cannot read excluded somebody", async () => {
    const conn = fakeConnection({ runs: [runRow({ id: "run-1" })], steps: fiveSteps() });
    await openRun(conn);
    expect(
      await screen.findByLabelText(
        "5 steps: 3 deterministic, 1 reasoning, 1 human.",
      ),
    ).toBeTruthy();
  });

  it("says 'no steps yet' rather than drawing an empty bar with no account of itself", async () => {
    const conn = fakeConnection({
      runs: [runRow({ id: "run-1", status: "compiling" })],
      steps: [],
    });
    await openRun(conn);
    expect(await screen.findByLabelText("No steps yet.")).toBeTruthy();
    expect(
      screen.getByText(/still working out what to do/),
    ).toBeTruthy();
  });
});

describe("what a run's bar offers", () => {
  it("offers Replay on a finished run and NOT on one still going -- absent, never disabled", async () => {
    // A replay serves every model call from the journal, so replaying a run
    // still writing that journal is a run that will miss. Offering it would
    // be offering a failure.
    const running = fakeConnection({
      runs: [runRow({ id: "run-1", status: "running" })],
      steps: fiveSteps(),
    });
    const first = mount(running);
    fireEvent.click(await screen.findByText("nightlyReconcile"));
    await screen.findByLabelText("What this run did, in order");
    expect(screen.queryByText("Replay")).toBeNull();
    expect(screen.getByText(/replay and fork wait until it finishes/)).toBeTruthy();
    first.view.unmount();

    const done = fakeConnection({ runs: [runRow({ id: "run-1" })], steps: fiveSteps() });
    mount(done);
    fireEvent.click(await screen.findByText("nightlyReconcile"));
    expect(await screen.findByText("Replay")).toBeTruthy();
  });

  it("offers a Fork only once a step is picked, and forks at THAT step", async () => {
    const conn = fakeConnection({
      runs: [runRow({ id: "run-1" })],
      steps: fiveSteps(),
      deriveReply: { runId: "run-2" },
    });
    const timeline = await openRun(conn);
    expect(screen.queryByText(/^Fork from/)).toBeNull();

    fireEvent.click(within(timeline).getByLabelText(/Step 3, classify/));
    fireEvent.click(await screen.findByText("Fork from classify"));
    await waitFor(() => expect(conn.query.forkRun).toHaveBeenCalled());
    expect(conn.query.forkRun.mock.calls[0]?.[0]).toEqual({
      runId: "run-1",
      atStepKey: "classify",
    });
  });

  it("carries NO cancel, because the verb stops every run of the goal", async () => {
    // A control that destroys this run's siblings, on a page about this one,
    // is the shape the Deployables recomposition removed.
    const conn = fakeConnection({
      runs: [runRow({ id: "run-1", status: "running" })],
      steps: fiveSteps(),
    });
    await openRun(conn);
    expect(screen.queryByText(/Cancel/)).toBeNull();
    expect(screen.queryByText(/Stop this run/)).toBeNull();
  });

  it("shows a refusal beside the act, verbatim, rather than as a toast", async () => {
    const conn = fakeConnection({
      runs: [runRow({ id: "run-1" })],
      steps: fiveSteps(),
      writeError: new Error("no executor registered for replayRun"),
    });
    await openRun(conn);
    fireEvent.click(screen.getByText("Replay"));
    expect(await screen.findByText("no executor registered for replayRun")).toBeTruthy();
  });

  // memql#4999. A strict replay that finds no journaled match STOPS -- the
  // model seam refuses to substitute a live call, because a replay that
  // quietly called a provider would report a reproduction that did not
  // happen. Nothing broke, and the page must not say something did: under the
  // generic failure notice this read as "This run failed" followed by a
  // sentence about a classifier that never saw it.
  it("calls a diverged replay a divergence, not a failure", async () => {
    const conn = fakeConnection({
      runs: [
        runRow({
          id: "run-1",
          mode: "replay",
          status: "failed",
          errorCode: "replay_diverged",
          errorMessage: "replay diverged at step classify of run v1:work:run:run-0",
        }),
      ],
      steps: fiveSteps(),
    });
    await openRun(conn);
    expect(screen.getByText("This replay diverged.")).toBeTruthy();
    expect(screen.queryByText("This run failed.")).toBeNull();
    // The step it parted at has to be reachable, or the divergence names
    // nothing the reader can act on.
    expect(
      screen.getByText(/replay diverged at step classify of run v1:work:run:run-0/),
    ).toBeTruthy();
    // AND IT MUST NOT CONTRADICT ITSELF two lines down. The replay caption
    // used to print unconditionally, so this page carried "This replay
    // diverged" and "Every model call was served from the journal, so this
    // run reached no provider" together -- only one of them true of this run.
    // Caught by looking at it rendered, which is what DESIGN.md asks for.
    expect(screen.queryByText(/Every model call was served from the journal/)).toBeNull();
    expect(screen.getByText(/A replay under the strict policy/)).toBeTruthy();
  });

  it("keeps the full replay caption on a replay that did not diverge", async () => {
    const conn = fakeConnection({
      runs: [runRow({ id: "run-1", mode: "replay", status: "succeeded" })],
      steps: fiveSteps(),
    });
    await openRun(conn);
    expect(screen.getByText(/Every model call was served from the journal/)).toBeTruthy();
  });

  // The other direction, so the fix cannot be "never say failed".
  it("still calls an ordinary failure a failure", async () => {
    const conn = fakeConnection({
      runs: [
        runRow({
          id: "run-1",
          status: "failed",
          errorCode: "compile_failed",
          errorMessage: "the authoring pass produced no template",
        }),
      ],
      steps: fiveSteps(),
    });
    await openRun(conn);
    expect(screen.getByText("This run failed.")).toBeTruthy();
    expect(screen.queryByText("This replay diverged.")).toBeNull();
  });

  it("calls a lost run lost, and says nothing failed", async () => {
    const conn = fakeConnection({
      runs: [runRow({ id: "run-1", status: "abandoned" })],
      steps: fiveSteps(),
    });
    await openRun(conn);
    expect(screen.getByText("Lost")).toBeTruthy();
    expect(screen.getByText("The node running this went away.")).toBeTruthy();
    // The sweep offers a silent run to another replica before closing it
    // (memql#5054), so reaching this state means the offer was not taken --
    // which is the difference between "try again" and "find out why nothing
    // took it".
    expect(screen.getByText(/was offered it before it was closed/)).toBeTruthy();
    // AND IT MUST NOT PROMISE MORE THAN THE SYSTEM DOES. The old copy said "a
    // resume picks up from it", naming an action with no button behind it,
    // and "nothing was left half-done", which is not true of the step that
    // was in flight when the node went away.
    expect(screen.queryByText(/a resume picks up from it/)).toBeNull();
    expect(screen.queryByText(/nothing was left half-done/)).toBeNull();
  });

  // memql#5054. Both of these close a run BEFORE any step runs, and under the
  // generic notice both said "This run failed" and pointed at "the step it
  // stopped at" -- of which there is none. They are also the two failures a
  // reader is most likely to misdiagnose: one is a deploy, the other is the
  // arguments, and neither is the machinery the error tone sends people to.
  it("sends an unrunnable template at the deploy, not at the run", async () => {
    const conn = fakeConnection({
      runs: [
        runRow({
          id: "run-1",
          status: "failed",
          errorCode: "automation_not_runnable",
          errorMessage: 'no automation named "invokeAgent" is registered on this node',
        }),
      ],
      steps: [],
    });
    await openRun(conn);
    expect(screen.getByText("Nothing here could run this.")).toBeTruthy();
    expect(screen.queryByText("This run failed.")).toBeNull();
    // The name is the whole of what the reader can act on.
    expect(screen.getByText(/no automation named "invokeAgent" is registered/)).toBeTruthy();
    // It must not invite a retry: retrying resolves the same nothing.
    expect(screen.getByText(/wants a deploy, not another attempt/)).toBeTruthy();
  });

  it("calls a refusal a refusal, and keeps it out of the error tone", async () => {
    const conn = fakeConnection({
      runs: [
        runRow({
          id: "run-1",
          status: "failed",
          errorCode: "run_refused",
          errorMessage: "agentId is required",
        }),
      ],
      steps: [],
    });
    await openRun(conn);
    expect(screen.getByText("This run was turned down before it started.")).toBeTruthy();
    expect(screen.queryByText("This run failed.")).toBeNull();
    expect(screen.getByText("agentId is required")).toBeTruthy();
    // Nothing broke, so nothing raises an alert -- an error tone sends people
    // to the logs, and the answer is in what was asked for.
    expect(document.querySelector('.os-notice[data-tone="error"]')).toBeNull();
    expect(document.querySelector('.os-notice[data-tone="warn"]')).toBeTruthy();
  });

  // THE REPORTED BUG, AS THE PAGE SAW IT. The Materializer wrapped its one
  // model call in a fixed three-minute deadline; a long document blew through
  // it, the composition row was written `failed`, and the error said "context
  // deadline exceeded" -- which the symptom table read as a blip, so the run
  // parked at "Waiting" over a document the database had already given up on.
  // The deadline is gone and the run goes terminal now; this is what the
  // person who was watching that spinner should read instead.
  it("says the document was not made, rather than pointing at a classifier", async () => {
    const conn = fakeConnection({
      runs: [
        runRow({
          id: "run-1",
          status: "failed",
          errorCode: "composition_failed",
          errorMessage: "composition_failed: compose: composing the draft failed",
        }),
      ],
      steps: fiveSteps(),
    });
    await openRun(conn);
    expect(screen.getByText("The document was not made.")).toBeTruthy();
    expect(screen.queryByText("This run failed.")).toBeNull();
    // The absent file is the headline, and the reader must not be sent to
    // press retry: another attempt reads the same failed record.
    expect(screen.getByText(/no file and no partial one/)).toBeTruthy();
    expect(screen.getByText(/reads the same failed record/)).toBeTruthy();
  });

  // A limit we chose, said as ours. Goals carry no duration ceiling, so this
  // notice appears only where somebody configured one -- and then it must keep
  // the reader out of the network logs and keep their own number visible as
  // the thing to change.
  it("owns a self-imposed deadline instead of blaming the far side", async () => {
    const conn = fakeConnection({
      runs: [
        runRow({
          id: "run-1",
          status: "failed",
          errorCode: "self_timeout",
          errorMessage: "agent: turn wallclock timeout after 3m0s",
        }),
      ],
      steps: fiveSteps(),
    });
    await openRun(conn);
    expect(screen.getByText("This stopped on a deadline this system set for itself.")).toBeTruthy();
    expect(screen.queryByText("This run failed.")).toBeNull();
    expect(screen.getByText(/Nothing on the far side failed/)).toBeTruthy();
    expect(screen.getByText(/Goals carry no time limit of their own/)).toBeTruthy();
  });
});

describe("the journal", () => {
  it("does NOT read on open, and says it has not looked", async () => {
    const conn = fakeConnection({
      runs: [runRow({ id: "run-1" })],
      steps: fiveSteps(),
      modelCalls: [],
    });
    await openRun(conn);
    expect(screen.getByText("Not read yet")).toBeTruthy();
    expect(conn.query.workModelCallsForOwnerRun).not.toHaveBeenCalled();
  });

  it("reads both halves when asked, and prints when it looked", async () => {
    const conn = fakeConnection({
      runs: [runRow({ id: "run-1" })],
      steps: fiveSteps(),
      modelCalls: [
        {
          id: "m1",
          runId: "run-1",
          stepKey: "classify",
          model: "claude-sonnet",
          served: "journal",
          inputTokens: 900,
          outputTokens: 340,
          cost: 0.0041,
          latencyMs: 2100,
          error: "",
          createdAt: "2026-09-01T09:02:00Z",
        },
      ],
      observations: [
        {
          id: "o1",
          runId: "run-1",
          stepKey: "fetchLedger",
          kind: "tool_result",
          content: "412 rows",
          createdAt: "2026-09-01T09:01:00Z",
        },
      ],
    });
    await openRun(conn);
    fireEvent.click(screen.getByText("Read the journal"));
    await waitFor(() => expect(conn.query.workModelCallsForOwnerRun).toHaveBeenCalled());
    expect(conn.query.workObservationsForOwnerRun).toHaveBeenCalled();
    expect(await screen.findByText("1 model call")).toBeTruthy();
    expect(screen.getByText("served from the journal")).toBeTruthy();
    expect(screen.getByText("1 observation")).toBeTruthy();
    expect(screen.getByText(/^Read at /)).toBeTruthy();
    // It says what an on-demand read costs, rather than implying liveness.
    expect(screen.getByText(/A call made since you looked is not here/)).toBeTruthy();
  });

  it("renders the server's own sentence when the read refuses", async () => {
    const conn = fakeConnection({
      runs: [runRow({ id: "run-1" })],
      steps: fiveSteps(),
      modelCalls: new Error("permission denied"),
    });
    await openRun(conn);
    fireEvent.click(screen.getByText("Read the journal"));
    expect(await screen.findByText("permission denied")).toBeTruthy();
    expect(screen.getByText("The journal could not be read.")).toBeTruthy();
  });
});

describe("the steps feed", () => {
  it("is opened by the RUN PAGE and not by the app root", async () => {
    // A per-run timeline retained at the root would subscribe a window to
    // every step of every run this person owns in order to draw one of them.
    const conn = fakeConnection({ runs: [runRow({ id: "run-1" })], steps: fiveSteps() });
    mount(conn);
    await screen.findByText("nightlyReconcile");
    expect(conn.query.workStepsForOwnerRun).not.toHaveBeenCalled();

    fireEvent.click(screen.getByText("nightlyReconcile"));
    await waitFor(() => expect(conn.query.workStepsForOwnerRun).toHaveBeenCalled());
    expect(conn.query.workStepsForOwnerRun.mock.calls[0]?.[0]).toEqual({ runId: "run-1" });
  });

  it("folds a live step update into the timeline in place", async () => {
    const conn = fakeConnection({ runs: [runRow({ id: "run-1" })], steps: fiveSteps() });
    const timeline = await openRun(conn);
    expect(within(timeline).getByLabelText(/Step 5, notify, Human,.*Waiting/)).toBeTruthy();

    await act(async () => {
      conn.subscriptions.emit(
        STEP,
        stepRow({ id: "s5", key: "notify", seq: 4, kind: "human", status: "done" }),
      );
    });
    await waitFor(() =>
      expect(within(timeline).getByLabelText(/Step 5, notify, Human,.*Done/)).toBeTruthy(),
    );
  });
});

describe("what a run is for", () => {
  it("leads with the goal's own words, not the automation's name", async () => {
    const conn = fakeConnection({
      goals: [goalRow({ id: "g1", statement: "Reconcile the ledger" })],
      runs: [runRow({ id: "run-1", goalId: "g1" })],
      steps: fiveSteps(),
    });
    await openRun(conn);
    expect(screen.getByText("Reconcile the ledger")).toBeTruthy();
  });

  it("says so plainly when no goal asked for it", async () => {
    const conn = fakeConnection({ runs: [runRow({ id: "run-1", goalId: "" })], steps: fiveSteps() });
    await openRun(conn);
    expect(
      screen.getByText("No goal asked for this run -- it is an automation execution."),
    ).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// Which door answered, on the step that asked (memql#5157, design record D2)
// ---------------------------------------------------------------------------

/** Four steps: one that never called a model, and one per door. */
function decisionSteps() {
  return [
    stepRow({ id: "d1", key: "fetchLedger", seq: 0, durationMs: 38 }),
    stepRow({ id: "d2", key: "classify", seq: 1, kind: "reasoning", durationMs: 2100 }),
    stepRow({ id: "d3", key: "summarise", seq: 2, kind: "reasoning", durationMs: 900 }),
    stepRow({ id: "d4", key: "draft", seq: 3, kind: "reasoning", durationMs: 12 }),
  ];
}

/**
 * One call per door. The steps above carry NO cost of their own, so every
 * money figure on the page below comes from a decision line -- otherwise "the
 * fleet line shows no money" would be asserting against the step's own spend
 * readout rather than against the line under test.
 */
function decisionCalls() {
  return [
    {
      id: "m1",
      runId: "run-1",
      stepKey: "classify",
      provider: "anthropic",
      model: "claude-sonnet-4",
      served: "live",
      inputTokens: 900,
      outputTokens: 340,
      cost: 0.0041,
      latencyMs: 2100,
      error: "",
      createdAt: "2026-09-01T09:02:00Z",
    },
    {
      // A fleet call reports no cost at all, because nothing was billed.
      id: "m2",
      runId: "run-1",
      stepKey: "summarise",
      provider: "",
      model: "qwen3:8b",
      served: "local",
      inputTokens: 400,
      outputTokens: 120,
      latencyMs: 800,
      error: "",
      createdAt: "2026-09-01T09:03:00Z",
    },
    {
      id: "m3",
      runId: "run-1",
      stepKey: "draft",
      provider: "anthropic",
      model: "claude-haiku",
      served: "journal",
      inputTokens: 120,
      outputTokens: 40,
      cost: 0,
      latencyMs: 4,
      error: "",
      createdAt: "2026-09-01T09:04:00Z",
    },
  ];
}

/** The decision comes from the journal, which is an on-demand read. */
async function openRunAndReadJournal(conn: Conn) {
  const timeline = await openRun(conn);
  fireEvent.click(screen.getByText("Read the journal"));
  await waitFor(() => expect(conn.query.workModelCallsForOwnerRun).toHaveBeenCalled());
  await screen.findByText(/^Read at /);
  return timeline;
}

describe("each step's decision", () => {
  it("names your own fleet, and puts no money on it", async () => {
    // The claim this epic exists to make legible: a call your own hardware
    // answered was not billed, and the row must not leave a reader hunting
    // for a charge that does not exist.
    const conn = fakeConnection({
      runs: [runRow({ id: "run-1" })],
      steps: decisionSteps(),
      modelCalls: decisionCalls(),
    });
    const timeline = await openRunAndReadJournal(conn);
    const row = within(timeline).getByLabelText(/Step 3, summarise/);
    const line = row.querySelector(".os-nexus-step-decision");
    expect(line?.textContent).toBe("your fleet · qwen3:8b");
    // Asserted on the LINE and on the name, because a money figure could
    // arrive in either and each channel has to stand on its own.
    expect(line?.textContent ?? "").not.toContain("$");
    expect(row.getAttribute("aria-label") ?? "").not.toContain("$");
  });

  it("names the vendor that answered, and what it cost", async () => {
    const conn = fakeConnection({
      runs: [runRow({ id: "run-1" })],
      steps: decisionSteps(),
      modelCalls: decisionCalls(),
    });
    const timeline = await openRunAndReadJournal(conn);
    expect(within(timeline).getByText("anthropic · claude-sonnet-4 · $0.0041")).toBeTruthy();
  });

  it("says a replayed step made no call, which is the whole product claim", async () => {
    const conn = fakeConnection({
      runs: [runRow({ id: "run-1" })],
      steps: decisionSteps(),
      modelCalls: decisionCalls(),
    });
    const timeline = await openRunAndReadJournal(conn);
    expect(within(timeline).getByText("replayed · claude-haiku, no call made")).toBeTruthy();
  });

  it("puts NO decision line on a step that called no model -- not a dash, nothing", async () => {
    // Most of a run is deterministic. A dash on every free row to say "no
    // model was involved" is a row of things to read past, and the spine has
    // already said which steps thought.
    const conn = fakeConnection({
      runs: [runRow({ id: "run-1" })],
      steps: decisionSteps(),
      modelCalls: decisionCalls(),
    });
    const timeline = await openRunAndReadJournal(conn);
    expect(timeline.querySelectorAll(".os-nexus-step-decision")).toHaveLength(3);
    const free = within(timeline).getByLabelText(/Step 1, fetchLedger/);
    expect(free.querySelector(".os-nexus-step-decision")).toBeNull();
  });

  it("says the decision in WORDS too, or the epic's point is sighted-only", async () => {
    // The file's own contract: the accessible name says everything the drawing
    // says. A visible line about who was billed, absent from the name, is that
    // contract broken in the one place it matters most.
    const conn = fakeConnection({
      runs: [runRow({ id: "run-1" })],
      steps: decisionSteps(),
      modelCalls: decisionCalls(),
    });
    const timeline = await openRunAndReadJournal(conn);
    expect(
      within(timeline).getByLabelText(
        "Step 2, classify, Reasoning, called a model, Done, anthropic · claude-sonnet-4 · $0.0041",
      ),
    ).toBeTruthy();
    expect(
      within(timeline).getByLabelText(
        "Step 4, draft, Reasoning, called a model, Done, replayed · claude-haiku, no call made",
      ),
    ).toBeTruthy();
  });

  it("shows nothing until the journal is read, and never claims a door it was not told about", async () => {
    // `useJournal` does not read on open, deliberately. Before the read this
    // window does not know which door answered, and a row cannot say what it
    // has not been told.
    const conn = fakeConnection({
      runs: [runRow({ id: "run-1" })],
      steps: decisionSteps(),
      modelCalls: decisionCalls(),
    });
    const timeline = await openRun(conn);
    expect(timeline.querySelectorAll(".os-nexus-step-decision")).toHaveLength(0);
  });
});
