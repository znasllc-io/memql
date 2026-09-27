import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { readFileSync, readdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

// Stepping into a run (epic memql#5414, issue #5419): the bar that follows the
// selection, the one composer behind Run again and Branch from here, Make
// current, the verdicts, and the validator beside them -- through the REAL app
// over the suite's connection-shaped fake, every builtin answered in its wire
// shape.

const h = vi.hoisted(() => ({ connection: null as unknown }));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

const { NexusApp } = await import("../../src/apps/nexus/NexusApp");
const { LocalNexusSettingsStore } = await import("../../src/apps/nexus/settings");
const {
  approvalRow,
  fakeConnection,
  feedbackObservation,
  goalRow,
  runRow,
  stepRow,
  stepVersionRow,
  validatorObservation,
  withSession,
} = await import("./harness");

type Conn = ReturnType<typeof fakeConnection>;
type Seed = Parameters<typeof fakeConnection>[0];

const RUN = "run-1";
const ME = "v1:identity:user:me";

const V3_OVERRIDE = {
  level: "reasoning",
  model: "app:claude-code:opus",
  effort: "high",
  prompt: "Lead with the regional totals.",
  requestedBy: ME,
};

function reportRun(over: Row = {}): Row {
  return runRow({
    id: RUN,
    goalId: "g1",
    automationName: "weeklyReport",
    status: "succeeded",
    stepOrder: ["fetch", "draft", "publish"],
    head: { fetch: { version: 1 }, draft: { version: 3 }, publish: { version: 1 } },
    ...over,
  });
}

function reportSteps(over: { draft?: Row; fetch?: Row; publish?: Row } = {}): Row[] {
  return [
    stepRow({ id: `${RUN}-fetch`, runId: RUN, key: "fetch", seq: 0, ...over.fetch }),
    stepRow({
      id: `${RUN}-draft`,
      runId: RUN,
      key: "draft",
      seq: 1,
      kind: "reasoning",
      attempt: 3,
      version: 3,
      override: V3_OVERRIDE,
      authoredBy: ME,
      result: { status: "ok", result: "Sales rose 8%." },
      ...over.draft,
    }),
    stepRow({ id: `${RUN}-publish`, runId: RUN, key: "publish", seq: 2, stepType: "mutation", ...over.publish }),
  ];
}

/** Every version: fetch v1, draft v1-v3 (v3 current), publish v1. */
function reportVersions(over: { v2?: Row } = {}): Row[] {
  return [
    stepVersionRow({ id: `${RUN}-fetch`, runId: RUN, key: "fetch", seq: 0, version: 1 }),
    stepVersionRow({ id: `${RUN}-draft`, runId: RUN, key: "draft", seq: 1, kind: "reasoning", version: 1, current: false, result: { status: "ok", result: "First draft." } }),
    stepVersionRow({ id: `${RUN}-draft`, runId: RUN, key: "draft", seq: 1, kind: "reasoning", version: 2, current: false, override: { level: "strong" }, ...over.v2 }),
    stepVersionRow({ id: `${RUN}-draft`, runId: RUN, key: "draft", seq: 1, kind: "reasoning", version: 3, current: true, override: V3_OVERRIDE, authoredBy: ME, result: { status: "ok", result: "Sales rose 8%." } }),
    stepVersionRow({ id: `${RUN}-publish`, runId: RUN, key: "publish", seq: 2, stepType: "mutation", version: 1 }),
  ];
}

function seed(over: Seed = {}): Seed {
  return {
    goals: [goalRow({ id: "g1", statement: "Summarise last week's sales" })],
    runs: [reportRun()],
    steps: reportSteps(),
    versions: reportVersions(),
    observations: [],
    ...over,
  };
}

function mount(connection: Conn) {
  h.connection = connection;
  const bag = new Map<string, string>();
  const navigate = vi.fn();
  const view = render(
    withSession(
      <NexusApp
        sectionId="runs"
        navigate={navigate}
        askContext={() => {}}
        intent={{ id: "open", payload: { runId: RUN } }}
        consumeIntent={() => {}}
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

async function openRun(over: Seed = {}): Promise<Conn> {
  const conn = fakeConnection(seed(over));
  mount(conn);
  await screen.findByLabelText("What this run did, in order");
  // The versions read and the verdicts read both land before a test acts.
  await waitFor(() => expect(conn.query.workStepVersions).toHaveBeenCalled());
  await waitFor(() => expect(conn.query.workObservationsForOwnerRun).toHaveBeenCalled());
  return conn;
}

/** The page's ONE bar -- not a dialog's floor -- and the acts it offers, by label. */
function bar(): HTMLElement {
  const groups = screen.getAllByRole("group", { name: "What you can do with this" });
  const page = groups.find((group) => group.closest("dialog") === null);
  if (page === undefined) throw new Error("no action bar on the page");
  return page;
}

function acts(): string[] {
  return within(bar())
    .queryAllByRole("button")
    .map((button) => (button.textContent ?? "").trim());
}

async function selectStep(key: string) {
  fireEvent.click(screen.getByLabelText(new RegExp(`^Step \\d+, ${key},`)));
  await screen.findByRole("heading", { name: /^Version \d/ });
}

async function composer(name: "Run again" | "Branch from here"): Promise<HTMLElement> {
  fireEvent.click(within(bar()).getByRole("button", { name: new RegExp(`^${name === "Run again" ? "Run" : "Branch"}`) }));
  return screen.findByRole("dialog", { name: new RegExp(name) });
}

describe("the bar follows the selection (rule 12)", () => {
  it("offers the run's acts with no step selected, and the step's once one is", async () => {
    await openRun();
    expect(acts()).toEqual(["Replay"]);

    await selectStep("draft");
    // The CURRENT version is picked by default, so there is nothing to make current.
    expect(acts()).toEqual(["Branch from here", "Run again"]);
    expect(within(bar()).getByText("draft, version 3 of 3")).toBeTruthy();

    fireEvent.click(screen.getByRole("radio", { name: /^Version 2/ }));
    expect(acts()).toEqual(["Make current", "Branch from here", "Run again"]);
    expect(within(bar()).getByText("draft, version 2 of 3 -- version 3 is current")).toBeTruthy();

    // Closing the step hands the bar back to the run.
    fireEvent.click(screen.getByLabelText(/^Step 2, draft,/));
    expect(acts()).toEqual(["Replay"]);
  });

  it("keeps Answer it while the run is parked on you, whatever is selected", async () => {
    await openRun({
      runs: [reportRun({ status: "waiting", waitingOn: { kind: "approval", subject: "publish" }, finishedAt: "" })],
      approvals: [approvalRow({ id: "a1", runId: RUN })],
    });
    expect(acts()).toEqual(["Answer it"]);
    await selectStep("draft");
    expect(acts()).toEqual(["Answer it"]);
  });

  it("offers no step act while a re-run is in flight, and says in words what is running", async () => {
    await openRun({
      runs: [reportRun({ status: "running", finishedAt: "", rerun: { requestId: "q1", reason: "rerun", stepKey: "draft", requestedAt: "t" } })],
      steps: reportSteps({ draft: { status: "running", version: 4, attempt: 4, override: {} } }),
    });
    expect(acts()).toEqual([]);
    expect(within(bar()).getByText("running draft again as version 4")).toBeTruthy();
    await selectStep("draft");
    expect(acts()).toEqual([]);
  });

  it("never offers Make current for a version that failed", async () => {
    await openRun({ versions: reportVersions({ v2: { status: "failed" } }) });
    await selectStep("draft");
    fireEvent.click(screen.getByRole("radio", { name: /^Version 2/ }));
    expect(acts()).toEqual(["Branch from here", "Run again"]);
  });
});

describe("Run again", () => {
  it("opens the composer, says what it will do, and sends ONLY what was changed", async () => {
    const conn = await openRun({ rerunReply: { id: "r", runId: RUN, stepKey: "draft", version: 4, staleSteps: ["draft", "publish"] } });
    await selectStep("draft");
    const dialog = await composer("Run again");
    expect(within(dialog).getByText("draft, version 3")).toBeTruthy();
    expect(within(dialog).getByText("Runs as version 4. The step after it runs again. Anything they changed outside the run is done again.")).toBeTruthy();

    fireEvent.click(within(dialog).getByRole("combobox", { name: "Level" }));
    fireEvent.click(within(dialog).getByRole("option", { name: "Reasoning" }));
    fireEvent.change(within(dialog).getByLabelText("Instructions"), { target: { value: "  Reconcile against the ledger first. " } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Run again" }));

    await waitFor(() => expect(conn.query.rerunStep).toHaveBeenCalledTimes(1));
    expect(conn.query.rerunStep.mock.calls[0]?.[0]).toEqual({
      runId: RUN,
      stepKey: "draft",
      level: "reasoning",
      prompt: "Reconcile against the ledger first.",
    });
    // THE DIALOG CLOSES ONLY ON SUCCESS, and the version it asked for is picked.
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByRole("heading", { name: /^Version 4/ })).toBeTruthy();
  });

  it("sends the step alone when nothing was changed -- a plain retry as a new version", async () => {
    const conn = await openRun();
    await selectStep("publish");
    const dialog = await composer("Run again");
    expect(within(dialog).getByText("Runs as version 2. Nothing after it runs again. Anything it changed outside the run is done again.")).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Run again" }));
    await waitFor(() => expect(conn.query.rerunStep).toHaveBeenCalled());
    expect(conn.query.rerunStep.mock.calls[0]?.[0]).toEqual({ runId: RUN, stepKey: "publish" });
  });

  it("asks only for the inputs of a step no model answers, starting from what it ran with", async () => {
    const conn = await openRun({
      steps: reportSteps({ publish: { input: { channel: "#sales", limit: 20 } } }),
      versions: reportVersions().map((row) =>
        row["key"] === "publish" ? { ...row, input: { channel: "#sales", limit: 20 } } : row,
      ),
    });
    await selectStep("publish");
    const dialog = await composer("Run again");
    // A mutation has no level, model, effort or prompt to change.
    expect(within(dialog).queryByRole("combobox", { name: "Level" })).toBeNull();
    expect(within(dialog).queryByLabelText("Instructions")).toBeNull();
    // Its recorded inputs are laid out to edit; only the one changed is sent.
    fireEvent.change(within(dialog).getByLabelText("Value of limit"), { target: { value: "50" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Run again" }));
    await waitFor(() => expect(conn.query.rerunStep).toHaveBeenCalled());
    expect(conn.query.rerunStep.mock.calls[0]?.[0]).toEqual({ runId: RUN, stepKey: "publish", inputs: { limit: 50 } });
  });

  it("keeps the composer, every field and the server's own sentence when it refuses", async () => {
    const conn = await openRun({ writeError: new Error("run_not_finished: run run-1 is still running") });
    await selectStep("draft");
    const dialog = await composer("Run again");
    fireEvent.change(within(dialog).getByLabelText("Instructions"), { target: { value: "Be brief." } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Run again" }));
    await waitFor(() => expect(conn.query.rerunStep).toHaveBeenCalled());
    expect(await within(dialog).findByText("run_not_finished: run run-1 is still running")).toBeTruthy();
    expect(within(dialog).getByText("It was not run again.")).toBeTruthy();
    expect(screen.getByRole("dialog", { name: /Run again/ })).toBeTruthy();
    expect((within(dialog).getByLabelText("Instructions") as HTMLTextAreaElement).value).toBe("Be brief.");
  });

  it("keeps what was typed through Escape, and Cancel throws it away", async () => {
    await openRun();
    await selectStep("draft");
    let dialog = await composer("Run again");
    fireEvent.change(within(dialog).getByLabelText("Instructions"), { target: { value: "Keep this." } });
    fireEvent.keyDown(dialog, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

    dialog = await composer("Run again");
    expect((within(dialog).getByLabelText("Instructions") as HTMLTextAreaElement).value).toBe("Keep this.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

    dialog = await composer("Run again");
    expect((within(dialog).getByLabelText("Instructions") as HTMLTextAreaElement).value).toBe("");
  });

  it("says what the replaced version ran with, and starts from it in one click", async () => {
    const conn = await openRun();
    await selectStep("draft");
    const dialog = await composer("Run again");
    // AN OVERRIDE NEVER CARRIES: the form says so rather than letting a plain
    // Run again quietly drop what version 3 was asked for.
    expect(within(dialog).getByText(/Version 3 ran with changes: Reasoning, app:claude-code:opus, high effort, instructions/)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Start from them" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Run again" }));
    await waitFor(() => expect(conn.query.rerunStep).toHaveBeenCalled());
    expect(conn.query.rerunStep.mock.calls[0]?.[0]).toEqual({
      runId: RUN,
      stepKey: "draft",
      level: "reasoning",
      model: "app:claude-code:opus",
      effort: "high",
      prompt: "Lead with the regional totals.",
    });
  });

  it("shows, read-only, the dislike that will be passed on", async () => {
    await openRun({
      observations: [
        feedbackObservation({ id: "f1", runId: RUN, verdict: "dislike", stepKey: "draft", version: 3, axes: { product: true }, reason: "The totals are last month's." }),
      ],
    });
    await selectStep("draft");
    const dialog = await composer("Run again");
    expect(within(dialog).getByText("What will be passed on")).toBeTruthy();
    expect(within(dialog).getByText("What was wrong with the result: The totals are last month's.")).toBeTruthy();
  });
});

describe("Branch from here", () => {
  it("branches at the step, opens the new run, and Back returns to the run it came from", async () => {
    const branch = runRow({
      id: "run-branch",
      goalId: "g1",
      automationName: "weeklyReport",
      mode: "fork",
      forkedFromRunId: RUN,
      forkAtStepKey: "draft",
      status: "running",
      finishedAt: "",
    });
    const conn = await openRun({ runs: [reportRun(), branch] });
    await selectStep("draft");
    const dialog = await composer("Branch from here");
    expect(
      within(dialog).getByText("Opens a new run from this step. The steps before it are reused, not run again; from this step on, anything changed outside the run is done again."),
    ).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Branch from here" }));

    await waitFor(() => expect(conn.query.branchRun).toHaveBeenCalled());
    expect(conn.query.branchRun.mock.calls[0]?.[0]).toEqual({ runId: RUN, stepKey: "draft" });
    // THE NEW RUN, and the source untouched.
    expect(await screen.findByText(/The steps before it were reused from that run/)).toBeTruthy();
    expect(conn.query.forkRun).not.toHaveBeenCalled();

    // BACK IS THE RUN IT CAME FROM -- open at the step the person branched from.
    fireEvent.click(screen.getByRole("button", { name: "Back to weeklyReport" }));
    await waitFor(() => expect(screen.queryByText(/The steps before it were reused from that run/)).toBeNull());
    expect(screen.getByRole("heading", { name: /^Version 3/ })).toBeTruthy();
  });
});

describe("Make current", () => {
  it("moves the head to the version picked", async () => {
    const conn = await openRun();
    await selectStep("draft");
    fireEvent.click(screen.getByRole("radio", { name: /^Version 1/ }));
    fireEvent.click(within(bar()).getByRole("button", { name: /^Make version 1 of draft current/ }));
    await waitFor(() => expect(conn.query.moveRunHead).toHaveBeenCalled());
    expect(conn.query.moveRunHead.mock.calls[0]?.[0]).toEqual({ runId: RUN, stepKey: "draft", version: 1 });
  });

  it("puts a refusal on the bar that offered it, verbatim", async () => {
    await openRun({ writeError: new Error("version_not_found: draft has no version 1") });
    await selectStep("draft");
    fireEvent.click(screen.getByRole("radio", { name: /^Version 1/ }));
    fireEvent.click(within(bar()).getByRole("button", { name: /^Make version 1/ }));
    expect(await within(bar()).findByText("version_not_found: draft has no version 1")).toBeTruthy();
  });
});

describe("feedback", () => {
  function stepVerdict(): HTMLElement {
    return screen.getByRole("group", { name: "Your verdict" });
  }

  it("saves a Like at once, and presses the pill only once the server said so", async () => {
    const conn = await openRun();
    await selectStep("draft");
    let resolve: (value: unknown) => void = () => {};
    conn.query.recordFeedback.mockImplementationOnce(
      () =>
        new Promise((done) => {
          resolve = done;
        }) as never,
    );
    const like = within(stepVerdict()).getByRole("button", { name: "Like" });
    fireEvent.click(like);
    await waitFor(() => expect(conn.query.recordFeedback).toHaveBeenCalled());
    expect(conn.query.recordFeedback.mock.calls[0]?.[0]).toEqual({ runId: RUN, stepKey: "draft", version: 3, verdict: "like" });
    // NOT YET: success appears only after the operation succeeds.
    expect(like.getAttribute("aria-pressed")).toBe("false");

    const { builtinReply } = await import("./harness");
    await act(async () => {
      resolve(builtinReply("recordFeedback", [{ id: "o9", observationId: "o9", verdict: "like", validatorDisagrees: false }]));
    });
    await waitFor(() =>
      expect(within(stepVerdict()).getByRole("button", { name: "Like" }).getAttribute("aria-pressed")).toBe("true"),
    );
  });

  it("asks what was wrong before a dislike, and offers Save only once an axis is chosen", async () => {
    const conn = await openRun();
    await selectStep("draft");
    fireEvent.click(within(stepVerdict()).getByRole("button", { name: "Dislike" }));
    const question = screen.getByRole("group", { name: "What was wrong?" });
    expect(within(question).queryByRole("button", { name: "Save" })).toBeNull();
    expect(within(question).getByText("Choose what was wrong")).toBeTruthy();
    // An open question is a PROPOSAL, not a saved dislike.
    expect(within(stepVerdict()).getByRole("button", { name: "Dislike" }).getAttribute("aria-pressed")).toBe("false");

    fireEvent.click(within(question).getByRole("button", { name: "The approach" }));
    fireEvent.change(within(question).getByLabelText("Why?"), { target: { value: "It never opened the ledger." } });
    fireEvent.click(within(question).getByRole("button", { name: "Save" }));

    await waitFor(() => expect(conn.query.recordFeedback).toHaveBeenCalled());
    expect(conn.query.recordFeedback.mock.calls[0]?.[0]).toEqual({
      runId: RUN,
      stepKey: "draft",
      version: 3,
      verdict: "dislike",
      process: true,
      reason: "It never opened the ledger.",
    });
    await waitFor(() => expect(screen.queryByRole("group", { name: "What was wrong?" })).toBeNull());
    expect(screen.getByText("You disliked the approach: It never opened the ledger.")).toBeTruthy();
    expect(within(stepVerdict()).getByRole("button", { name: "Dislike" }).getAttribute("aria-pressed")).toBe("true");
  });

  it("shows the server's refusal in place and keeps the question as it was", async () => {
    await openRun({ writeError: new Error("feedback_axis_required: a dislike names what was wrong") });
    await selectStep("draft");
    fireEvent.click(within(stepVerdict()).getByRole("button", { name: "Dislike" }));
    const question = screen.getByRole("group", { name: "What was wrong?" });
    fireEvent.click(within(question).getByRole("button", { name: "The result" }));
    fireEvent.click(within(question).getByRole("button", { name: "Save" }));
    expect(await screen.findByText("feedback_axis_required: a dislike names what was wrong")).toBeTruthy();
    expect(screen.getByRole("group", { name: "What was wrong?" })).toBeTruthy();
    expect(within(screen.getByRole("group", { name: "What was wrong?" })).getByRole("button", { name: "The result" }).getAttribute("aria-pressed")).toBe("true");
  });

  it("records the run's own verdict with no step", async () => {
    const conn = await openRun();
    fireEvent.click(within(screen.getByRole("group", { name: "Your verdict on this run" })).getByRole("button", { name: "Neutral" }));
    await waitFor(() => expect(conn.query.recordFeedback).toHaveBeenCalled());
    expect(conn.query.recordFeedback.mock.calls[0]?.[0]).toEqual({ runId: RUN, verdict: "neutral" });
  });

  it("shows the NEWEST verdict from the journal as the one pressed", async () => {
    await openRun({
      observations: [
        feedbackObservation({ id: "f1", runId: RUN, verdict: "like", stepKey: "draft", version: 3, createdAt: "2026-09-01T10:00:00Z" }),
        feedbackObservation({ id: "f2", runId: RUN, verdict: "dislike", stepKey: "draft", version: 3, axes: { performance: true }, reason: "Too slow.", createdAt: "2026-09-01T11:00:00Z" }),
      ],
    });
    await selectStep("draft");
    await waitFor(() =>
      expect(within(stepVerdict()).getByRole("button", { name: "Dislike" }).getAttribute("aria-pressed")).toBe("true"),
    );
    expect(within(stepVerdict()).getByRole("button", { name: "Like" }).getAttribute("aria-pressed")).toBe("false");
    expect(screen.getByText("You disliked the behaviour: Too slow.")).toBeTruthy();
  });

  it("offers no verdict on a step no model or app took part in", async () => {
    await openRun();
    await selectStep("fetch");
    expect(screen.queryByRole("group", { name: "Your verdict" })).toBeNull();
    // ...and no "Answered by" on it either: nothing was asked.
    expect(screen.queryByText("Answered by")).toBeNull();
    expect(screen.getByRole("group", { name: "Your verdict on this run" })).toBeTruthy();
  });
});

describe("the validator", () => {
  const FLAG = { verdict: "flag", stepKey: "draft", version: 3, observationId: "val-1", level: "strong", at: "t" };
  const REASON = "The summary gives returns as 2.1% but the ledger says 3.4%.";

  it("says what it flagged beside the verdict, and that it disagrees with a like", async () => {
    await openRun({
      runs: [reportRun({ validation: FLAG })],
      observations: [
        validatorObservation({ id: "val-1", runId: RUN, verdict: "flag", stepKey: "draft", version: 3, axes: { product: true }, reason: REASON }),
        feedbackObservation({ id: "f1", runId: RUN, verdict: "like" }),
      ],
    });
    const run = screen.getByRole("group", { name: "Your verdict on this run" }).parentElement as HTMLElement;
    expect(await within(run).findByText(`Checked before you saw it: flagged the result in draft -- ${REASON}`)).toBeTruthy();
    expect(within(run).getByText("It disagrees with you.")).toBeTruthy();

    // Beside the version it judged, too -- where a like of that version disagrees.
    await selectStep("draft");
    fireEvent.click(within(screen.getByRole("group", { name: "Your verdict" })).getByRole("button", { name: "Like" }));
    const panel = screen.getByRole("region", { name: "Version 3 of draft" });
    expect(await within(panel).findByText(`Checked before you saw it: flagged the result -- ${REASON}`)).toBeTruthy();
    await waitFor(() => expect(within(panel).getByText("It disagrees with you.")).toBeTruthy());
  });

  it("takes the server's own word for a disagreement it reported on the write", async () => {
    await openRun({
      runs: [reportRun({ validation: { ...FLAG, verdict: "pass" } })],
      observations: [validatorObservation({ id: "val-1", runId: RUN, verdict: "pass", stepKey: "draft", version: 3 })],
      feedbackReply: { id: "o1", observationId: "o1", verdict: "like", validatorDisagrees: true },
    });
    const run = screen.getByRole("group", { name: "Your verdict on this run" }).parentElement as HTMLElement;
    expect(within(run).getByText("Checked before you saw it: no problems found in draft.")).toBeTruthy();
    fireEvent.click(within(screen.getByRole("group", { name: "Your verdict on this run" })).getByRole("button", { name: "Like" }));
    expect(await within(run).findByText("It disagrees with you.")).toBeTruthy();
  });

  it("offers Run again with this, which starts the composer from its reason", async () => {
    await openRun({
      runs: [reportRun({ validation: FLAG })],
      observations: [validatorObservation({ id: "val-1", runId: RUN, verdict: "flag", stepKey: "draft", version: 3, axes: { process: true }, reason: REASON })],
    });
    fireEvent.click(await screen.findByRole("button", { name: "Run again with this" }));
    const dialog = await screen.findByRole("dialog", { name: /Run again/ });
    expect(within(dialog).getByText("draft, version 3")).toBeTruthy();
    await waitFor(() => expect((within(dialog).getByLabelText("Instructions") as HTMLTextAreaElement).value).toBe(REASON));
  });

  it("offers no Run again with this while a re-run is in flight", async () => {
    await openRun({
      runs: [reportRun({ validation: FLAG, status: "running", finishedAt: "", rerun: { requestId: "q", reason: "rerun", stepKey: "publish", requestedAt: "t" } })],
      observations: [validatorObservation({ id: "val-1", runId: RUN, verdict: "flag", stepKey: "draft", version: 3, reason: REASON })],
    });
    await screen.findByText(/Checked before you saw it: flagged/);
    expect(screen.queryByRole("button", { name: "Run again with this" })).toBeNull();
  });
});

describe("versions on the spine", () => {
  it("says a step's versions in its name, and which one is current", async () => {
    await openRun();
    expect(screen.getByLabelText(/^Step 2, draft, .*3 versions, version 3 is current$/)).toBeTruthy();
    // One version says nothing about versions.
    expect(screen.getByLabelText(/^Step 1, fetch,/).getAttribute("aria-label")).not.toContain("version");
  });

  it("says in words when the run went back to an earlier version", async () => {
    await openRun({
      runs: [reportRun({ head: { fetch: { version: 1 }, draft: { version: 1 }, publish: { version: 1 } } })],
    });
    expect(screen.getByLabelText(/version 1 is current/)).toBeTruthy();
    expect(screen.getByText("version 1 of 3")).toBeTruthy();
  });

  it("shows the picked version's own facts", async () => {
    await openRun();
    await selectStep("draft");
    // Version 3 was asked for more, and written by the person looking.
    expect(screen.getByText("Reasoning, app:claude-code:opus, high effort")).toBeTruthy();
    expect(screen.getByText("You")).toBeTruthy();
    fireEvent.click(screen.getByRole("radio", { name: /^Version 1/ }));
    expect(screen.getByRole("heading", { name: /^Version 1/ })).toBeTruthy();
    expect(screen.getByText("First draft.")).toBeTruthy();
    expect(screen.queryByText("Asked for")).toBeNull();
  });

  it("marks the steps that will run again", async () => {
    await openRun({ runs: [reportRun({ staleSteps: ["publish"] })] });
    expect(screen.getByLabelText(/^Step 3, publish, .*runs again$/)).toBeTruthy();
    expect(screen.getByText("runs again")).toBeTruthy();
  });

  it("still draws the timeline, and says so once, when the versions cannot be read", async () => {
    await openRun({ versions: new Error("work: stepVersions is not wired yet") });
    expect(await screen.findByText("Earlier versions of these steps could not be read.")).toBeTruthy();
    expect(screen.getByText("work: stepVersions is not wired yet")).toBeTruthy();
    await selectStep("draft");
    // The acts need only the step, so they stay.
    expect(acts()).toEqual(["Branch from here", "Run again"]);
  });
});

describe("every act survives", () => {
  it("keeps every act the run page offered before, renamed where the verb changed", async () => {
    // Replay on a finished run; Answer it on a parked one -- both from before
    // this epic -- and Fork, which is now Branch from here.
    await openRun();
    expect(acts()).toContain("Replay");
    await selectStep("draft");
    expect(acts()).toContain("Branch from here");
    expect(acts()).not.toContain("Fork from draft");
  });

  it("calls forkRun from nowhere in the app -- a person branches instead", () => {
    const here = dirname(fileURLToPath(import.meta.url));
    const dir = join(here, "../../src/apps/nexus");
    const offenders = readdirSync(dir)
      .filter((name) => /\.(ts|tsx)$/.test(name))
      .filter((name) => /\bforkRun\s*\(/.test(readFileSync(join(dir, name), "utf8")));
    expect(offenders).toEqual([]);
    // The reachable positive: the scan saw the file that calls its successor.
    expect(readFileSync(join(dir, "actions.ts"), "utf8")).toMatch(/\bbranchRun\s*\(/);
  });
});
