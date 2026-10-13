import { useState } from "react";
import { ArrowUpRight, ChevronDown, ChevronUp, Layers } from "lucide-react";
import { AskWait } from "./AskWait";
import { canGuideTurn, isBackgroundTurn, type AskTurn } from "./conversationSession";

export function AskWorkQueue({ turns, onOpen, onGuide, guideDisabled = false }: { turns: AskTurn[]; onOpen?: (runId?: string) => void; onGuide?: (turnId: string) => void; guideDisabled?: boolean }) {
  const [expanded, setExpanded] = useState(false);
  const pending = turns.filter(isBackgroundTurn);
  const expandable = pending.length > 3 && !!onGuide && pending.some(canGuideTurn);
  if (!pending.length) return null;
  return <div className="os-ask-work-queue" aria-label="Background work">
    {expandable ? <div className="os-ask-work-summary"><button type="button" className="os-ask-retry" aria-expanded={expanded} onClick={() => setExpanded(value => !value)}><Layers size={16} aria-hidden />{pending.length} tasks in progress{expanded ? <ChevronUp size={14} aria-hidden /> : <ChevronDown size={14} aria-hidden />}</button>{onOpen ? <button type="button" className="os-ask-retry" onClick={() => onOpen()}>Open Nexus</button> : null}</div> : null}
    {pending.length > 3 && !expandable ? <button type="button" className="os-ask-work-card" onClick={() => onOpen?.()}>
      <Layers size={16} aria-hidden /><span>{pending.length} tasks in progress</span><ArrowUpRight size={14} aria-hidden />
    </button> : (!expandable || expanded) ? pending.map(turn => <div key={turn.id} className="os-ask-work-row"><button type="button" className="os-ask-work-card" onClick={() => onOpen?.(turn.runId)}>
      <span className="os-ask-work-body"><strong>{turn.workTitle || turn.prompt}</strong>
        {turn.state === "waiting" ? <span className="os-caption">Needs your input</span> : <AskWait activity={turn.activity} startedAt={turn.startedAt} hasText={false} label={turn.workStatus === "compiling" ? "Preparing" : turn.workStatus === "waiting" ? "Recovering" : turn.workStatus === "running" ? "Working" : turn.state === "interrupted" ? "Reconnecting" : "Queued"} />}
      </span><ArrowUpRight size={14} aria-hidden />
    </button>{onGuide && canGuideTurn(turn) ? <button type="button" className="os-ask-retry os-ask-work-guide" aria-label={`Guide this work: ${turn.workTitle || turn.prompt}`} disabled={guideDisabled} onClick={() => onGuide(turn.id)}>Guide this work</button> : null}</div>) : null}
  </div>;
}
