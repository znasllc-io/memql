import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Result } from "@znasllc-io/memql-sdk-core/client";
const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
}));
import { LessonsReview } from "../../src/apps/training/LessonsReview";
import { ProposeLesson } from "../../src/apps/training/ProposeLesson";
import { lessonUsage } from "../../src/apps/training/LessonUsage";
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
    status: "applied",
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
    expect(guidanceReceiptLabel("applied")).toBe("Received by this work.");
    expect(guidanceReceiptLabel("not-received")).toContain("Not received");
  });
  it("preserves an edited draft when a decision fails, without claiming success", async () => {
    const save = vi.fn().mockRejectedValue(new Error("lesson changed"));
    h.connection = {
      query: {
        workTrainingUsageForOwner: async () => new Result({ data: [] }),
        workTrainingLessonsForOwner: async () =>
          new Result({ data: [{ data: { trainingLesson: candidate } }] }),
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
