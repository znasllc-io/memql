import { useState } from "react";
import { Check, ChevronRight, CircleHelp, Pause, X } from "lucide-react";
import { Caption } from "../../kit";
import { stepOutcome, stepReason, stepTitle } from "../../nexus/presentation";
import type { StepRow } from "./rows";
import { stepStatusWord } from "./words";

/** A readable account of the observed work. The dependency map remains separate evidence. */
export function GoalJourney({ steps, selected, onSelect, completed, total, stale = false }: {
  steps: readonly StepRow[]; selected: string; onSelect: (key: string) => void;
  completed: number; total: number; stale?: boolean;
}) {
  const [expanded, setExpanded] = useState(false);
  let finishedPrefix = 0;
  while (finishedPrefix < steps.length && ["done", "skipped"].includes(steps[finishedPrefix]!.status)) finishedPrefix++;
  const foldable = finishedPrefix > 8 ? finishedPrefix - 3 : 0;
  const selectedIndex = steps.findIndex(step => step.key === selected);
  const folded = expanded || selectedIndex >= 0 && selectedIndex < foldable ? 0 : foldable;
  return <section className="nexus-journey" aria-label="Goal journey" data-stale={stale || undefined}>
    <p className="nexus-journey-progress">{total === 0 ? "The work is taking shape" : `${completed} of ${total} steps complete`}</p>
    {stale ? <Caption>Showing the last activity received. Reconnecting will refresh this journey.</Caption> : null}
    {foldable > 0 ? <button className="os-link nexus-journey-fold" type="button" aria-expanded={!folded} onClick={() => {
      if (!folded && selectedIndex >= 0 && selectedIndex < foldable) onSelect("");
      setExpanded(Boolean(folded));
    }}>{folded ? `Show ${folded} earlier completed steps` : "Collapse earlier completed steps"}</button> : null}
    <ol className="nexus-journey-steps" start={folded + 1}>{steps.slice(folded).map((step, index) => {
      const title = stepTitle(step), reason = stepReason(step);
      const status = stepStatusWord(step.status);
      const dependencies = step.dependsOn.map(key => steps.find(row => row.key === key)).filter((row): row is StepRow => row !== undefined);
      return <li key={step.id || step.key} data-status={step.status}>
        <button type="button" className="nexus-journey-action" aria-label={`${title}, ${status}`} aria-expanded={selected === step.key} data-selected={selected === step.key || undefined} onClick={() => onSelect(selected === step.key ? "" : step.key)}>
          <span className="nexus-journey-marker" aria-hidden>{step.status === "done" ? <Check size={18} /> : step.status === "failed" ? <X size={18} /> : step.status === "waiting" ? <Pause size={16} /> : <span>{folded + index + 1}</span>}</span>
          <span className="nexus-journey-copy"><span className="nexus-journey-heading"><strong>{title}</strong><span className="nexus-journey-state">{status}</span></span>
            <span className="nexus-journey-outcome">{stepOutcome(step)}</span>
            {dependencies.length > 1 ? <span className="nexus-journey-context">After {dependencies.map(stepTitle).join(" and ")}</span> : null}
            {reason ? <span className="nexus-journey-context"><CircleHelp size={13} aria-hidden /> Recorded reason available</span> : null}
          </span><ChevronRight className="nexus-journey-open" size={16} aria-hidden />
        </button>
      </li>;
    })}</ol>
  </section>;
}
