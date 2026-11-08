import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Result } from "@znasllc-io/memql-sdk-core/client";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));
import { LessonsReview } from "../../src/apps/training/LessonsReview";
import { ProposeLesson } from "../../src/apps/training/ProposeLesson";
import { LessonUsage, lessonUsage } from "../../src/apps/training/LessonUsage";
import {
  emptyLesson,
  guidanceReceiptLabel,
  lessonHeads,
  type Lesson,
} from "../../src/apps/training/lessons";
const candidate: Lesson = {
  schemaVersion: 1,
  id: "lesson-one",
  receipt: "event-one",
  sequence: 1,
  version: 1,
  digest: "a".repeat(64),
  draft: {
    ...emptyLesson("Name the gate that refused the operation."),
    title: "Read the refusal",
    applies: "A policy refusal",
    exceptions: "Skip unnecessary diagnostics",
    observed: "A synthetic refusal",
    alternative: "A permitted path",
    principle: "Separate gates",
    scenario: "tool-unavailable",
    counterexample: "implement-and-check",
  },
  source: {
    conversationId: "conversation",
    turnId: "guidance",
    runId: "run",
    quote: "Check which gate refused the action.",
    status: "received",
    at: "2026-10-15T00:00:00Z",
  },
  review: "candidate",
  enabled: false,
  evaluationIds: [],
  action: "propose",
  reason: "",
  at: "2026-10-15T00:00:00Z",
};
describe("manual lesson review", () => {
  it("folds by sequence and keeps rejection over older enabled evidence", () => {
    const rejected = { ...candidate, sequence: 3, review: "rejected" as const };
    expect(
      lessonHeads([
        rejected,
        { ...candidate, sequence: 2, enabled: true },
        candidate,
      ]),
    ).toEqual([rejected]);
  });
  it("says what the work did with the guidance a lesson came from", () => {
    expect(
      ["queued", "received", "answered", "notApproved", "saved", "notDelivered", "notReceived", "applied"].map(guidanceReceiptLabel),
    ).toEqual([
      "Queued for the next safe boundary.",
      "Received by this work.",
      "Answered.",
      "Sent as a correction; nothing was approved.",
      "Saved until the limit is raised.",
      "Not delivered.",
      "Not received. This work ended or changed.",
      "Queued for the next safe boundary.",
    ]);
  });
  it("preserves an edited draft when a decision fails, without claiming success", async () => {
    const save = vi.fn().mockRejectedValue(new Error("lesson changed"));
    h.connection = {
      query: {
        workTrainingSelectionsForLesson: async () => new Result({ data: [] }),
        workTrainingLessonHistory: async () => new Result({ data: [{ data: { trainingLesson: candidate } }] }),
        libraryTrainingLessonsPage: async () => new Result({ data: [{ lessons: [candidate], nextCursor: "" }] }),
        librarySaveTrainingLesson: save,
      },
    };
    render(<LessonsReview />);
    fireEvent.click(await screen.findByText("Read the refusal"));
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("Proposed guidance"), {
      target: { value: "Keep my edited guidance" },
    });
    await new Promise((r) => setTimeout(r, 500));
    fireEvent.click(screen.getByRole("button", { name: "Save revision" }));
    await screen.findByText("lesson changed");
    expect(
      (screen.getByLabelText("Proposed guidance") as HTMLTextAreaElement).value,
    ).toBe("Keep my edited guidance");
    expect(save.mock.calls[0]![0].expectedReceipt).toBe("event-one");
  });
  it("saving a proposal does not review or enable it", async () => {
    const save = vi.fn().mockResolvedValue(new Result({ data: [candidate] }));
    h.connection = { query: { librarySaveTrainingLesson: save } };
    render(
      <ProposeLesson
        conversationId="conversation"
        turnId="guidance"
        text="Source feedback"
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Propose lesson" }));
    fireEvent.click(screen.getByRole("button", { name: "Save candidate" }));
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    expect(save.mock.calls[0]![0].action).toBe("propose");
    await screen.findByText("Candidate saved. Review it in Training.");
  });
});

