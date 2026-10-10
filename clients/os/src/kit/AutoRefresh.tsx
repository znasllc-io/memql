import { useEffect, useRef } from "react";

export const AUTO_REFRESH_MS = 15_000;

/** Quiet recovery for reads that cannot subscribe. The owning data hook still
 * performs the initial read, reports failures, and cancels requests on teardown.
 * Mount beside the reading so parked panes pause along with hidden browser tabs.
 * Never use this for mutations, probes, or other actions with side effects. */
export function AutoRefresh({ onRefresh, busy = false, enabled = true, intervalMs = AUTO_REFRESH_MS }: {
  onRefresh: () => void | Promise<unknown>;
  busy?: boolean;
  enabled?: boolean;
  intervalMs?: number;
}) {
  const marker = useRef<HTMLSpanElement>(null);
  const latest = useRef({ onRefresh, busy, enabled });
  useEffect(() => { latest.current = { onRefresh, busy, enabled }; });
  useEffect(() => {
    let disposed = false;
    let running = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let lastStart = 0;
    const visible = () => {
      if (document.visibilityState === "hidden" || navigator.onLine === false) return false;
      for (let parent = marker.current?.parentElement; parent; parent = parent.parentElement) {
        if (parent.hidden || parent.inert || getComputedStyle(parent).display === "none") return false;
      }
      return true;
    };
    const schedule = () => {
      clearTimeout(timer);
      if (!disposed) timer = setTimeout(() => void run(), intervalMs);
    };
    const run = async () => {
      if (disposed || running) return;
      const current = latest.current;
      if (!current.enabled || current.busy || !visible() || Date.now() - lastStart < 1_000) { schedule(); return; }
      clearTimeout(timer);
      running = true;
      lastStart = Date.now();
      try { await current.onRefresh(); }
      catch { /* The read's own error state reports the failure. Retry next interval. */ }
      finally { running = false; schedule(); }
    };
    const wake = () => { void run(); };
    schedule();
    document.addEventListener("visibilitychange", wake);
    window.addEventListener("focus", wake);
    window.addEventListener("online", wake);
    return () => {
      disposed = true;
      clearTimeout(timer);
      document.removeEventListener("visibilitychange", wake);
      window.removeEventListener("focus", wake);
      window.removeEventListener("online", wake);
    };
  }, [intervalMs]);
  return <span ref={marker} hidden aria-hidden="true" />;
}
