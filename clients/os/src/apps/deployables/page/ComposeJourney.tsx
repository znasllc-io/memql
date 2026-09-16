import { type ReactNode } from "react";
import { JourneyTrail } from "../../../kit/JourneyTrail";
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
  const stages = railFor(input).stages.map(stage => stage.id === "live" && awaitingLive ? { ...stage, state: "open" as const } : stage);
  const current = stages.find(s => s.id === selected) ?? stages[0];
  return <>
    {selected !== "source" && input.answers?.source ? <Caption>{input.answers.source}</Caption> : null}
    <JourneyTrail label="Deployable setup progress" selected={selected} onSelect={id => onSelect(id as StopId)}
      steps={stages.map(stage => ({ ...stage, label: LABELS[stage.id as StopId], available: stage.state !== "pending" && stage.state !== "ahead" || stage.id === selected }))} />
    {current ? <ActivityTarget target={`deployables:compose:${current.id}`} className="deployable-journey-current">
      <h3>{LABELS[current.id as StopId]}</h3>
      <Caption>{current.reason || current.blurb}</Caption>
      {stopBody(current)}
    </ActivityTarget> : null}
  </>;
}
