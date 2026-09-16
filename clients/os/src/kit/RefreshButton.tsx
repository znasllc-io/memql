import { RefreshCw } from "lucide-react";

/** One read/reconnect action: stable name, target size and pending state. */
export function RefreshButton({ label, onClick, busy = false }: { label: string; onClick: () => void; busy?: boolean }) {
  return <button type="button" className="os-icon-button os-refresh" aria-label={label} title={label} aria-busy={busy || undefined} disabled={busy} onClick={onClick}><RefreshCw size={16} aria-hidden /></button>;
}
