import { ArrowUpRight } from "lucide-react";
import type { AskTurn } from "./conversationSession";

/** A quiet receipt beneath the model's acknowledgment. The entire button opens
 * the real run; queue state comes from the runtime, never from generated prose. */
export function AskWorkLink({ title, state, onOpen }: { title: string; state: AskTurn["state"]; onOpen: () => void }) {
  const label = state === "waiting" ? "Needs your input" : "View work";
  return <div className="os-ask-work-link"><button type="button" className="os-icon-button" title={label} aria-label={`${label}: ${title} — open in Nexus`} onClick={onOpen}>
    <ArrowUpRight size={14} aria-hidden />
  </button></div>;
}
