import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AskSurface } from "../../src/ask/AskSurface";
import type { AskCallbacks } from "../../src/ask/askController";
import { conversationFeed } from "../../src/ask/conversationFeed";
import { ConversationSession, type AskTurn } from "../../src/ask/conversationSession";
import { READY_ASK } from "../../src/ask/useAskReadiness";

afterEach(() => { cleanup(); vi.useRealTimers(); });
const startedAt = "2026-10-05T10:00:00.000Z";
const acknowledgedAt = "2026-10-05T10:00:12.000Z";
const endedAt = "2026-10-05T10:03:00.000Z";
const receipt = { id: "work:run", kind: "run" as const, phase: "queued" as const, at: acknowledgedAt, arguments: { runId: "run" } };
const task: AskTurn = { id: "task", runId: "run", prompt: "Find my preference", answer: "Found it.", acknowledgement: "I'll check your previous conversations.", background: true, state: "done", startedAt, endedAt, activity: [receipt] };
const timestamps = () => [...screen.getByRole("log").querySelectorAll("time")].map(time => time.dateTime);

it("stamps the delivered acknowledgment independently of the later result, including after reopening", async () => {
  vi.useFakeTimers(); vi.setSystemTime(new Date(startedAt));
  let callbacks!: AskCallbacks;
  let stored: AskTurn[] = [];
  const transport = {
    ask: (_prompt: string, _context: string | null, on: AskCallbacks) => { callbacks = on; return { cancel() {} }; },
    conversations: { create: async () => ({ id: "chat", title: "Chat" }), list: async () => [], read: async () => stored },
  };
  const session = new ConversationSession(transport);
  const view = render(<AskSurface transport={transport} conversation={session} availability={READY_ASK} variant="sheet" />);
  fireEvent.change(screen.getByRole("textbox", { name: "Ask" }), { target: { value: task.prompt } });
  await act(async () => fireEvent.click(screen.getByRole("button", { name: "Send" })));
  act(() => { callbacks.activity?.(receipt); callbacks.delta(task.acknowledgement!); });
  expect(timestamps()).toEqual([startedAt]);
  act(() => callbacks.done());
  expect(timestamps()).toEqual([startedAt, acknowledgedAt]);
  const id = session.getSnapshot().turns[0]!.id;
  stored = [{ ...task, id }];
  await act(async () => session.reload(false));
  expect(timestamps()).toEqual([startedAt, acknowledgedAt, endedAt]);
  expect(screen.getByText(task.acknowledgement!)).toBeTruthy();
  expect(screen.getByText(task.answer)).toBeTruthy();
  view.unmount(); session.dispose();

  // A new viewer has no local stream state; timing comes from durable receipts.
  const reopened = new ConversationSession(transport);
  await reopened.select("chat");
  const restored = render(<AskSurface transport={transport} conversation={reopened} availability={READY_ASK} variant="sheet" />);
  expect(timestamps()).toEqual([startedAt, acknowledgedAt, endedAt]);
  expect([...screen.getByRole("log").querySelectorAll("time")].at(-2)?.title).toContain("2026");
  restored.unmount(); reopened.dispose();
});

it("retains the acknowledgment time through waiting and failure without stamping a failed result", () => {
  const activity = [...task.activity, { ...receipt, phase: "waiting" as const, at: "2026-10-05T10:02:00Z" }];
  expect(conversationFeed([{ ...task, state: "waiting", endedAt: undefined, activity }])[0]?.responseAt).toBe(acknowledgedAt);
  const failed = conversationFeed([{ ...task, state: "error", error: "Failed", activity }]);
  expect(failed.map(item => item.responseAt)).toEqual([acknowledgedAt, undefined]);
});

it("does not invent a delivery time for absent prose or missing and invalid receipts", () => {
  for (const activity of [[], [{ ...receipt, at: "invalid" }], [{ ...receipt, phase: "running" as const }]]) {
    expect(conversationFeed([{ ...task, activity }]).map(item => item.responseAt)).toEqual([undefined, endedAt]);
  }
  expect(conversationFeed([{ ...task, acknowledgement: "" }]).map(item => item.responseAt)).toEqual([undefined, endedAt]);
});
