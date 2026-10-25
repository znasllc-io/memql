import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { readTheBar } from "../readTheBar";
import type { LiveState, Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { GoalView } = await import("../../src/apps/nexus/GoalView");
const { goalFromRow } = await import("../../src/apps/nexus/rows");
const { NexusApp } = await import("../../src/apps/nexus/NexusApp");
const { LocalNexusSettingsStore } = await import("../../src/apps/nexus/settings");
const { approvalRow, builtinReply, fakeConnection, goalRow, runRow, stepRow, withSession } = await import(
  "./harness"
);

type Conn = ReturnType<typeof fakeConnection>;

// THE GOAL VIEW: the map, the rail, and the one selection they share.
//
// What a jsdom test can and cannot say about a drawing: jsdom applies no
// stylesheet and measures everything as zero, so nothing here proves a pixel.
// What IS assertable is the CONTRACT -- which nodes the layout produced, what
// each one says in words, and the attributes the stylesheet selects on. The
// picture itself was checked by eye in a browser; the README records that.

function memoryStore() {
  const bag = new Map<string, string>();
  return new LocalNexusSettingsStore({
    getItem: (k: string) => bag.get(k) ?? null,
    setItem: (k: string, v: string) => void bag.set(k, v),
  });
}

function mount(connection: Conn, intent?: { id: string; payload: Record<string, unknown> }) {
  h.connection = connection;
  return render(
    withSession(
      <NexusApp
        sectionId="goals"
        navigate={vi.fn()}
        askContext={vi.fn()}
        intent={intent}
        consumeIntent={vi.fn()}
        store={memoryStore()}
      />,
    ),
  );
}

const CHAIN: Row[] = [
  stepRow({ id: "s1", key: "read", seq: 0, runId: "r1", status: "done" }),
  stepRow({
    id: "s2",
    key: "decide",
    seq: 1,
    runId: "r1",
    dependsOn: ["read"],
    kind: "reasoning",
    status: "running",
  }),
  stepRow({
    id: "s3",
    key: "write",
    seq: 2,
    runId: "r1",
    dependsOn: ["decide"],
    status: "pending",
  }),
];

function openGoal(): Promise<HTMLElement> {
  return screen.findByText("Reconcile last month's ledger against the bank export").then((el) => {
    fireEvent.click(el);
    return screen.findByRole("application");
  });
}

function seeded(over: { steps?: Row[]; runs?: Row[]; approvals?: Row[] } = {}): Conn {
  return fakeConnection({
    goals: [goalRow({ id: "g1" })],
    runs: over.runs ?? [runRow({ id: "r1", goalId: "g1", status: "running", finishedAt: "" })],
    steps: over.steps ?? CHAIN,
    approvals: over.approvals ?? [],
  });
}

describe("the map draws what the rows say", () => {
  it("puts you at the start, the automation next, and the goal at the far end", async () => {
    mount(seeded());
    await openGoal();
    expect(screen.getByLabelText("Start, where the goal was set")).toBeTruthy();
    expect(screen.getByLabelText(/^Automation Work begins/)).toBeTruthy();
    // The beacon carries the goal's own words and how far the work has got.
    // Two readings of the same fact -- the count under the ring and the map's
    // own accessible name -- which is exactly the "colour is never the only
    // carrier" rule applied to a number.
    expect(screen.getAllByText("1 of 3 steps complete").length).toBeGreaterThan(0);
    expect(screen.getByRole("application").getAttribute("aria-label")).toContain(
      "1 of 3 steps done",
    );
  });

  it("gives every step a node that says its state in words", async () => {
    mount(seeded());
    await openGoal();
    expect(screen.getByLabelText("Step Read, done")).toBeTruthy();
    expect(screen.getByLabelText("Step Decide, running")).toBeTruthy();
    expect(screen.getByLabelText("Step Write, pending")).toBeTruthy();
  });

  it("says the run is working it out rather than drawing a fraction of nothing", async () => {
    mount(
      seeded({
        runs: [runRow({ id: "r1", goalId: "g1", status: "compiling", finishedAt: "" })],
        steps: [],
      }),
    );
    await openGoal();
    expect(screen.getByText("working it out")).toBeTruthy();
    // NOT "0 of 0" -- a compiling run has no denominator, and 0% reads as a
    // run that has failed to do anything.
    expect(screen.queryByText("0 of 0")).toBeNull();
  });

  it("hangs a waiting approval under its step and says who it is waiting on", async () => {
    mount(
      seeded({
        approvals: [approvalRow({ id: "a1", runId: "r1", stepKey: "decide" })],
      }),
    );
    await openGoal();
    expect(screen.getByLabelText("Waiting on you: Review a decision")).toBeTruthy();
  });
});

describe("attempts without recorded actions", () => {
  it("does not infer an empty journal until the disconnected history has been read", async () => {
    const rawGoal = goalRow({ id: "g1" });
    const run = runRow({ id: "r1", goalId: "g1", status: "failed", errorMessage: "An unclassified failure" });
    const props = {
      goal: goalFromRow(rawGoal), goalRow: rawGoal, runRows: [run], stepRows: [], artifactRows: [], approvalRows: [],
      openRunId: "r1", onPickRun: vi.fn(), onBack: vi.fn(), onOpenRun: vi.fn(), onOpenApproval: vi.fn(),
      cancel: { busy: false, error: "", reset: vi.fn(), cancel: vi.fn(async () => true) },
      derive: { busy: false, error: "", reset: vi.fn(), derivedRunId: "", replay: vi.fn(async () => "") },
    };
    const draw = (stepsState: LiveState) => withSession(<GoalView {...props} stepsState={stepsState} />);
    const view = render(draw("disconnected"));
    for (const state of ["disconnected", "degraded", "seeding"] as const) {
      view.rerender(draw(state));
      const progress = screen.getByRole("region", { name: "Current progress" });
      expect(within(progress).getByText(state === "seeding" ? "Loading recorded actions." : "The action history is unavailable. Reconnect to read this attempt.")).toBeTruthy();
      expect(screen.queryByText("This attempt failed before any actions were recorded.")).toBeNull();
      expect(screen.queryByText("No actions recorded")).toBeNull();
      expect(screen.queryByText("This run recorded no steps.")).toBeNull();
      expect(screen.queryByText("No action was recorded as running when it stopped.")).toBeNull();
      expect(screen.queryByLabelText("No recorded steps.")).toBeNull();
      expect(screen.queryByRole("application")).toBeNull();
      await readTheBar();
      expect(screen.queryByText("No recorded actions to review")).toBeNull();
    }
    view.rerender(draw("live"));
    expect(within(screen.getByRole("region", { name: "Current progress" })).getByText("This attempt failed before any actions were recorded.")).toBeTruthy();
    expect(screen.getByText("No actions recorded")).toBeTruthy();
    expect(screen.getByText("This run recorded no steps.")).toBeTruthy();
    expect(screen.getByLabelText("No recorded steps.")).toBeTruthy();
    await readTheBar();
    expect(screen.getByText("No recorded actions to review")).toBeTruthy();
    expect(props.cancel.cancel).not.toHaveBeenCalled();
    expect(props.derive.replay).not.toHaveBeenCalled();
  });

  it("summarizes the recorded planning refusal without exposing provider diagnostics as the main explanation", async () => {
    // The live pre-step failure shape; identifiers and model name are neutral fixtures.
    const diagnostic = 'work compile: intent classification failed; retry the request: ai call via provider "fleet:example-model" failed: run v1:work:run:fixture reached its unevaluated ceiling at step <unnamed step> (limit , ): provider admission could not be verified: work is no longer running under this goal';
    mount(seeded({ steps: [], runs: [runRow({ id: "r1", goalId: "g1", status: "failed", errorMessage: diagnostic })] }));
    await openGoal();
    const progress = screen.getByRole("region", { name: "Current progress" });
    expect(within(progress).getByText("Planning stopped because permission to use the model could not be verified.")).toBeTruthy();
    expect(within(progress).queryByText(diagnostic)).toBeNull();
    const disclosure = screen.getByText("Usage and execution details").closest("details")!;
    expect(disclosure.open).toBe(false);
    expect(within(disclosure).getByText(diagnostic)).toBeTruthy();
    expect(within(disclosure).getByText("No recorded steps to summarize.")).toBeTruthy();
    expect(within(disclosure).getByText("No action was recorded as running when it stopped.")).toBeTruthy();
    expect(within(screen.getByRole("region", { name: "Goal journey" })).getByText("No actions recorded")).toBeTruthy();
    expect(screen.queryByText("The work is taking shape")).toBeNull();
    expect(screen.queryByText("The next action has not been recorded yet.")).toBeNull();
    expect(screen.queryByText(/The band fills in/)).toBeNull();
    await readTheBar();
    expect(screen.queryByText("Select an action to review its result")).toBeNull();
    expect(screen.getByText("No recorded actions to review")).toBeTruthy();
  });

  it.each([
    { status: "failed", errorCode: "", errorMessage: "Unrecognized technical failure ref-123", summary: "This attempt failed before any actions were recorded." },
    { status: "failed", errorCode: "run_refused", errorMessage: "The saved gate refused the request", summary: "This request was refused before any actions were recorded." },
    { status: "cancelled", summary: "This attempt was stopped before any actions were recorded." },
    { status: "abandoned", summary: "The process running this attempt was lost. No actions were recorded." },
    { status: "succeeded", summary: "This attempt finished without recorded actions." },
  ])("keeps an empty $status attempt in the past without inventing a reason", async ({ status, summary, ...fields }) => {
    mount(seeded({ steps: [], runs: [runRow({ id: "r1", goalId: "g1", status, ...fields })] }));
    await openGoal();
    expect(within(screen.getByRole("region", { name: "Current progress" })).getByText(summary)).toBeTruthy();
    expect(within(screen.getByRole("region", { name: "Goal journey" })).getByText("No actions recorded")).toBeTruthy();
    expect(screen.queryByText(/The band fills in/)).toBeNull();
    await readTheBar();
    expect(screen.queryByText("Select an action to review its result")).toBeNull();
  });

  it("shows the actual wait before the first action instead of suggesting planning is running", async () => {
    mount(seeded({ steps: [], runs: [runRow({ id: "r1", goalId: "g1", status: "waiting", finishedAt: "", waitingOn: { kind: "external", subject: "fixture" } })] }));
    await openGoal();
    expect(within(screen.getByRole("region", { name: "Current progress" })).getByText("Waiting for something outside the cluster. No actions have been recorded.")).toBeTruthy();
    expect(screen.queryByText("The work is taking shape")).toBeNull();
    expect(screen.queryByText(/The band fills in/)).toBeNull();
    await readTheBar();
    expect(screen.queryByText("Follow the actions above to see what happens next")).toBeNull();
  });
});

describe("one selection, shared by the map and the rail", () => {
  it("opens the rail's step when its node is clicked", async () => {
    mount(seeded());
    await openGoal();
    fireEvent.click(screen.getByLabelText("Step Decide, running"));
    // The rail row for that step is now expanded, which is the shared
    // selection arriving on the other surface.
    const row = screen.getByRole("button", { name: /^Decide, / });
    expect(row.getAttribute("aria-expanded")).toBe("true");
  });

  it("keeps the selection to ONE step, so opening another closes the first", async () => {
    mount(seeded());
    await openGoal();
    fireEvent.click(screen.getByLabelText("Step Decide, running"));
    fireEvent.click(screen.getByLabelText("Step Write, pending"));
    expect(
      screen.getByRole("button", { name: /^Decide, / }).getAttribute("aria-expanded"),
    ).toBe("false");
    expect(
      screen.getByRole("button", { name: /^Write, / }).getAttribute("aria-expanded"),
    ).toBe("true");
  });
});

describe("density: a finished stretch folds and says how much it stands for", () => {
  it("folds, and opens again when the fold is clicked", async () => {
    const long: Row[] = [];
    for (let i = 0; i < 6; i += 1) {
      long.push(
        stepRow({
          id: `s${i}`,
          key: `s${i}`,
          seq: i,
          runId: "r1",
          status: "done",
          dependsOn: i === 0 ? [] : [`s${i - 1}`],
        }),
      );
    }
    mount(seeded({ steps: long }));
    await openGoal();
    const fold = screen.getByLabelText("6 finished steps, folded. Open to see them.");
    expect(fold).toBeTruthy();
    // The steps it stands for are not also drawn.
    expect(screen.queryByLabelText("Step s0, done")).toBeNull();

    fireEvent.click(fold);
    expect(document.querySelectorAll('.os-nexus-node[data-kind="step"]')).toHaveLength(6);
    expect(screen.queryByLabelText("6 finished steps, folded. Open to see them.")).toBeNull();
  });
});

describe("two expanders against one render", () => {
  // THE FAILING CASE IS TWO CLICKS INSIDE ONE `act`. A value-taking setter
  // reads the set from the render's closure, so the second write lands over
  // the first and one expander silently does nothing -- and every other case
  // in this file clicks once per render, which is why a green suite says
  // nothing about it. Both handles come from ONE render on purpose.
  it("keeps both open", async () => {
    const long: Row[] = [];
    for (let i = 0; i < 6; i += 1) {
      long.push(
        stepRow({
          id: `a${i}`,
          key: `a${i}`,
          seq: i,
          runId: "r1",
          status: "done",
          dependsOn: i === 0 ? [] : [`a${i - 1}`],
        }),
      );
    }
    // A second, separate finished stretch after a step that is still running.
    long.push(
      stepRow({ id: "mid", key: "mid", seq: 6, runId: "r1", status: "running", dependsOn: ["a5"], finishedAt: "" }),
    );
    for (let i = 0; i < 4; i += 1) {
      long.push(
        stepRow({
          id: `b${i}`,
          key: `b${i}`,
          seq: 7 + i,
          runId: "r1",
          status: "done",
          dependsOn: [i === 0 ? "mid" : `b${i - 1}`],
        }),
      );
    }
    mount(seeded({ steps: long }));
    await openGoal();

    const folds = screen.getAllByLabelText(/finished steps, folded/);
    expect(folds).toHaveLength(2);
    // BOTH HANDLES FROM ONE RENDER, clicked inside a single act.
    act(() => {
      fireEvent.click(folds[0]!);
      fireEvent.click(folds[1]!);
    });
    expect(screen.queryAllByLabelText(/finished steps, folded/)).toHaveLength(0);
  });
});

describe("rewind", () => {
  // "Does nothing" is illegal, and rule 12 says an illegal act is ABSENT. A
  // goal with no runs still dates its own creation, so a `count > 0` guard
  // offered Rewind on a goal there is nothing to scrub through.
  it("is not offered when there is only one moment to stand at", async () => {
    mount(seeded({ runs: [], steps: [] }));
    // Opened WITHOUT waiting for the map: a goal with no runs draws none, so
    // `openGoal`'s wait would never resolve.
    fireEvent.click(
      await screen.findByText("Reconcile last month's ledger against the bank export"),
    );
    await screen.findByRole("group", { name: "What you can do with this" });
    expect(screen.queryByRole("button", { name: /^Rewind/ })).toBeNull();
    expect(screen.getByText("No attempt is available yet")).toBeTruthy();
  });

  it("is offered, and says plainly that it spends nothing", async () => {
    mount(seeded());
    await openGoal();
    const rewind = screen.getByRole("button", { name: /^Rewind/ });
    expect(rewind.getAttribute("aria-label")).toContain("Nothing runs and nothing is spent");
  });

  // The whole point of the words being different: Rewind reads rows this
  // browser already has; Replay opens a NEW run and costs money.
  it("does not call replayRun when the goal is rewound", async () => {
    const conn = seeded();
    mount(conn);
    await openGoal();
    fireEvent.click(screen.getByRole("button", { name: /^Rewind/ }));
    expect(conn.query.replayRun).not.toHaveBeenCalled();
    expect(screen.getByLabelText("Scrub through what happened")).toBeTruthy();
  });

  it("says which moment it is drawing, so a rewound picture never lies", async () => {
    mount(seeded());
    await openGoal();
    fireEvent.click(screen.getByRole("button", { name: /^Rewind/ }));
    const map = screen.getByRole("application");
    expect(map.getAttribute("aria-label")).toContain("as it stood at");
  });

  it("comes back to now, and the scrubber goes with it", async () => {
    mount(seeded());
    await openGoal();
    fireEvent.click(screen.getByRole("button", { name: /^Rewind/ }));
    fireEvent.click(screen.getByRole("button", { name: "Back to now" }));
    expect(screen.queryByLabelText("Scrub through what happened")).toBeNull();
  });

  // The OS has no per-window URL; the shell's deep-link primitive is the open
  // intent, so a moment is shareable through it.
  it("opens rewound when an opener hands in a moment", async () => {
    mount(seeded(), {
      id: "i1",
      payload: { goalId: "g1", at: "2026-09-01T09:00:11Z" },
    });
    const map = await screen.findByRole("application");
    // The moment is spoken in a PERSON'S format, not the wire's: this string
    // is read aloud, and an RFC3339 stamp spoken out is worse than no answer.
    expect(map.getAttribute("aria-label")).toContain("as it stood at");
    expect(map.getAttribute("aria-label")).not.toContain("2026-09-01T09:00:11Z");
  });

  // THE PAGE SHOWS ONE MOMENT. A rewound map beside a live rail says three
  // steps have landed while the list under it says eleven, which is worse than
  // not offering rewind at all.
  it("rewinds the rail with the map, not just the map", async () => {
    mount(seeded(), {
      id: "i1",
      // After `read` was created and before `decide` finished.
      payload: { goalId: "g1", at: "2026-09-01T09:00:11Z" },
    });
    await screen.findByRole("application");
    const rail = screen.getAllByRole("button", { name: /^(Read|Decide|Write), / });
    // Every rail row agrees with the map: nothing is `done` at a moment when
    // the run had not finished a step.
    for (const row of rail) {
      expect(row.getAttribute("aria-label")).not.toContain("done");
    }
  });
});

describe("the acts legal from the run's state", () => {
  // A goal view that drew the pause without offering the answer would be a
  // picture of being stuck.
  it("offers the answer when the run is parked on a person, and says why", async () => {
    mount(
      seeded({
        runs: [
          runRow({
            id: "r1",
            goalId: "g1",
            status: "waiting",
            finishedAt: "",
            waitingOn: { kind: "approval", subject: "a1" },
          }),
        ],
        approvals: [
          approvalRow({
            id: "a1",
            runId: "r1",
            stepKey: "decide",
            evidence: { tier: "B", reason: "sends mail outside the cluster" },
          }),
        ],
      }),
    );
    await openGoal();
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).getByText("Answer it")).toBeTruthy();
    expect(screen.getByText(/sends mail outside the cluster/)).toBeTruthy();
  });

  it("offers no Replay while the run is still going, and says why", async () => {
    mount(seeded());
    await openGoal();
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).queryByText("Replay")).toBeNull();
    expect(screen.getByText(/Follow the actions above to see what happens next/)).toBeTruthy();
  });

  it("offers Replay once the run is terminal", async () => {
    mount(
      seeded({
        runs: [runRow({ id: "r1", goalId: "g1", status: "succeeded" })],
        steps: CHAIN.map((s) => ({ ...s, status: "done" })),
      }),
    );
    await openGoal();
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).getByText("Replay")).toBeTruthy();
  });

  it("keeps all terminal lifecycle actions in the footer and Rewind beside the time controls", async () => {
    const conn = seeded({ runs: [runRow({ id: "r1", goalId: "g1", status: "succeeded" })], steps: CHAIN.map(step => ({ ...step, status: "done" })) });
    mount(conn);
    await openGoal();
    fireEvent.click(screen.getByRole("button", { name: "Read, Done" }));
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).getAllByRole("button")).toHaveLength(3);
    expect(within(bar).getByRole("button", { name: /^Branch from read:/ })).toBeTruthy();
    expect(within(bar).getByRole("button", { name: /^Replay this run/ })).toBeTruthy();
    expect(within(bar).getByRole("button", { name: "Close goal" })).toBeTruthy();
    expect(within(bar).queryByRole("button", { name: /^Rewind/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /^Rewind/ }));
    expect(screen.getByLabelText("Scrub through what happened")).toBeTruthy();
    expect(conn.query.replayRun).not.toHaveBeenCalled();
    expect(conn.query.cancelGoal).not.toHaveBeenCalled();
  });

  it("keeps a long request available and lets the person cancel a brief close confirmation", async () => {
    const statement = "Please reconcile the qualification evidence and keep every source receipt available for a later review.\n\nDo not change permissions or repeat the deployment.";
    const conn = fakeConnection({ goals: [goalRow({ id: "g1", statement })] });
    mount(conn, { id: "long-request", payload: { goalId: "g1" } });
    const heading = await screen.findByRole("heading", { name: /Please reconcile.*…/ });
    expect(heading.textContent!.length).toBeLessThanOrEqual(81);
    const request = screen.getByText("Request").closest("details")!;
    fireEvent.click(within(request).getByText("Request"));
    expect(request.textContent).toContain(statement);
    fireEvent.click(screen.getByRole("button", { name: "Close goal" }));
    expect(screen.queryByRole("textbox")).toBeNull();
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: "Keep open" }));
    expect(conn.query.cancelGoal).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Close goal" }));
  });

  it("preserves pending and refused closing without asking for the request text", async () => {
    const conn = fakeConnection({ goals: [goalRow({ id: "g1", statement: "Reconcile the ledger.\n\nReport all differences." })] });
    let refuse!: (error: Error) => void;
    conn.query.cancelGoal.mockImplementationOnce(() => new Promise((_, reject) => { refuse = reject; }));
    mount(conn, { id: "multiline-close", payload: { goalId: "g1" } });
    fireEvent.click(await screen.findByRole("button", { name: "Close goal" }));
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: /Confirm closing this goal/ }));
    expect(conn.query.cancelGoal).toHaveBeenCalledExactlyOnceWith({ goalId: "g1", reason: "Closed from MemQL OS" });
    const pending = screen.getByRole("button", { name: /Confirm closing this goal/ }) as HTMLButtonElement;
    expect(pending.disabled).toBe(true);
    fireEvent.click(pending);
    expect(conn.query.cancelGoal).toHaveBeenCalledTimes(1);
    await act(async () => refuse(new Error("The cluster refused this request.")));
    expect(screen.getByText("Closing the goal needs attention.")).toBeTruthy();
    expect(screen.getByText("The cluster refused this request.")).toBeTruthy();
    conn.query.cancelGoal.mockResolvedValueOnce(builtinReply("cancelGoal", [{ goalId: "g1", goalClosed: true, runsAsked: 1 }]));
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: /Confirm closing this goal/ }));
    await act(async () => {});
    expect(conn.query.cancelGoal).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("button", { name: /Confirm closing this goal/ })).toBeNull();
  });

  it.each([
    [{ goalId: "g1", goalClosed: false, runsAsked: 1 }, "Some runs may have been asked to stop, but the goal did not close. Check its current state."],
    [{ goalId: "g1", goalClosed: true, runsAsked: 1, runsRefused: ["r2"] }, "The goal closed, but some runs could not be asked to stop. Check their current activity."],
    [{ goalId: "other", goalClosed: true }, "The cluster did not confirm whether this goal closed. Check its current state before trying again."],
  ] as const)("does not turn a partial or unrelated close receipt into success", async (reply, message) => {
    const conn = seeded();
    conn.query.cancelGoal.mockResolvedValueOnce(builtinReply("cancelGoal", [reply]));
    mount(conn);
    await openGoal();
    fireEvent.click(screen.getByRole("button", { name: "Close goal" }));
    await readTheBar();
    fireEvent.click(screen.getByRole("button", { name: /Confirm closing this goal/ }));
    expect(await screen.findByText(message)).toBeTruthy();
    expect(conn.query.cancelGoal).toHaveBeenCalledTimes(1);
  });

});

describe("the receipt", () => {
  it("is absent while the run is going, and present once it ends", async () => {
    mount(seeded());
    await openGoal();
    expect(screen.queryByRole("region", { name: "What it cost" })).toBeNull();
  });

  it("gives a failed run the same card, with the failure and the step in flight", async () => {
    mount(
      seeded({
        runs: [
          runRow({
            id: "r1",
            goalId: "g1",
            status: "failed",
            errorMessage: "the provider refused",
          }),
        ],
      }),
    );
    await openGoal();
    // `Panel` names itself through aria-label, not through text.
    expect(screen.getByRole("region", { name: "What it cost" })).toBeTruthy();
    expect(screen.getByText("the provider refused")).toBeTruthy();
    expect(screen.getByText(/decide was running when it stopped/)).toBeTruthy();
  });
});
