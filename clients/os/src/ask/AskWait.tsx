import { useEffect, useState, type CSSProperties } from "react";
import type { AskActivity } from "./conversationSession";

/** An estimate is neither a deadline nor a percentage of completed work. */
export function expectedWait(now: number, start: number, expectedMs: number) {
  const elapsed = Math.max(0, now - start);
  return { remaining: Math.max(0, Math.ceil((expectedMs - elapsed) / 1000)), overrun: Math.floor(Math.max(0, elapsed - expectedMs) / 1000), fraction: Math.max(0.06, 1 - elapsed / expectedMs), overdue: elapsed >= expectedMs };
}
export function AskWait({ activity, startedAt, hasText, label: activeLabel }: { label?: string; activity: AskActivity[]; startedAt: string; hasText: boolean }) {
  const [now, setNow] = useState(Date.now);
  useEffect(() => { const timer = setInterval(() => setNow(Date.now()), 250); return () => clearInterval(timer); }, []);
  // A running/completed pair is one step; receipts and artifacts are not work.
  // Keep the original start even when the same step publishes another update.
  const latest = new Map<string, { event: AskActivity; startedAt: string }>();
  for (const event of activity) {
    if (event.kind !== "model" && event.kind !== "action") continue;
    const key = `${event.kind}:${event.id}`;
    const previous = latest.get(key);
    latest.set(key, { event: { ...previous?.event, ...event }, startedAt: previous?.startedAt ?? event.at });
  }
  const steps = [...latest.values()];
  let current = -1;
  steps.forEach((step, index) => { if (step.event.phase === "running") current = index; });
  const index = current >= 0 ? current : Math.max(0, steps.length - 1);
  const step = steps[index];
  const call = step?.event;
  const wait = expectedWait(now, new Date(step?.startedAt ?? startedAt).valueOf(), Math.max(1000, call?.expectedMs ?? 60000));
  const label = activeLabel ?? (call?.kind === "action" && call.phase === "running" ? "Working in your workspace" : hasText ? "Replying" : wait.overdue ? "Still working" : "Thinking");
  const timer = wait.overdue ? `+${wait.overrun}s` : `${wait.remaining}s`;
  return <div className="os-ask-wait" aria-label={label} title={call ? `${call.estimateSource ?? "Estimate"} · ${call.model || call.provider || "Selected model"}` : "Waiting for the selected inference route"}>
    <span className="os-ask-countdown" style={{ "--ask-remaining": wait.fraction } as CSSProperties} aria-hidden><span /></span>
    <span>{label} · Step {index + 1} · {timer}</span>
  </div>;
}
