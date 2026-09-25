import { useEffect, useState, type CSSProperties } from "react";
import type { AskActivity } from "./conversationSession";

/** An estimate is neither a deadline nor a percentage of completed work. */
export function expectedWait(now: number, start: number, expectedMs: number) {
  const elapsed = Math.max(0, now - start);
  return { remaining: Math.max(0, Math.ceil((expectedMs - elapsed) / 1000)), fraction: Math.max(0.06, 1 - elapsed / expectedMs), overdue: elapsed >= expectedMs };
}
export function AskWait({ activity, startedAt, hasText, label: activeLabel }: { label?: string; activity: AskActivity[]; startedAt: string; hasText: boolean }) {
  const [now, setNow] = useState(Date.now);
  useEffect(() => { const timer = setInterval(() => setNow(Date.now()), 250); return () => clearInterval(timer); }, []);
  const last = activity.at(-1);
  const latest = new Map(activity.filter(event => event.kind === "model").map(event => [event.id, event]));
  const call = [...latest.values()].reverse().find(event => event.phase === "running");
  const wait = expectedWait(now, new Date(call?.at ?? startedAt).valueOf(), Math.max(1000, call?.expectedMs ?? 60000));
  const label = last?.kind === "action" && last.phase === "running" ? "Working in your workspace" : hasText ? "Replying" : wait.overdue ? "Still working" : activeLabel ?? "Thinking";
  return <div className="os-ask-wait" aria-label={label} title={call ? `${call.estimateSource ?? "Estimate"} · ${call.model || call.provider || "Selected model"}` : "Waiting for the selected inference route"}>
    <span className="os-ask-countdown" style={{ "--ask-remaining": wait.fraction } as CSSProperties} aria-hidden><span /></span>
    <span>{label}{!hasText && !wait.overdue ? ` · about ${wait.remaining}s` : "…"}</span>
  </div>;
}