it("reports failed attempts separately from successful fallback delivery", () => {
  const selection = {
    id: "selection",
    runId: "run",
    data: {
      trainingSelection: {
        selected: [{ lessonId: "lesson-one", lessonVersion: 1 }],
      },
    },
  };
  const attempt = (phase: string, model: string) => ({
    data: {
      execution: {
        call: {
          id: phase,
          phase,
          provider: "fleet:test",
          model,
          contextReferences: ["selection"],
        },
      },
    },
  });
  expect(lessonUsage([selection], "lesson-one")[0]?.attempts).toHaveLength(0);
  const usage = lessonUsage(
    [
      selection,
      attempt("failed", "requested"),
      attempt("completed", "fallback"),
    ],
    "lesson-one",
  );
  expect(usage[0]?.attempts.map((c) => c.model)).toEqual([
    "requested",
    "fallback",
  ]);
});

it("loads ten lesson heads at a time, preserves pages on reopen and searches on the server", async () => {
  const lessons = Array.from({ length: 23 }, (_, n) => ({ ...candidate, id: `lesson-${n}`, receipt: `event-${n}`, draft: { ...candidate.draft, title: `Lesson ${n}` } }));
  const read = vi.fn(async ({ cursor, search }: { cursor?: string; search?: string }) => {
    const matched = search ? lessons.filter(l => l.draft.title.toLowerCase().includes(search.toLowerCase())) : lessons;
    const offset = Number(cursor || 0);
    return new Result({ data: [{ lessons: matched.slice(offset, offset + 10), nextCursor: offset + 10 < matched.length ? String(offset + 10) : "" }] });
  });
  h.connection = { query: { libraryTrainingLessonsPage: read } };
  const view = render(<LessonsReview />);
  await screen.findByText("Lesson 9");
  expect(screen.queryByText("Lesson 10")).toBeNull();
  expect(read).toHaveBeenCalledTimes(1);
  fireEvent.click(screen.getByRole("button", { name: "Load more" }));
  await screen.findByText("Lesson 19");
  view.unmount();
  const reopened = render(<LessonsReview />);
  await screen.findByText("Lesson 19");
  expect(read).toHaveBeenCalledTimes(2);
  fireEvent.click(screen.getByRole("button", { name: "Search lessons" }));
  fireEvent.change(screen.getByPlaceholderText("Search lessons"), { target: { value: "Lesson 22" } });
  await waitFor(() => expect(read).toHaveBeenCalledTimes(3));
  expect(await screen.findByRole("button", { name: /Lesson 22 Version/ })).toBeTruthy();
  expect(screen.queryByText("Lesson 0")).toBeNull();
  reopened.unmount();
});


it("reads delivery evidence only for the visible lesson selections, with cached continuation", async () => {
  const selections = Array.from({ length: 13 }, (_, n) => ({ id: `selection-${n}`, runId: `run-${n}`, data: { trainingSelection: { selected: [{ lessonId: candidate.id, lessonVersion: n + 1 }] } } }));
  const read = vi.fn(async (_args, { cursor }: { cursor?: string } = {}) => {
    const offset = Number(cursor || 0);
    return new Result({ data: selections.slice(offset, offset + 10), meta: { cursor: offset === 0 ? "10" : "", hasMore: offset === 0 } });
  });
  const deliveries = vi.fn(async ({ observationIds }: { observationIds: string[] }) => new Result({ data: observationIds.map(id => ({ data: { execution: { call: { id: `call-${id}`, phase: "completed", provider: "fleet", model: "local", contextReferences: [id] } } } })) }));
  h.connection = { query: { workTrainingSelectionsForLesson: read, workTrainingDeliveryForSelections: deliveries } };
  const view = render(<LessonUsage lessonId={candidate.id} />);
  await screen.findByText("run-9");
  expect(screen.queryByText("run-10")).toBeNull();
  expect(read).toHaveBeenCalledTimes(1);
  expect(deliveries).toHaveBeenCalledTimes(1);
  expect(deliveries.mock.calls[0]![0].observationIds).toEqual(selections.slice(0, 10).map(row => row.id));
  fireEvent.click(screen.getByRole("button", { name: "Load more" }));
  await screen.findByText("run-12");
  expect(deliveries.mock.calls[1]![0].observationIds).toEqual(selections.slice(10).map(row => row.id));
  view.unmount();
  const reopened = render(<LessonUsage lessonId={candidate.id} />);
  await screen.findByText("run-12");
  expect(read).toHaveBeenCalledTimes(2);
  expect(deliveries).toHaveBeenCalledTimes(2);
  reopened.unmount();
});
