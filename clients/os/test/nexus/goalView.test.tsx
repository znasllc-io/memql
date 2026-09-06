import { fireEvent, render, screen, within } from "@testing-library/react";
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
const { approvalRow, fakeConnection, goalRow, runRow, stepRow, withSession } = await import(
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
    expect(screen.getByLabelText("You, where the goal was set")).toBeTruthy();
    expect(screen.getByLabelText(/^Automation nightlyReconcile/)).toBeTruthy();
    // The beacon carries the goal's own words and how far the work has got.
    // Two readings of the same fact -- the count under the ring and the map's
    // own accessible name -- which is exactly the "colour is never the only
    // carrier" rule applied to a number.
    expect(screen.getAllByText("1 of 3").length).toBeGreaterThan(0);
    expect(screen.getByRole("application").getAttribute("aria-label")).toContain(
      "1 of 3 steps done",
    );
  });

  it("gives every step a node that says its state in words", async () => {
    mount(seeded());
    await openGoal();
    expect(screen.getByLabelText("Step read, done")).toBeTruthy();
    expect(screen.getByLabelText("Step decide, running")).toBeTruthy();
    expect(screen.getByLabelText("Step write, pending")).toBeTruthy();
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
    expect(screen.getByLabelText("Waiting on you: sideEffect")).toBeTruthy();
  });
});

describe("one selection, shared by the map and the rail", () => {
  it("opens the rail's step when its node is clicked", async () => {
    mount(seeded());
    await openGoal();
    fireEvent.click(screen.getByLabelText("Step decide, running"));
    // The rail row for that step is now expanded, which is the shared
    // selection arriving on the other surface.
    const row = screen.getByRole("button", { name: /^Step 2, decide/ });
    expect(row.getAttribute("aria-expanded")).toBe("true");
  });

  it("keeps the selection to ONE step, so opening another closes the first", async () => {
    mount(seeded());
    await openGoal();
    fireEvent.click(screen.getByLabelText("Step decide, running"));
    fireEvent.click(screen.getByLabelText("Step write, pending"));
    expect(
      screen.getByRole("button", { name: /^Step 2, decide/ }).getAttribute("aria-expanded"),
    ).toBe("false");
    expect(
      screen.getByRole("button", { name: /^Step 3, write/ }).getAttribute("aria-expanded"),
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
    expect(screen.getByLabelText("Step s0, done")).toBeTruthy();
    expect(screen.queryByLabelText("6 finished steps, folded. Open to see them.")).toBeNull();
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
    expect(screen.getByText("no run to act on yet")).toBeTruthy();
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
    const rail = screen.getAllByRole("button", { name: /^Step \d+, / });
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
    expect(screen.getByText(/parked on you: sends mail outside the cluster/)).toBeTruthy();
  });

  it("offers no Replay while the run is still going, and says why", async () => {
    mount(seeded());
    await openGoal();
    const bar = screen.getByRole("group", { name: "What you can do with this" });
    expect(within(bar).queryByText("Replay")).toBeNull();
    expect(screen.getByText(/replay and fork wait until the run finishes/)).toBeTruthy();
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

  it("will not close a goal until the goal's own words are typed", async () => {
    const conn = seeded();
    mount(conn);
    await openGoal();
    fireEvent.click(screen.getByText("Close this goal"));
    expect(conn.query.cancelGoal).not.toHaveBeenCalled();
    fireEvent.change(screen.getByLabelText(/^Type the goal's own words to close it/), {
      target: { value: "Reconcile last month's ledger against the bank export" },
    });
    fireEvent.click(screen.getByText("Close this goal"));
    expect(conn.query.cancelGoal).toHaveBeenCalled();
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
