import { ArrowUpRight } from "lucide-react";
import type { AskTurn } from "./conversationSession";

/** A quiet receipt beneath the model's acknowledgment. The entire row opens
 * the real run; queue state comes from the runtime, never from generated prose. */
export function AskWorkLink({ title, state, onOpen }: { title: string; state: AskTurn["state"]; onOpen: () => void }) {
  const label = state === "waiting" ? "Needs your input" : state === "queued" || state === "streaming" ? "Queued work" : "View work";
  return <button type="button" className="os-ask-work-link" aria-label={`${label}: ${title} — open in Nexus`} onClick={onOpen}>
    <span>{label}</span><ArrowUpRight size={14} aria-hidden />
  </button>;
}
