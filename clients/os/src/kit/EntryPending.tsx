import { useEffect, useState } from "react";
import { Mark } from "../chrome/Mark";
import { EntryLayout } from "./EntryLayout";

/** The destination waits only for this bounded visual exit, never for a
 * network read or animation event. A refused read still reaches its error
 * view, and reduced motion reveals it immediately. */
export function useEntryTransition(pending: boolean) {
  const [held, setHeld] = useState(pending);
  const [reduced, setReduced] = useState(() => window.matchMedia("(prefers-reduced-motion: reduce)").matches);
  useEffect(() => {
    const query = window.matchMedia("(prefers-reduced-motion: reduce)");
    const changed = () => setReduced(query.matches);
    query.addEventListener("change", changed);
    return () => query.removeEventListener("change", changed);
  }, []);
  useEffect(() => {
    if (pending) { setHeld(true); return; }
    if (reduced) { setHeld(false); return; }
    if (!held) return;
    const timer = setTimeout(() => setHeld(false), 160);
    return () => clearTimeout(timer);
  }, [pending, reduced, held]);
  return { show: pending || (held && !reduced), leaving: !pending };
}

/** One quiet handoff while identity and the initial cluster destination resolve. */
export function EntryPending({ label = "Opening MemQL OS", leaving = false }: { label?: string; leaving?: boolean }) {
  return <EntryLayout><main className="os-entry-pending" data-leaving={leaving || undefined} role="status" aria-busy="true">
    <div className="os-entry-pending-content">
      <span className="os-entry-pending-mark" aria-hidden="true"><Mark size={36} /></span>
      <p className="os-entry-pending-label">{label}</p>
    </div>
  </main></EntryLayout>;
}
