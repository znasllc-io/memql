import { ArrowUpRight, Layers } from "lucide-react";
import { AskWait } from "./AskWait";
import { isBackgroundTurn, type AskTurn } from "./conversationSession";

export function AskWorkQueue({ turns, onOpen }: { turns: AskTurn[]; onOpen?: (runId?: string) => void }) {
  const pending = turns.filter(isBackgroundTurn);
  if (!pending.length) return null;
  return <div className="os-ask-work-queue" aria-label="Background work">
    {pending.length > 3 ? <button type="button" className="os-ask-work-card" onClick={() => onOpen?.()}>
      <Layers size={16} aria-hidden /><span>{pending.length} tasks in progress</span><ArrowUpRight size={14} aria-hidden />
    </button> : pending.map(turn => <button type="button" key={turn.id} className="os-ask-work-card" onClick={() => onOpen?.(turn.runId)}>
      <span className="os-ask-work-body"><strong>{turn.workTitle || turn.prompt}</strong>
        {turn.state === "waiting" ? <span className="os-caption">Needs your input</span> : <AskWait activity={turn.activity} startedAt={turn.startedAt} hasText={false} label={turn.workStatus === "compiling" ? "Preparing" : turn.workStatus === "waiting" ? "Recovering" : turn.workStatus === "running" ? "Working" : turn.state === "interrupted" ? "Reconnecting" : "Queued"} />}
      </span><ArrowUpRight size={14} aria-hidden />
    </button>)}
  </div>;
}
