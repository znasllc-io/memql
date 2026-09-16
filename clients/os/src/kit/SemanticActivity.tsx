import { createContext, useContext, type ReactNode } from "react";
import { Check, CircleDashed, OctagonAlert, Sparkles } from "lucide-react";

/** Only an actual orchestrator may supply activity. No timers infer intent,
 * no cursor movement, focus transfer, or selection changes happen here. */
export interface SemanticActivity {
  id: string;
  target: string;
  phase: "proposed" | "running" | "completed" | "failed";
  label: string;
}
const ActivityContext = createContext<readonly SemanticActivity[]>([]);
export const SemanticActivityProvider = ActivityContext.Provider;
const PHASE_LABELS = { proposed: "AI proposal", running: "AI working", completed: "AI completed", failed: "AI needs attention" };

export function ActivityTarget({ target, children, className = "" }: { target: string; children: ReactNode; className?: string }) {
  const activity = useContext(ActivityContext).find(item => item.target === target);
  const Icon = activity?.phase === "completed" ? Check : activity?.phase === "failed" ? OctagonAlert : activity?.phase === "proposed" ? CircleDashed : Sparkles;
  return <div className={`os-activity-target ${className}`} data-activity={activity?.phase} data-semantic-target={target}>
    {activity ? <div key={activity.id} className="os-activity-label" role="status"><Icon size={13} aria-hidden /><span>{PHASE_LABELS[activity.phase]}: {activity.label}</span></div> : null}
    {children}
  </div>;
}
