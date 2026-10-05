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
