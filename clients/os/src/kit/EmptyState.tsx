import type { ReactNode } from "react";
import { Inbox, type LucideIcon } from "lucide-react";

/** A settled empty result, never a substitute for loading or a failed read. */
export function EmptyState({ title, children, action, icon: Icon = Inbox }: { title: string; children: ReactNode; action?: ReactNode; icon?: LucideIcon }) {
  return <section className="os-empty-state" aria-label={title}><Icon size={28} strokeWidth={1.4} aria-hidden /><div><h4>{title}</h4><div className="os-empty-state-copy">{children}</div>{action ? <div className="os-empty-state-action">{action}</div> : null}</div></section>;
}
