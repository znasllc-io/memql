import { useState, type FormEvent } from "react";
import { ArrowUp } from "lucide-react";
import type { AskQuestion as Question } from "./conversationSession";

/** A temporary question within the original exchange. Suggested answers never
 * exclude the person's own words; submitting resumes the existing work. */
export function AskQuestion({ question, onAnswer }: { question: Question; onAnswer: (answer: Record<string, unknown>) => Promise<void> }) {
  const resumeLabel = question.resumeAnswer?.value === "abandon" ? "Stop saved work" : "Continue saved work";
  const options = Array.isArray(question.options) ? question.options : [];
  const [text, setText] = useState("");
  const [selected, setSelected] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function submit(event: FormEvent) {
    event.preventDefault();
    if (busy || (!question.resumeAnswer && !text.trim() && !selected.length)) return;
    const answer = question.resumeAnswer ?? (text.trim() ? { text: text.trim() } : question.kind === "multi" ? { values: selected } : { value: selected[0] });
    setBusy(true); setError("");
    try { await onAnswer(answer); }
    catch (error) { setError(error instanceof Error ? error.message : String(error)); }
    finally { setBusy(false); }
  }
  return <form className="os-ask-question" data-ask-question={question.id} onSubmit={submit} aria-label="Question from MemQL">
    <p>{question.text}</p>
    {question.optional ? <p>{question.continued ? "Work has continued. Your answer can still guide it." : "Optional. Work will continue after a short wait."}</p> : null}
    {options.length ? <div className="os-ask-question-options" role="group" aria-label="Suggested answers">{options.map(option => <button key={option.value} type="button" aria-pressed={selected.includes(option.value)} disabled={busy} onClick={() => {
      setText(""); setSelected(question.kind === "multi" ? selected.includes(option.value) ? selected.filter(value => value !== option.value) : [...selected, option.value] : [option.value]);
    }}>{option.label}</button>)}</div> : null}
    <div className="os-ask-question-input">{!question.choiceOnly ? <textarea aria-label="Your answer" placeholder="Your answer" maxLength={8000} rows={2} value={text} disabled={busy} onChange={event => { setText(event.target.value); setSelected([]); }} /> : null}
      <button type="submit" className="os-icon-button" title={question.resumeAnswer ? resumeLabel : "Send answer"} aria-label={busy ? "Sending answer" : question.resumeAnswer ? resumeLabel : "Send answer"} disabled={busy || (!question.resumeAnswer && !text.trim() && !selected.length)}><ArrowUp size={16} /></button></div>
    {error ? <p role="alert" className="os-ask-error">{error}</p> : null}
  </form>;
}
