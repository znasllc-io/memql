import { ArrowUpRight, Layers } from "lucide-react";
import { Button, Caption } from "../kit";
import { InPlaceAsk } from "../kit/pressHold";
import { AskWait } from "./AskWait";
import { isBackgroundTurn, isConversationalWait, pendingStops, stopKey, type AskTurn } from "./conversationSession";

type StopDecision = "stop" | "keep";

// A card holds two things side by side, never one inside the other: the
// control that opens the work, and -- when a message asked to drop that work --
// the question that stops it (design 2026-10-19, Interface). Only the question
// is held after it appears (InPlaceAsk, DESIGN.md rule 12): the wait above it
// ticks every 250 ms, and a hold that watched it would never let go.
//
// Four or more cards fold into one "N tasks in progress" card, except a card
// that is asking: a question folded away cannot be answered. For the same
// reason, work that would otherwise wait inline gets its card while asked about.
export function AskWorkQueue({ turns, onOpen, deciding, onDecideStop }: {
  turns: AskTurn[];
  onOpen?: (runId?: string) => void;
  /** Stop questions being answered now, by `stopKey`. */
  deciding?: Readonly<Record<string, StopDecision>>;
  /** Absent when this connection cannot answer a stop question; none is offered then. */
  onDecideStop?: (turnId: string, runId: string, decision: StopDecision) => void;
}) {
  const stops = pendingStops(onDecideStop ? turns : []);
  const asking = (turn: AskTurn) => Boolean(turn.runId && stops.has(turn.runId));
  const pending = turns.filter(turn => isBackgroundTurn(turn) && (!isConversationalWait(turn) || asking(turn)));
  if (!pending.length) return null;
  const fold = pending.length > 3;
  const folded = fold ? pending.filter(turn => !asking(turn)) : [];
  const card = (turn: AskTurn) => {
    const title = turn.workTitle || turn.prompt;
    const stop = turn.runId ? stops.get(turn.runId) : undefined;
    const decision = stop ? deciding?.[stopKey(stop.turnId, turn.runId!)] : undefined;
    return <div key={turn.id} className="os-ask-work-card">
      <button type="button" className="os-ask-work-open" onClick={() => onOpen?.(turn.runId)}>
        <span className="os-ask-work-body"><strong>{title}</strong>
          {turn.state === "waiting" ? <span className="os-caption">Needs your input</span> : <AskWait activity={turn.activity} startedAt={turn.startedAt} hasText={false} label={turn.workStatus === "compiling" ? "Preparing" : turn.workStatus === "waiting" ? "Recovering" : turn.workStatus === "running" ? "Working" : turn.state === "interrupted" ? "Reconnecting" : "Queued"} />}
        </span><ArrowUpRight size={14} aria-hidden />
      </button>
      {stop && onDecideStop ? <InPlaceAsk className="os-ask-work-stop" role="group" aria-label={`Stop ${title}`}>
        <Caption>Stop this work? It can't be resumed.</Caption>
        <span className="os-ask-work-stop-acts">
          <Button disabled={Boolean(decision)} busy={decision === "keep"} onClick={() => onDecideStop(stop.turnId, turn.runId!, "keep")}>Keep</Button>
          <Button tone="danger" disabled={Boolean(decision)} busy={decision === "stop"} onClick={() => onDecideStop(stop.turnId, turn.runId!, "stop")}>Stop</Button>
        </span>
      </InPlaceAsk> : null}
    </div>;
  };
  return <div className="os-ask-work-queue" aria-label="Background work">
    {(fold ? pending.filter(asking) : pending).map(card)}
    {folded.length ? <div className="os-ask-work-card"><button type="button" className="os-ask-work-open" onClick={() => onOpen?.()}>
      <Layers size={16} aria-hidden /><span>{folded.length} {folded.length === 1 ? "task" : "tasks"} in progress</span><ArrowUpRight size={14} aria-hidden />
    </button></div> : null}
  </div>;
}
