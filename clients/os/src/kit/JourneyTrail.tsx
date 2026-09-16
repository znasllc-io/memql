import { useEffect, useRef } from "react";
import { Check } from "lucide-react";

export interface JourneyStep {
  id: string;
  label: string;
  state: string;
  available: boolean;
}

/** Shared wizard navigation. The caller supplies server-derived completion;
 * selecting a step only changes which instructions are visible. */
export function JourneyTrail({ label, steps, selected, onSelect }: {
  label: string; steps: readonly JourneyStep[]; selected: string; onSelect: (id: string) => void;
}) {
  const trail = useRef<HTMLOListElement>(null);
  useEffect(() => {
    trail.current?.querySelector<HTMLElement>('[aria-current="step"]')?.scrollIntoView?.({ block: "nearest", inline: "nearest" });
  }, [selected]);
  return <ol ref={trail} className="os-journey-trail" aria-label={label}>
    {steps.map((step, i) => {
      const content = <><span className="os-step-number">{step.state === "done" || step.state === "complete" ? <Check size={13} aria-hidden /> : i + 1}</span><span>{step.label}</span></>;
      return <li key={step.id} data-state={step.state}>{step.available ?
        <button type="button" aria-current={step.id === selected ? "step" : undefined} onClick={() => onSelect(step.id)}>{content}</button> :
        <span className="os-step-pending">{content}</span>}</li>;
    })}
  </ol>;
}
