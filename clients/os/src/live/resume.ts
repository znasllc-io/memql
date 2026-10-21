/** One browser lifecycle signal for read recovery. Never replay a write here.
 * Safari page restoration, tab return, window focus and network return often
 * arrive together. Coalesce them, and do no work in hidden/offline documents. */
const listeners = new Set<() => void>();
let lastWake = -Infinity;
const visible = () => document.visibilityState !== "hidden" && navigator.onLine !== false;
const wake = () => {
  const now = Date.now(), elapsed = now - lastWake;
  if (!visible() || (elapsed >= 0 && elapsed < 250)) return;
  // Recover promptly on the first event, then absorb its companion events.
  lastWake = now;
  for (const listener of [...listeners]) listener();
};
const events = ["focus", "online", "pageshow"] as const;
export function onAppResume(listener: () => void): () => void {
  if (listeners.size === 0) {
    document.addEventListener("visibilitychange", wake);
    for (const event of events) window.addEventListener(event, wake);
  }
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
    if (listeners.size !== 0) return;
    lastWake = -Infinity;
    document.removeEventListener("visibilitychange", wake);
    for (const event of events) window.removeEventListener(event, wake);
  };
}
