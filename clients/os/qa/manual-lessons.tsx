import { createRoot } from "react-dom/client";
import { Result } from "@znasllc-io/memql-sdk-core/client";
import { LessonsReview } from "../src/apps/training/LessonsReview";
import { ProposeLesson } from "../src/apps/training/ProposeLesson";
import { emptyLesson, type Lesson } from "../src/apps/training/lessons";
import { installQaConnection } from "./connectionShim";
import "../src/styles/index.css";
const params = new URLSearchParams(location.search),
  state = params.get("state") ?? "candidate";
document.documentElement.dataset.theme =
  params.get("mode") === "dark" ? "dark" : "light";
document.documentElement.dataset.osTheme = "graphite";
let event: Lesson = {
  schemaVersion: 1,
  id: "lesson-preview",
  receipt: "event-preview",
  sequence: 1,
  version: 1,
  digest: "a".repeat(64),
  draft: {
    ...emptyLesson(
      "After a refusal, distinguish task permission from worker policy before requesting broader access.",
    ),
    title: "Identify the refused gate",
    applies: "A tool returned a policy or permission refusal.",
    exceptions:
      "Skip an optional diagnostic when a permitted path is sufficient.",
    observed:
      "Synthetic example: permission was granted, but a separate worker policy refused the operation.",
    alternative: "Read the refusal and choose a permitted alternative.",
    principle: "Permission and capability are separate gates.",
    factConfidence: "Synthetic fixture",
    lessonConfidence: "Candidate; benefit not measured",
    scenario: "tool-unavailable",
    counterexample: "implement-and-check",
  },
  source: {
    conversationId: "preview-conversation",
    turnId: "feedback",
    runId: "preview-work",
    quote: "Identify which gate refused the operation.",
    status: "applied",
    at: "2026-10-15T00:00:00Z",
  },
  review: state === "reviewed" ? "reviewed" : "candidate",
  enabled: false,
  evaluationIds: [],
  action: "propose",
  reason: "",
  at: "2026-10-15T00:00:00Z",
};
installQaConnection({
  query: {
    workTrainingLessonsForOwner: async () =>
      state === "loading"
        ? new Promise<Result>(() => {})
        : new Result({
            data:
              state === "empty" ? [] : [{ data: { trainingLesson: event } }],
          }),
    workTrainingUsageForOwner: async () => new Result({ data: [] }),
    librarySaveTrainingLesson: async (args: Record<string, unknown>) => {
      if (state === "error")
        throw new Error("Synthetic refusal: this revision changed.");
      event = {
        ...event,
        sequence: event.sequence + 1,
        receipt: `event-${event.sequence + 1}`,
        action: String(args.action),
        review:
          args.action === "review"
            ? "reviewed"
            : args.action === "reject"
              ? "rejected"
              : "candidate",
      };
      return new Result({ data: [event] });
    },
  },
});
createRoot(document.getElementById("root")!).render(
  <main
    className="os-app-body"
    style={{ padding: 24, maxWidth: 1000, margin: "0 auto" }}
  >
    <p>Preview · Synthetic records</p>
    {params.get("view") === "propose" ? (
      <ProposeLesson
        conversationId="preview"
        turnId="feedback"
        text="Identify which gate refused the operation."
      />
    ) : (
      <LessonsReview />
    )}
  </main>,
);
