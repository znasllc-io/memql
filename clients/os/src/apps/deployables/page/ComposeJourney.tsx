import { useEffect, useRef, type ReactNode } from "react";
import { Check } from "lucide-react";
import { Caption } from "../../../kit";
import { ActivityTarget } from "../../../kit/SemanticActivity";
import type { StopId } from "../targets";
import { railFor, type ComposeInput, type RailStage } from "./rail";

const LABELS: Record<StopId, string> = { source: "Source", whatItIs: "Review", whereItLives: "Address", build: "Build", live: "Live" };

/** UI navigation only. Completion, failures and availability still come from
 * the pipeline's rail reading, never from incrementing a local step counter. */
export function ComposeJourney({ input, selected, onSelect, stopBody, awaitingLive = false }: {
  awaitingLive?: boolean;
  input: ComposeInput; selected: StopId; onSelect: (id: StopId) => void; stopBody: (stage: RailStage) => ReactNode;
}) {
  const trail = useRef<HTMLOListElement>(null);
  useEffect(() => {
    const active = trail.current?.querySelector<HTMLElement>('[aria-current="step"]');
    active?.scrollIntoView?.({ block: "nearest", inline: "nearest" });
  }, [selected]);
  const stages = railFor(input).stages.map(stage => stage.id === "live" && awaitingLive ? { ...stage, state: "open" as const } : stage);
  const current = stages.find(s => s.id === selected) ?? stages[0];
  return <>
    {selected !== "source" && input.answers?.source ? <Caption>{input.answers.source}</Caption> : null}
    <ol ref={trail} className="deployable-journey-trail" aria-label="Deployable setup progress">
      {stages.map((stage, i) => {
        const id = stage.id as StopId;
        const available = stage.state !== "pending" && stage.state !== "ahead" || id === selected;
        const label = <><span className="deployable-step-number">{(stage.state === "done" || stage.state === "complete") ? <Check size={13} aria-hidden /> : i + 1}</span><span>{LABELS[id]}</span></>;
        return <li key={id} data-state={stage.state}>{available ? <button type="button" aria-current={id === selected ? "step" : undefined} onClick={() => onSelect(id)}>{label}</button> : <span className="deployable-step-pending">{label}</span>}</li>;
      })}
    </ol>
    {current ? <ActivityTarget target={`deployables:compose:${current.id}`} className="deployable-journey-current">
      <h3>{LABELS[current.id as StopId]}</h3>
      <Caption>{current.reason || current.blurb}</Caption>
      {stopBody(current)}
    </ActivityTarget> : null}
  </>;
}
