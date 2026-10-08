/** Coalesce overlapping reads without losing a refresh requested mid-flight.
 * The current predicate lets a reader discard a response superseded by a new
 * user action, including a terminal response that would otherwise stop polling.
 */
export function refreshQueue(work: (current: () => boolean) => Promise<void>): () => Promise<void> {
  let needed = false;
  let pending: Promise<void> | undefined;
  const refresh = (): Promise<void> => {
    needed = true;
    if (!pending) {
      pending = Promise.resolve().then(async () => {
        while (needed) {
          needed = false;
          try { await work(() => !needed); }
          catch (error) { if (!needed) throw error; }
        }
      }).finally(() => {
        pending = undefined;
        // A caller can arrive after the loop returns but before this finalizer.
        if (needed) return refresh();
      });
    }
    return pending;
  };
  return refresh;
}
