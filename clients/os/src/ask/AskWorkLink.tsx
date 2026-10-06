import { ArrowUpRight } from "lucide-react";
import { Button } from "../kit/controls";
import type { AskTurn } from "./conversationSession";

/** A quiet receipt beneath the model's acknowledgment. The entire button opens
 * the real run; queue state comes from the runtime, never from generated prose. */
export function AskWorkLink({ title, state, onOpen }: { title: string; state: AskTurn["state"]; onOpen: () => void }) {
  const label = state === "waiting" ? "Needs your input" : "View work";
  return <div className="os-ask-work-link"><Button ariaLabel={`${label}: ${title} — open in Nexus`} onClick={onOpen}>
    <span>{label}</span><ArrowUpRight size={14} aria-hidden />
  </Button></div>;
}
