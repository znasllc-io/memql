import { useEffect, useRef, useState, type FormEvent } from "react";
import { ArrowUp, Mic } from "lucide-react";

import type { AskHandle, AskTransport } from "./askController";

// THE Ask surface (spec C): one component behind all three entry points.
// Input row (text, mic toggle, send), streamed answer log, context chip.
// Errors render here in the surface's own voice -- never a toast.

interface Exchange {
  id: number;
  prompt: string;
  answer: string;
  state: "streaming" | "done" | "error";
  error?: string;
}

export const ASK_VOICE_SOON = "Voice arrives with the Ask voice epic. Text works now.";

export function AskSurface({
  transport,
  context = null,
  variant,
  autoFocus = false,
}: {
  transport: AskTransport;
  context?: string | null;
  variant: "sheet" | "widget";
  autoFocus?: boolean;
}) {
  const [draft, setDraft] = useState("");
  const [exchanges, setExchanges] = useState<Exchange[]>([]);
  const [micNote, setMicNote] = useState(false);
  const handleRef = useRef<AskHandle | null>(null);
  const nextIdRef = useRef(1);
  const inputRef = useRef<HTMLInputElement | null>(null);
  const logRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (autoFocus) inputRef.current?.focus();
    return () => handleRef.current?.cancel();
  }, [autoFocus]);

  useEffect(() => {
    logRef.current?.scrollTo({ top: logRef.current.scrollHeight });
  }, [exchanges]);

  function send(prompt: string) {
    const id = nextIdRef.current;
    nextIdRef.current += 1;
    setExchanges((prev) => [...prev, { id, prompt, answer: "", state: "streaming" }]);
    const patch = (p: Partial<Exchange>) =>
      setExchanges((prev) => prev.map((e) => (e.id === id ? { ...e, ...p } : e)));
    handleRef.current = transport.ask(prompt, context, {
      delta: (text) => setExchanges((prev) => prev.map((e) => (e.id === id ? { ...e, answer: e.answer + text } : e))),
      done: () => patch({ state: "done" }),
      error: (message) => patch({ state: "error", error: message }),
    });
  }

  function onSubmit(event: FormEvent) {
    event.preventDefault();
    const prompt = draft.trim();
    if (!prompt) return;
    setDraft("");
    send(prompt);
  }

  return (
    <div className="os-ask" data-os-ask={variant}>
      {context ? <span className="os-ask-context">{context}</span> : null}
      <div className="os-ask-log" ref={logRef} aria-live="polite">
        {exchanges.length === 0 ? (
          <p className="os-caption os-ask-hint">
            {variant === "widget" ? "Ask the OS anything." : "Ask about this cluster, an app, or anything on your desk."}
          </p>
        ) : null}
        {exchanges.map((e) => (
          <div key={e.id} className="os-ask-exchange" data-state={e.state}>
            <p className="os-ask-prompt">{e.prompt}</p>
            {e.answer ? <p className="os-ask-answer">{e.answer}</p> : null}
            {e.state === "error" ? (
              <p className="os-ask-error">
                {e.error ?? "Something went wrong."}{" "}
                <button type="button" className="os-link" onClick={() => send(e.prompt)}>
                  Retry
                </button>
              </p>
            ) : null}
          </div>
        ))}
      </div>
      <form className="os-ask-input" onSubmit={onSubmit}>
        <button
          type="button"
          className="os-ask-mic"
          aria-label="Ask by voice"
          aria-pressed={micNote}
          onClick={() => setMicNote((v) => !v)}
        >
          <Mic size={15} aria-hidden />
        </button>
        <input
          ref={inputRef}
          className="os-ask-field"
          placeholder="Ask"
          aria-label="Ask"
          value={draft}
          onChange={(event) => setDraft(event.target.value)}
        />
        <button type="submit" className="os-ask-send" aria-label="Send" disabled={!draft.trim()}>
          <ArrowUp size={15} aria-hidden />
        </button>
      </form>
      {micNote ? <p className="os-caption os-ask-micnote">{ASK_VOICE_SOON}</p> : null}
    </div>
  );
}
