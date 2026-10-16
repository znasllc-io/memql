import type { Row } from "@znasllc-io/memql-sdk-core/client";
export type LessonDraft = {
  title: string;
  text: string;
  scope: "owner";
  applies: string;
  exceptions: string;
  observed: string;
  alternative: string;
  principle: string;
  factConfidence: string;
  lessonConfidence: string;
  scenario: string;
  counterexample: string;
};
export type Lesson = {
  schemaVersion: 1;
  id: string;
  receipt: string;
  sequence: number;
  version: number;
  digest: string;
  draft: LessonDraft;
  source: {
    conversationId: string;
    turnId: string;
    runId: string;
    quote: string;
    status: string;
    at: string;
  };
  review: "candidate" | "reviewed" | "rejected" | "retired";
  enabled: boolean;
  evaluationIds: string[];
  action: string;
  reason: string;
  at: string;
};
export const emptyLesson = (text = ""): LessonDraft => ({
  title: "",
  text,
  scope: "owner",
  applies: "",
  exceptions: "",
  observed: "",
  alternative: "",
  principle: "",
  factConfidence: "Not assessed",
  lessonConfidence: "Not assessed",
  scenario: "",
  counterexample: "",
});
export function lesson(value: unknown): Lesson {
  const v = value as Lesson;
  if (
    !v ||
    v.schemaVersion !== 1 ||
    !v.id ||
    !v.receipt ||
    !Number.isSafeInteger(v.sequence) ||
    !v.digest ||
    !v.draft?.text ||
    v.draft.scope !== "owner" ||
    !v.source?.runId ||
    !["candidate", "reviewed", "rejected", "retired"].includes(v.review) ||
    typeof v.enabled !== "boolean"
  )
    throw new Error(
      "The lesson receipt is incomplete. Reopen Training to check it.",
    );
  return v;
}
export function lessonEvents(rows: Row[]): Lesson[] {
  return rows.map((row) =>
    lesson((row.data as Record<string, unknown>)?.trainingLesson),
  );
}
export function lessonHeads(events: Lesson[]): Lesson[] {
  const heads = new Map<string, Lesson>();
  for (const l of events)
    if (!heads.has(l.id) || heads.get(l.id)!.sequence < l.sequence)
      heads.set(l.id, l);
  return [...heads.values()];
}
export const guidanceReceiptLabel = (status?: string) =>
  status === "applied"
    ? "Received by this work."
    : status === "not-received"
      ? "Not received. This work ended or changed."
      : "Queued for the next safe boundary.";
