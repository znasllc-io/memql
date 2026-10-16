import { ArrowUpRight } from "lucide-react";
import type { AskTurn } from "./conversationSession";

/** A quiet inline shortcut at the end of the model's acknowledgment. */
export function AskWorkLink({ title, state, onOpen }: { title: string; state: AskTurn["state"]; onOpen: () => void }) {
  const label = state === "waiting" ? "Needs your input" : "View work";
  return <button type="button" className="os-ask-work-link" title={label} aria-label={`${label}: ${title} — open in Nexus`} onClick={onOpen}>
    <ArrowUpRight size={12} aria-hidden />
  </button>;
}
