import { useCallback, useEffect, useRef, useState } from "react";

const documentVisible = () => document.visibilityState !== "hidden";

// The SQL publication journal has no graph subscription. Read it only while
// visible, serially, every 20 seconds and on reconnect. A zero interval opts
// remote publisher discovery into initial/manual/reconnect reads only. Never
// repeat a write.
export function useReleaseRead<T>(key: string, read: ((signal: AbortSignal) => Promise<T>) | null, visible: boolean, source: object | null, pollInterval = 20_000) {
  const reader = useRef(read);
  reader.current = read;
  const [revision, setRevision] = useState(0);
  const retry = useCallback(() => setRevision((n) => n + 1), []);
  const [snapshot, setSnapshot] = useState<{ key: string; source: object | null; revision: number; value: T | null; error: string; fresh: boolean }>({ key, source, revision, value: null, error: "", fresh: false });
  const available = read !== null;
  useEffect(() => {
    let disposed = false;
    let controller: AbortController | null = null;
    let next: ReturnType<typeof setTimeout> | undefined;
    const invalidate = () => setSnapshot((old) => ({ key, source, revision, value: old.key === key && old.source === source ? old.value : null, error: "", fresh: false }));
    invalidate();
    async function poll() {
      if (disposed || !available || !visible || !documentVisible() || controller !== null) return;
      const current = new AbortController();
      controller = current;
      const timeout = setTimeout(() => current.abort(), 30_000);
      try {
        const value = await reader.current!(current.signal);
        if (current.signal.aborted) throw new Error("The release read timed out. Try again.");
        if (!disposed) setSnapshot({ key, source, revision, value, error: "", fresh: true });
      } catch (error) {
        if (!disposed) setSnapshot((old) => ({ key, source, revision, value: old.key === key && old.source === source ? old.value : null, error: current.signal.aborted ? "The release read timed out. Try again." : error instanceof Error ? error.message : String(error), fresh: false }));
      } finally {
        clearTimeout(timeout);
        controller = null;
        if (!disposed && available && visible && documentVisible() && pollInterval > 0) next = setTimeout(() => void poll(), pollInterval);
      }
    }
    const onVisibility = () => { clearTimeout(next); invalidate(); if (documentVisible()) void poll(); };
    document.addEventListener("visibilitychange", onVisibility);
    void poll();
    return () => { disposed = true; clearTimeout(next); controller?.abort(); document.removeEventListener("visibilitychange", onVisibility); };
  }, [key, available, visible, revision, source, pollInterval]);
  const same = snapshot.key === key && snapshot.source === source;
  return { value: same ? snapshot.value : null, error: same ? snapshot.error : "", fresh: available && visible && documentVisible() && same && snapshot.revision === revision && snapshot.fresh, retry };
}
