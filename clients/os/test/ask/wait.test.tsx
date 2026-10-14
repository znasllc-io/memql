import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { AskWait } from "../../src/ask/AskWait";
import type { AskActivity } from "../../src/ask/conversationSession";

const startedAt = "2026-10-05T14:00:00.000Z";
const call: AskActivity = { id: "classify", kind: "model", phase: "running", at: startedAt, expectedMs: 60_000 };
beforeEach(() => { vi.useFakeTimers(); vi.setSystemTime(new Date(startedAt)); });
afterEach(() => { cleanup(); vi.useRealTimers(); });

it("counts down without about, preserving the start across updates to the same step", () => {
 const view = render(<AskWait activity={[call]} startedAt={startedAt} hasText={false} />);
 act(() => vi.advanceTimersByTime(4000));
 expect(screen.getByText("Thinking · Step 1 · 56s")).toBeTruthy();
 view.rerender(<AskWait activity={[call, { ...call, at: new Date().toISOString() }]} startedAt={startedAt} hasText={false} />);
 expect(screen.getByText("Thinking · Step 1 · 56s")).toBeTruthy();
});

it("resets for a new model or action, ignoring completion, run and artifact receipts", () => {
 const activity: AskActivity[] = [call];
 const view = render(<AskWait activity={activity} startedAt={startedAt} hasText={false} />);
 act(() => vi.advanceTimersByTime(5000));
 activity.push({ ...call, phase: "completed" }, { id: "run", kind: "run", phase: "running", at: startedAt });
 activity.push({ id: "navigate", kind: "action", phase: "running", at: new Date().toISOString(), expectedMs: 2000 });
 view.rerender(<AskWait activity={activity} startedAt={startedAt} hasText={false} />);
 expect(screen.getByText("Working in your workspace · Step 2 · 2s")).toBeTruthy();
 act(() => vi.advanceTimersByTime(1000));
 activity.push({ ...activity[3]!, phase: "completed" }, { id: "file", kind: "artifact", phase: "completed", at: startedAt });
 activity.push({ ...call, id: "reply", at: new Date().toISOString() });
 view.rerender(<AskWait activity={activity} startedAt={startedAt} hasText />);
 expect(screen.getByText("Replying · Step 3 · 60s")).toBeTruthy();
});

it("shows time beyond the estimate without counting below zero or inventing another step", () => {
 render(<AskWait activity={[call]} startedAt={startedAt} hasText={false} />);
 act(() => vi.advanceTimersByTime(65_000));
 expect(screen.getByText("Still working · Step 1 · +5s")).toBeTruthy();
});

it("keeps a timer while waiting for the first activity update", () => {
 render(<AskWait activity={[]} startedAt={startedAt} hasText={false} />);
 act(() => vi.advanceTimersByTime(4000));
 expect(screen.getByText("Thinking · Step 1 · 56s")).toBeTruthy();
});

it("keeps the latest step after it finishes instead of reviving an orphan model timer", () => {
 const activity: AskActivity[] = [{ ...call, model: "Earlier model" }];
 const view = render(<AskWait activity={activity} startedAt={startedAt} hasText={false} />);
 act(() => vi.advanceTimersByTime(900_000));
 const newest: AskActivity = { ...call, id: "next-model", model: "Current model", at: new Date().toISOString() };
 activity.push(newest);
 view.rerender(<AskWait activity={activity} startedAt={startedAt} hasText={false} />);
 expect(screen.getByText("Thinking · Step 2 · 60s")).toBeTruthy();
 act(() => vi.advanceTimersByTime(5000));
 activity.push({ ...newest, phase: "completed", at: new Date().toISOString() });
 view.rerender(<AskWait activity={activity} startedAt={startedAt} hasText={false} />);
 expect(screen.getByText("Working · Step 2")).toBeTruthy();
 expect(screen.queryByTitle(/Earlier model|Current model/)).toBeNull();
 act(() => vi.advanceTimersByTime(20_000));
 expect(screen.getByText("Working · Step 2")).toBeTruthy();
 expect(activity[0]!.phase).toBe("running");
 const action: AskActivity = { id: "inspect", kind: "action", phase: "running", at: new Date().toISOString(), expectedMs: 3000 };
 activity.push(action);
 view.rerender(<AskWait activity={activity} startedAt={startedAt} hasText={false} />);
 expect(screen.getByText("Working in your workspace · Step 3 · 3s")).toBeTruthy();
 act(() => vi.advanceTimersByTime(1000));
 activity.push({ ...call, phase: "completed", at: new Date().toISOString() });
 view.rerender(<AskWait activity={activity} startedAt={startedAt} hasText={false} />);
 expect(screen.getByText("Working in your workspace · Step 3 · 2s")).toBeTruthy();
});

it("does not assign an earlier parallel action's elapsed time to the newest completed step", () => {
 const action: AskActivity = { id: "parallel-action", kind: "action", phase: "running", at: startedAt, expectedMs: 120_000 };
 const activity: AskActivity[] = [action];
 const view = render(<AskWait activity={activity} startedAt={startedAt} hasText={false} />);
 act(() => vi.advanceTimersByTime(10_000));
 expect(screen.getByText("Working in your workspace · Step 1 · 110s")).toBeTruthy();
 const newest = { ...call, at: new Date().toISOString() };
 activity.push(newest);
 view.rerender(<AskWait activity={activity} startedAt={startedAt} hasText={false} />);
 expect(screen.getByText("Thinking · Step 2 · 60s")).toBeTruthy();
 activity.push({ ...newest, phase: "completed" });
 view.rerender(<AskWait activity={activity} startedAt={startedAt} hasText={false} />);
 expect(screen.getByText("Working · Step 2")).toBeTruthy();
 act(() => vi.advanceTimersByTime(5000));
 activity.push({ ...action, at: new Date().toISOString() });
 view.rerender(<AskWait activity={activity} startedAt={startedAt} hasText={false} />);
 expect(screen.getByText("Working · Step 2")).toBeTruthy();
 expect(activity[0]).toEqual(action);
 expect(activity.at(-1)?.phase).toBe("running");
});

it.each(["completed", "failed", "cancelled", "waiting", "queued", "fallback"] as const)("does not run a countdown for a latest %s receipt", phase => {
 const view = render(<AskWait activity={[call]} startedAt={startedAt} hasText={false} />);
 act(() => vi.advanceTimersByTime(1000));
 view.rerender(<AskWait activity={[call, { ...call, phase, at: new Date().toISOString() }]} startedAt={startedAt} hasText={false} />);
 expect(screen.getByText("Working · Step 1")).toBeTruthy();
 act(() => vi.advanceTimersByTime(65_000));
 expect(screen.getByText("Working · Step 1")).toBeTruthy();
 expect(screen.queryByTitle(/Selected model/)).toBeNull();
});

it("preserves an explicit activity label across running and untimed states", () => {
 const view = render(<AskWait activity={[call]} startedAt={startedAt} hasText label="Transcribing" />);
 act(() => vi.advanceTimersByTime(4000));
 expect(screen.getByText("Transcribing · Step 1 · 56s")).toBeTruthy();
 view.rerender(<AskWait activity={[call, { ...call, phase: "completed" }]} startedAt={startedAt} hasText label="Transcribing" />);
 expect(screen.getByText("Transcribing · Step 1")).toBeTruthy();
 expect(screen.getByLabelText("Transcribing")).toBeTruthy();
});
