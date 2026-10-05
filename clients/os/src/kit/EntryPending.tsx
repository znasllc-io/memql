import { Mark } from "../chrome/Mark";
import { EntryLayout } from "./EntryLayout";

/** One quiet handoff while identity and the initial cluster destination resolve. */
export function EntryPending({ label = "Opening MemQL OS" }: { label?: string }) {
  return <EntryLayout><main className="os-entry-pending" role="status" aria-busy="true">
    <span className="os-sr-only">{label}</span>
    <span className="os-entry-pending-mark" aria-hidden="true"><Mark size={36} /></span>
  </main></EntryLayout>;
}
