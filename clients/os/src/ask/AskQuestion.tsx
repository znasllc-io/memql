import { useState, type FormEvent } from "react";
import { ArrowUp } from "lucide-react";
import type { AskQuestion as Question } from "./conversationSession";

/** A temporary question within the original exchange. Suggested answers never
 * exclude the person's own words; submitting resumes the existing work. */
export function AskQuestion({ question, onAnswer }: { question: Question; onAnswer: (answer: Record<string, unknown>) => Promise<void> }) {
  const [text, setText] = useState("");
  const [selected, setSelected] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function submit(event: FormEvent) {
    event.preventDefault();
    if (busy || (!text.trim() && !selected.length)) return;
    const answer = text.trim() ? { text: text.trim() } : question.kind === "multi" ? { values: selected } : { value: selected[0] };
    setBusy(true); setError("");
    try { await onAnswer(answer); }
    catch (error) { setError(error instanceof Error ? error.message : String(error)); }
    finally { setBusy(false); }
  }
  return <form className="os-ask-question" onSubmit={submit} aria-label="Question from MemQL">
    <p>{question.text}</p>
    {question.options.length ? <div className="os-ask-question-options" role="group" aria-label="Suggested answers">{question.options.map(option => <button key={option.value} type="button" aria-pressed={selected.includes(option.value)} disabled={busy} onClick={() => {
      setText(""); setSelected(question.kind === "multi" ? selected.includes(option.value) ? selected.filter(value => value !== option.value) : [...selected, option.value] : [option.value]);
    }}>{option.label}</button>)}</div> : null}
    <div className="os-ask-question-input"><textarea aria-label="Your answer" placeholder="Your answer" maxLength={8000} rows={2} value={text} disabled={busy} onChange={event => { setText(event.target.value); setSelected([]); }} />
      <button type="submit" className="os-icon-button" title="Send answer" aria-label={busy ? "Sending answer" : "Send answer"} disabled={busy || (!text.trim() && !selected.length)}><ArrowUp size={16} /></button></div>
    {error ? <p role="alert" className="os-ask-error">{error}</p> : null}
  </form>;
}
