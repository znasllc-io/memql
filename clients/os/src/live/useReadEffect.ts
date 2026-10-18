import { useEffect, useLayoutEffect, useRef, type DependencyList } from "react";

/** Read one identity at a time. Invalidation during a read remembers one
 * follow-up; it never cancels useful work and starts another copy. Identity
 * changes and unmount DO cancel. Callers must guard state writes with the
 * signal and handle failures in the surface's own language. */
export function useReadEffect(
  read: (signal: AbortSignal) => Promise<void> | void,
  identity: DependencyList,
  revision: unknown,
): void {
  const latest = useRef(read);
  useLayoutEffect(() => { latest.current = read; });
  const current = useRef<{ revision: unknown; request: () => void } | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    let running = false;
    let dirty = false;
    const request = () => {
      if (controller.signal.aborted) return;
      if (running) { dirty = true; return; }
      running = true;
      const settled = () => {
        running = false;
        if (dirty && !controller.signal.aborted) {
          dirty = false;
          request();
        }
      };
      // Invoke immediately, like useEffect, but normalize synchronous exits.
      void Promise.resolve(latest.current(controller.signal)).then(settled, settled);
    };
    const generation = { revision, request };
    current.current = generation;
    request();
    return () => {
      controller.abort();
      if (current.current === generation) current.current = null;
    };
    // Identity is supplied explicitly, like useEffect's dependency list.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, identity);

  useEffect(() => {
    const generation = current.current;
    if (generation && !Object.is(generation.revision, revision)) {
      generation.revision = revision;
      generation.request();
    }
  }, [revision]);
}
